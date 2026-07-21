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

// bootResumeEntry is a boot-resume candidate found by findBootResume: the
// air log's latest entry, still mid-flight and still scheduled in the
// active playlist, that a fresh RunSession should air first (at its
// already-elapsed offset) instead of starting fresh at the top of rotation.
type bootResumeEntry struct {
	track   library.Track
	entry   Entry
	offsetS float64
}

// findBootResume decides whether this session should resume an in-flight
// track instead of starting fresh. It deliberately uses real wall-clock
// time (not f.d.Clock): this is a question about how much actual time has
// passed since the process last wrote an air-log entry (e.g. across a
// restart), independent of the injected Clock that only paces this
// session's own encoder ticks. Every failure mode here (log/store/library
// errors, no active playlist, the track no longer scheduled, or an expired
// entry) is treated as "no resume" rather than fatal — RunSession's first
// real boundary() call right after this hits the same store/library and
// will surface any persistent problem through its own (fatal) error path.
func (f *Feeder) findBootResume(ctx context.Context) *bootResumeEntry {
	entry, found, err := f.d.Log.Latest(ctx)
	if err != nil {
		f.d.Logger.Error("boot resume: air log read failed", "err", err)
		return nil
	}
	if !found {
		return nil
	}
	offsetS, expired := ResumeOffset(entry, time.Now())
	if expired {
		return nil
	}
	st, err := f.d.Store.GetStation(ctx)
	if err != nil {
		f.d.Logger.Error("boot resume: station read failed", "err", err)
		return nil
	}
	if st.ActivePlaylistID == "" {
		return nil
	}
	_, items, err := f.d.Store.Get(ctx, st.ActivePlaylistID)
	if err != nil {
		if !errors.Is(err, playlist.ErrNotFound) {
			f.d.Logger.Error("boot resume: playlist read failed", "err", err)
		}
		return nil
	}
	inPlaylist := false
	for _, it := range items {
		if it.YTID == entry.YTID {
			inPlaylist = true
			break
		}
	}
	if !inPlaylist {
		return nil
	}
	track, ok, err := f.d.Library.Get(ctx, entry.YTID)
	if err != nil {
		f.d.Logger.Error("boot resume: library read failed", "err", err)
		return nil
	}
	if !ok {
		return nil
	}
	return &bootResumeEntry{track: track, entry: entry, offsetS: offsetS}
}

// RunSession broadcasts until off-air or ctx cancellation. An encoder crash
// (Session.Done() delivering a non-nil error) does NOT end the session: a
// new session dir + encoder is started in place and the current track is
// resumed at its aired offset — see the crash-resume block below.
func (f *Feeder) RunSession(ctx context.Context) error {
	// Per-session epoch: captured before any of MkdirAll/Encoder.Start/
	// sessionDir.Store, so callers can synchronize on SessionDir() != ""
	// becoming true and be guaranteed the anchor is already fixed (program
	// order on this goroutine — no separate lock needed for the field).
	// findBootResume below may still adjust it (to the resumed entry's real
	// StartedAt), but that happens before any of the dir/encoder setup, so
	// the invariant holds for anyone observing SessionDir() != "".
	f.anchor = f.d.Clock.Now()

	var samplesFed int64 // 4 bytes per stereo sample-frame at s16le
	resume := f.findBootResume(ctx)
	if resume != nil {
		f.anchor = resume.entry.StartedAt
		samplesFed = int64(resume.offsetS * 48000)
	}

	dir := filepath.Join(f.d.Dir, fmt.Sprintf("session-%d", f.seq.Add(1)))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Captured by closure (not by value) because a crash-resume mid-session
	// swaps `dir` to a fresh session directory; this defer must clean up
	// whichever dir is current when RunSession returns, not the original
	// one. RemoveAll is idempotent, and every crash-resume swap already
	// removes the OLD dir itself (see below), so this defer only ever has
	// the final dir left to reclaim.
	defer func() { os.RemoveAll(dir) }()

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

	prevYTID := ""
	if resume != nil {
		prevYTID = resume.entry.YTID
	}
	tick := f.d.Clock.Tick(250 * time.Millisecond)
	republish := f.d.Clock.Tick(republishEvery)

	for {
		var track library.Track
		var entry Entry
		var offsetS float64
		resumed := resume != nil
		if resumed {
			track, entry, offsetS = resume.track, resume.entry, resume.offsetS
			resume = nil
		} else {
			next, tr, skip, stop, err := f.boundary(ctx, prevYTID)
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
			track = tr
		}

		// Open the artifact + decoder BEFORE announcing anything: a track
		// that fails to fetch/decode must never be logged or published as
		// now-playing — it never aired, and doing so would poison the
		// sample-clock resume anchor with a track nobody heard.
		rd, cleanup, openSkip, err := f.openTrack(ctx, track, offsetS)
		if err != nil {
			return err
		}
		if openSkip {
			prevYTID = track.YTID // advance past the track that failed to open
			continue
		}

		// trackStartSamples anchors this track's aired offset: on a crash
		// mid-track, offset = (samplesFed at crash - trackStartSamples)/48000.
		// It is fixed once per track (not per crash-restart), so repeated
		// crashes on the same track keep computing offset from the track's
		// true start, not from the previous restart point. For a
		// boot-resumed track, samplesFed already starts pre-loaded at the
		// resumed offset (see above), and the track's true start is 0
		// frames from f.anchor (== the entry's original StartedAt) — not
		// the current samplesFed value.
		var trackStartSamples int64
		if resumed {
			trackStartSamples = 0
		} else {
			// Overflow-safe: samplesFed can exceed ~9.2e9 (≈53h continuous at
			// 48kHz) before time.Duration(samplesFed)*time.Second would overflow
			// int64 nanoseconds. Split into whole seconds + sub-second remainder.
			startedAt := f.anchor.Add(time.Duration(samplesFed/48000)*time.Second + time.Duration(samplesFed%48000)*time.Second/48000)
			entry = Entry{YTID: track.YTID, Title: track.Title, Artist: track.Channel,
				StartedAt: startedAt, DurationS: int(track.DurationS)}
			if err := f.d.Log.Append(ctx, entry); err != nil {
				f.d.Logger.Error("air log append failed", "err", err)
			}
			trackStartSamples = samplesFed
		}
		n, _ := f.d.Listeners.Count(ctx)
		f.publish(ctx, TopicNowPlaying, NowPlayingPayload(entry, n))
		// queue: re-read items at publish time for freshness
		if st, err := f.d.Store.GetStation(ctx); err == nil && st.ActivePlaylistID != "" {
			if _, items, err := f.d.Store.Get(ctx, st.ActivePlaylistID); err == nil {
				f.publish(ctx, TopicQueue, QueuePayload(items, track.YTID))
			}
		}

		crashRestarts := 0
		stopSession := false
	feedTrack:
		for {
			stopTrack, crashed, aerr := f.airTrack(ctx, sess, rd, &samplesFed, tick, republish, entry)
			if !crashed {
				cleanup()
				if aerr != nil {
					return aerr
				}
				stopSession = stopTrack
				break
			}

			// Encoder crashed: the reader tied to the dead session is done;
			// a fresh encoder session is required before we can do anything
			// else — including giving up on this track, since the NEXT
			// track also needs a live sess.
			// The crashed Session is deliberately not Stop()'d — the encoder process
			// already exited, and os/exec's Wait (running in FFEncoder's goroutine)
			// closes the parent's pipe FDs, so there is no leak.
			cleanup()
			crashRestarts++
			newDir, newSess, rerr := f.restartSession(ctx, dir)
			if rerr != nil {
				return rerr
			}
			dir, sess = newDir, newSess

			if crashRestarts > 3 {
				f.d.Logger.Error("crash-resume attempts exhausted; skipping track",
					"yt_id", track.YTID, "restarts", crashRestarts-1)
				break feedTrack
			}

			offsetS := float64(samplesFed-trackStartSamples) / 48000
			// Assignment, not `:=` — rd/cleanup/openSkip are the SAME
			// variables read at the top of this loop and at the end of the
			// outer per-track block; `:=` here would shadow them inside
			// just this iteration's block and the reopened reader would
			// never actually be fed (classic for-loop redeclaration trap).
			var oerr error
			rd, cleanup, openSkip, oerr = f.openTrack(ctx, track, offsetS)
			if oerr != nil {
				return oerr
			}
			if openSkip {
				f.d.Logger.Error("resume reopen failed; skipping track", "yt_id", track.YTID)
				break feedTrack
			}
		}
		prevYTID = track.YTID
		if stopSession {
			return nil
		}
	}
}

// restartSession starts a fresh encoder session after the previous one
// crashed (Session.Done() delivered a non-nil error). The new session dir is
// created and its encoder started, then f.sessionDir is atomically swapped
// to it, and ONLY THEN is oldDir removed — sessionDir is an atomic.Value
// read concurrently (e.g. by the HLS handler), so a request racing the swap
// must always see either the old, still-intact dir or the new one, never a
// dir mid-deletion.
func (f *Feeder) restartSession(ctx context.Context, oldDir string) (dir string, sess Session, err error) {
	dir = filepath.Join(f.d.Dir, fmt.Sprintf("session-%d", f.seq.Add(1)))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	sess, err = f.d.Encoder.Start(ctx, dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	f.sessionDir.Store(dir)
	_ = os.RemoveAll(oldDir)
	return dir, sess, nil
}

// openTrack fetches the artifact and opens the decoder for track at offsetS
// seconds in (0 for a fresh track; >0 when re-opening after a crash-resume),
// BEFORE any air-log entry or publish happens for it. skip=true (err=nil)
// means fetch/decode failed for THIS track specifically — already logged;
// the caller advances past it and re-runs the boundary without announcing
// anything. err != nil is a fatal, session-ending error (e.g. can't even
// create the fetch scratch dir). On success, cleanup must be called exactly
// once, after the caller is done reading from rd.
func (f *Feeder) openTrack(ctx context.Context, track library.Track, offsetS float64) (rd io.ReadCloser, cleanup func(), skip bool, err error) {
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
	rd, derr := f.d.Decoder.Open(ctx, path, Loudness{I: track.InputI, TP: track.InputTP, LRA: track.InputLRA}, offsetS)
	if derr != nil {
		_ = os.RemoveAll(tmp)
		f.d.Logger.Error("decoder open failed; skipping track", "yt_id", track.YTID, "err", derr)
		return nil, nil, true, nil
	}
	return rd, func() { _ = rd.Close(); _ = os.RemoveAll(tmp) }, false, nil
}

// ResumeOffset computes how far into e the broadcast is at `now`. expired
// means the track already finished (resume at the next boundary instead).
// Negative skew (now before StartedAt) clamps to an offset of 0 rather than
// going negative. Pure and side-effect free — also used by the boot resume
// path (Task 9).
func ResumeOffset(e Entry, now time.Time) (offsetS float64, expired bool) {
	off := now.Sub(e.StartedAt).Seconds()
	if off < 0 {
		return 0, false
	}
	if off >= float64(e.DurationS) {
		return 0, true
	}
	return off, false
}

// airTrack feeds one track's already-open PCM reader, paced one chunk per
// clock tick. Returns:
//   - stop=true: the session must end entirely (ctx cancelled, or off-air
//     observed within one chunk). No resume, no next track.
//   - crashed=true: the encoder session died (Session.Done() delivered a
//     non-nil error). The caller must start a fresh encoder session and
//     resume this SAME track at its aired offset — see RunSession's
//     crash-resume loop. A Done() close with a NIL error (e.g. our own
//     Stop() during shutdown) is treated like ctx.Done — stop=true,
//     crashed=false — never as a crash, since only error values signal an
//     actual encoder failure.
//   - err != nil: a fatal, session-ending error unrelated to crash-resume
//     (e.g. an encoder stdin write failure).
//
// When none of the above, the track finished normally (EOF). Off-air is
// checked on every tick right after writing that tick's chunk, so an
// operator's flip takes effect within ~250ms rather than waiting for the
// track to finish.
func (f *Feeder) airTrack(ctx context.Context, sess Session, rd io.Reader, samplesFed *int64, tick, republish <-chan time.Time, entry Entry) (stop, crashed bool, err error) {
	buf := make([]byte, chunkBytes)
	for {
		select {
		case <-ctx.Done():
			return true, false, nil
		case derr := <-sess.Done():
			if derr == nil { // clean close (our own Stop) — not a crash
				return true, false, nil
			}
			return false, true, nil
		case <-republish:
			n, _ := f.d.Listeners.Count(ctx)
			f.publish(ctx, TopicNowPlaying, NowPlayingPayload(entry, n))
		case <-tick:
			// one paced chunk per tick
			n, rerr := io.ReadFull(rd, buf)
			if n > 0 {
				if _, werr := sess.Stdin().Write(buf[:n]); werr != nil {
					return false, false, fmt.Errorf("encoder write: %w", werr)
				}
				*samplesFed += int64(n / 4) // 4 bytes per stereo frame
			}
			// Off-air check on EVERY tick (not just at track end): the
			// operator's flip must take effect within ~one chunk, not
			// after the whole track finishes.
			if st, serr := f.d.Store.GetStation(ctx); serr == nil && !st.OnAir {
				return true, false, nil
			}
			if rerr != nil { // EOF/short read = track finished
				return false, false, nil
			}
		}
	}
}
