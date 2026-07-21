package live

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/playlist"
)

const (
	bytesPerSecond = 192000 // s16le 48kHz stereo
	chunkBytes     = 48000  // 250ms per feed chunk
	republishEvery = 25 * time.Second
)

type Clock interface {
	Now() time.Time
	Tick(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) Tick(d time.Duration) <-chan time.Time {
	return time.NewTicker(d).C // session-lifetime; GC'd with the session
}
func RealClock() Clock { return realClock{} }

type FeederDeps struct {
	Store     playlist.Store
	Library   library.Library
	Log       AirLog
	Listeners Listeners
	Fetch     func(ctx context.Context, artifactID, dir string) (string, error)
	Decoder   Decoder
	Encoder   Encoder
	Producer  Producer
	Clock     Clock
	Dir       string
	Logger    *slog.Logger
}

type Feeder struct {
	d          FeederDeps
	sessionDir atomic.Value // string
	seq        atomic.Int64 // session counter for dir names
	// anchor is the sample-clock epoch for the CURRENT session only:
	// entry.StartedAt = anchor + samplesFed/48000. Must be captured fresh
	// at the start of each RunSession call, not once at Feeder
	// construction — one Feeder is built at boot and RunSession is called
	// again each time the operator goes back on-air, possibly hours
	// later, so a construction-time anchor would misdate every session
	// after the first. Written only by RunSession's own goroutine and
	// read only within that same call, so it needs no synchronization of
	// its own.
	anchor time.Time
}

func NewFeeder(d FeederDeps) *Feeder {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	f := &Feeder{d: d}
	f.sessionDir.Store("")
	return f
}

func (f *Feeder) SessionDir() string { return f.sessionDir.Load().(string) }

func (f *Feeder) publish(ctx context.Context, topic string, val []byte) {
	if f.d.Producer == nil {
		return
	}
	if err := f.d.Producer.Publish(ctx, topic, val); err != nil {
		f.d.Logger.Error("publish failed", "topic", topic, "err", err)
	}
}

// boundary decides what airs next. skip=true means the chosen item vanished
// from the library — the caller advances prevYTID past it and re-runs the
// boundary. stop=true ends the session (operator off-air or §10).
func (f *Feeder) boundary(ctx context.Context, prevYTID string) (next playlist.Item, track library.Track, skip, stop bool, err error) {
	st, err := f.d.Store.GetStation(ctx)
	if err != nil {
		return playlist.Item{}, library.Track{}, false, false, err
	}
	if !st.OnAir {
		return playlist.Item{}, library.Track{}, false, true, nil // operator stopped us
	}
	if st.ActivePlaylistID == "" {
		return playlist.Item{}, library.Track{}, false, true, f.autoOffAir(ctx) // §10
	}
	_, items, err := f.d.Store.Get(ctx, st.ActivePlaylistID)
	if errors.Is(err, playlist.ErrNotFound) {
		return playlist.Item{}, library.Track{}, false, true, f.autoOffAir(ctx) // §10
	}
	if err != nil {
		return playlist.Item{}, library.Track{}, false, false, err
	}
	if len(items) == 0 {
		return playlist.Item{}, library.Track{}, false, true, f.autoOffAir(ctx) // §10
	}
	// next item after prev, wrapping; unknown prev (playlist swap) → first.
	next = items[0]
	for i, it := range items {
		if it.YTID == prevYTID {
			next = items[(i+1)%len(items)]
			break
		}
	}
	track, found, err := f.d.Library.Get(ctx, next.YTID)
	if err != nil {
		return playlist.Item{}, library.Track{}, false, false, err
	}
	if !found {
		// library row vanished between store read and here — skip it.
		return next, library.Track{}, true, false, nil
	}
	return next, track, false, false, nil
}

// autoOffAir persists off-air (§10 engine-side closure) and reports the
// sentinel; store errors are logged, not fatal — the session ends either way.
func (f *Feeder) autoOffAir(ctx context.Context) error {
	if _, err := f.d.Store.GoOffAir(ctx); err != nil {
		f.d.Logger.Error("auto off-air persist failed", "err", err)
	}
	f.d.Logger.Info("auto off-air: active playlist empty or missing")
	return nil
}

// RunSession broadcasts until off-air or ctx cancellation. resumeOffset
// handling lives in Task 7; this core loop starts every session fresh.
func (f *Feeder) RunSession(ctx context.Context) error {
	// Per-session epoch: captured before any of MkdirAll/Encoder.Start/
	// sessionDir.Store, so callers can synchronize on SessionDir() != ""
	// becoming true and be guaranteed the anchor is already fixed (program
	// order on this goroutine — no separate lock needed for the field).
	f.anchor = f.d.Clock.Now()

	dir := filepath.Join(f.d.Dir, fmt.Sprintf("session-%d", f.seq.Add(1)))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Registered right after MkdirAll succeeds (not after Encoder.Start) so a
	// subsequent encoder-start failure doesn't leak the session dir.
	// RemoveAll is idempotent, so this is the ONLY removal of dir — the
	// on-air-exit defer below no longer calls it separately.
	defer os.RemoveAll(dir)

	sess, err := f.d.Encoder.Start(ctx, dir)
	if err != nil {
		return err
	}
	f.sessionDir.Store(dir)
	defer func() {
		f.sessionDir.Store("")
		sess.Stop()
		f.publish(context.WithoutCancel(ctx), TopicNowPlaying, OffAirPayload())
	}()

	var samplesFed int64 // 4 bytes per stereo sample-frame at s16le
	prevYTID := ""
	tick := f.d.Clock.Tick(250 * time.Millisecond)
	republish := f.d.Clock.Tick(republishEvery)

	for {
		next, track, skip, stop, err := f.boundary(ctx, prevYTID)
		if stop || ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		if skip { // vanished from library: advance past it, no publish
			prevYTID = next.YTID
			continue
		}

		// Open the artifact + decoder BEFORE announcing anything: a track
		// that fails to fetch/decode must never be logged or published as
		// now-playing — it never aired, and doing so would poison the
		// sample-clock resume anchor with a track nobody heard.
		rd, cleanup, openSkip, err := f.openTrack(ctx, track)
		if err != nil {
			return err
		}
		if openSkip {
			prevYTID = track.YTID // advance past the track that failed to open
			continue
		}

		// Overflow-safe: samplesFed can exceed ~9.2e9 (≈53h continuous at
		// 48kHz) before time.Duration(samplesFed)*time.Second would overflow
		// int64 nanoseconds. Split into whole seconds + sub-second remainder.
		startedAt := f.anchor.Add(time.Duration(samplesFed/48000)*time.Second + time.Duration(samplesFed%48000)*time.Second/48000)
		entry := Entry{YTID: track.YTID, Title: track.Title, Artist: track.Channel,
			StartedAt: startedAt, DurationS: int(track.DurationS)}
		if err := f.d.Log.Append(ctx, entry); err != nil {
			f.d.Logger.Error("air log append failed", "err", err)
		}
		n, _ := f.d.Listeners.Count(ctx)
		f.publish(ctx, TopicNowPlaying, NowPlayingPayload(entry, n))
		// queue: re-read items at publish time for freshness
		if st, err := f.d.Store.GetStation(ctx); err == nil && st.ActivePlaylistID != "" {
			if _, items, err := f.d.Store.Get(ctx, st.ActivePlaylistID); err == nil {
				f.publish(ctx, TopicQueue, QueuePayload(items, track.YTID))
			}
		}

		stopTrack, err := f.airTrack(ctx, sess, rd, &samplesFed, tick, republish, entry)
		cleanup()
		if err != nil {
			return err
		}
		prevYTID = track.YTID
		if stopTrack {
			return nil
		}
	}
}

// openTrack fetches the artifact and opens the decoder for track, BEFORE any
// air-log entry or publish happens for it. skip=true (err=nil) means
// fetch/decode failed for THIS track specifically — already logged; the
// caller advances past it and re-runs the boundary without announcing
// anything. err != nil is a fatal, session-ending error (e.g. can't even
// create the fetch scratch dir). On success, cleanup must be called exactly
// once, after the caller is done reading from rd.
func (f *Feeder) openTrack(ctx context.Context, track library.Track) (rd io.ReadCloser, cleanup func(), skip bool, err error) {
	tmp, err := os.MkdirTemp(f.d.Dir, "fetch-*")
	if err != nil {
		return nil, nil, false, err
	}
	path, ferr := f.d.Fetch(ctx, track.ArtifactID, tmp)
	if ferr != nil {
		_ = os.RemoveAll(tmp)
		f.d.Logger.Error("artifact fetch failed; skipping track", "yt_id", track.YTID, "err", ferr)
		return nil, nil, true, nil
	}
	rd, derr := f.d.Decoder.Open(ctx, path, Loudness{I: track.InputI, TP: track.InputTP, LRA: track.InputLRA}, 0)
	if derr != nil {
		_ = os.RemoveAll(tmp)
		f.d.Logger.Error("decoder open failed; skipping track", "yt_id", track.YTID, "err", derr)
		return nil, nil, true, nil
	}
	return rd, func() { _ = rd.Close(); _ = os.RemoveAll(tmp) }, false, nil
}

// airTrack feeds one track's already-open PCM reader, paced one chunk per
// clock tick. Returns stop=true when the session must end (off-air observed
// within one chunk, or ctx done). Off-air is checked on every tick right
// after writing that tick's chunk, so it takes effect within ~250ms rather
// than waiting for the track to finish.
func (f *Feeder) airTrack(ctx context.Context, sess Session, rd io.Reader, samplesFed *int64, tick, republish <-chan time.Time, entry Entry) (bool, error) {
	buf := make([]byte, chunkBytes)
	for {
		select {
		case <-ctx.Done():
			return true, nil
		case err := <-sess.Done():
			return false, fmt.Errorf("encoder exited: %w", err) // Task 7 turns this into resume
		case <-republish:
			n, _ := f.d.Listeners.Count(ctx)
			f.publish(ctx, TopicNowPlaying, NowPlayingPayload(entry, n))
		case <-tick:
			// one paced chunk per tick
			n, rerr := io.ReadFull(rd, buf)
			if n > 0 {
				if _, werr := sess.Stdin().Write(buf[:n]); werr != nil {
					return false, fmt.Errorf("encoder write: %w", werr)
				}
				*samplesFed += int64(n / 4) // 4 bytes per stereo frame
			}
			// Off-air check on EVERY tick (not just at track end): the
			// operator's flip must take effect within ~one chunk, not
			// after the whole track finishes.
			if st, serr := f.d.Store.GetStation(ctx); serr == nil && !st.OnAir {
				return true, nil
			}
			if rerr != nil { // EOF/short read = track finished
				return false, nil
			}
		}
	}
}
