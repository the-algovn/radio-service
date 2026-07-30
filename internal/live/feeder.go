package live

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/schedule"
	"github.com/the-algovn/radio-service/internal/station"
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
	Store     station.Store
	Requests  request.Store  // the play queue; planNext priority 1–2
	Sched     schedule.Store // committed next-up (design 2026-07-23)
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
	// Rand picks the shuffle-bed track (0 <= result < n). nil → math/rand.Intn.
	Rand func(n int) int
	// Talk hands pre-rendered DJ talk clips to the boundary (v2). nil =
	// feature absent; the feeder behaves exactly as before.
	Talk TalkSource
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
	// skip is the operator's skip-current-track flag (v1.2). Ephemeral by
	// design: set by RequestSkip (any goroutine), consumed by airTrack's
	// next tick as "track finished", and cleared at session start so a
	// skip can never go stale across off-air. Single replica — no
	// persistence.
	skip atomic.Bool
}

func NewFeeder(d FeederDeps) *Feeder {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Rand == nil {
		d.Rand = rand.Intn
	}
	f := &Feeder{d: d}
	f.sessionDir.Store("")
	return f
}

func (f *Feeder) SessionDir() string { return f.sessionDir.Load().(string) }

// RequestSkip asks the currently-airing track to end at the next tick — a
// no-op when nothing consumes it (the flag is reset at session start).
func (f *Feeder) RequestSkip() { f.skip.Store(true) }

// startedAt converts the sample clock to wall time: anchor + samplesFed/48000,
// split into whole seconds + sub-second remainder so it cannot overflow int64
// nanoseconds (samplesFed can exceed ~9.2e9 ≈ 53h continuous at 48kHz).
func (f *Feeder) startedAt(samplesFed int64) time.Time {
	return f.anchor.Add(time.Duration(samplesFed/48000)*time.Second + time.Duration(samplesFed%48000)*time.Second/48000)
}

func (f *Feeder) publish(ctx context.Context, topic string, val []byte) {
	if f.d.Producer == nil {
		return
	}
	if err := f.d.Producer.Publish(ctx, topic, val); err != nil {
		f.d.Logger.ErrorContext(ctx, "publish failed", "topic", topic, "err", err)
	}
}

// airItem is planNext's pick: the library track to air and, when it came
// from the request queue, the request id to mark aired/failed plus its
// provenance (copied onto the air-log Entry; empty for shuffle picks).
type airItem struct {
	track           library.Track
	requestID       string
	source          string
	requestedByName string
	reason          string
}

const shuffleWindowCap = 50

// pickShuffleFrom chooses a shuffle-bed track from ids: uniform over the
// library minus the last-N aired. ok=false means the chosen track vanished
// between AllIDs and Get (caller skips/retries). ids must be non-empty.
func (f *Feeder) pickShuffleFrom(ctx context.Context, ids []string) (library.Track, bool, error) {
	window := len(ids) / 2
	if window > shuffleWindowCap {
		window = shuffleWindowCap
	}
	exclude := map[string]bool{}
	if window > 0 {
		recent, err := f.d.Log.RecentYTIDs(ctx, window)
		if err != nil {
			return library.Track{}, false, err
		}
		for _, id := range recent {
			exclude[id] = true
		}
	}
	pool := make([]string, 0, len(ids))
	for _, id := range ids {
		if !exclude[id] {
			pool = append(pool, id)
		}
	}
	if len(pool) == 0 {
		pool = ids // tiny library: the window covered everything
	}
	pick := pool[f.d.Rand(len(pool))]
	track, ok, err := f.d.Library.Get(ctx, pick)
	if err != nil {
		return library.Track{}, false, err
	}
	if !ok {
		return library.Track{}, false, nil
	}
	return track, true, nil
}

// commitNextUp keeps the queue non-empty (design 2026-07-23): the instant a
// track goes active, if the request queue is empty, pin a shuffle track as
// "next up"; otherwise pending requests already cover the slot, so clear any
// stale commitment. Every failure is logged and swallowed — a missing
// next-up only means planNext falls back to its own lazy shuffle.
func (f *Feeder) commitNextUp(ctx context.Context) {
	pending, err := f.d.Requests.Pending(ctx)
	if err != nil {
		f.d.Logger.ErrorContext(ctx, "commit next-up: pending read failed", "err", err)
		return
	}
	if len(pending) > 0 {
		if cerr := f.d.Sched.ClearNextUp(ctx); cerr != nil {
			f.d.Logger.ErrorContext(ctx, "commit next-up: clear failed", "err", cerr)
		}
		return
	}
	ids, err := f.d.Library.AllIDs(ctx)
	if err != nil {
		f.d.Logger.ErrorContext(ctx, "commit next-up: library read failed", "err", err)
		return
	}
	if len(ids) == 0 {
		return // empty library — nothing to commit; commitNext handles off-air
	}
	track, ok, err := f.pickShuffleFrom(ctx, ids)
	if err != nil {
		f.d.Logger.ErrorContext(ctx, "commit next-up: shuffle pick failed", "err", err)
		return
	}
	if !ok {
		return // vanished; next track start tries again
	}
	if serr := f.d.Sched.SetNextUp(ctx, schedule.NextUp{
		YTID: track.YTID, Title: track.Title, Channel: track.Channel,
	}); serr != nil {
		f.d.Logger.ErrorContext(ctx, "commit next-up: set failed", "err", serr)
	}
}

// plan is what planNext decided, with none of it committed. Every field that
// implies a write is a flag for commitNext to act on, never an action taken.
type plan struct {
	item     airItem
	skip     bool // chosen item can't air; caller re-plans
	stop     bool // operator off-air observed, or nothing to air at all
	emptyLib bool // library empty — commitNext turns this into auto off-air
	// consumedNextUp records that item came from the committed next-up, so
	// commitNext knows to clear it. planNext must not clear it itself: a
	// transition that aborts after planning would otherwise lose the
	// commitment with no way to recover it.
	consumedNextUp bool
	// failRequestID is a ready request whose track vanished. commitNext marks
	// it failed; planNext only reports it.
	failRequestID string
}

// planNext chooses what airs next using reads only, so it is safe to call
// ahead of the boundary from the prefetch goroutine. Priority is unchanged:
// committed next-up, then oldest ready request, then library no-repeat
// shuffle. Callers on the lookahead path treat every error as non-fatal.
func (f *Feeder) planNext(ctx context.Context) (plan, error) {
	st, err := f.d.Store.GetStation(ctx)
	if err != nil {
		return plan{}, err
	}
	if !st.OnAir {
		// Reported, never acted on here. Acting on it at plan time would end
		// the session while the current item is still airing, truncating it
		// by the lookahead distance. airTrack's per-tick check is what
		// actually stops the session, within one chunk.
		return plan{stop: true}, nil
	}

	nu, ok, err := f.d.Sched.GetNextUp(ctx)
	if err != nil {
		return plan{}, err
	}
	if ok {
		track, exists, gerr := f.d.Library.Get(ctx, nu.YTID)
		if gerr != nil {
			// The next-up is already being consumed at this point: the
			// pre-split code cleared it unconditionally BEFORE this Get ever
			// ran, so a persistent Get failure still let the next restart
			// find GetNextUp empty and fall through to requests/shuffle
			// instead of wedging on the same broken commitment forever.
			// planNext cannot clear it itself — the lookahead path must
			// never commit a consumption it might still abandon — so it
			// reports the pending consumption via consumedNextUp alongside
			// the error; planInline commits it before propagating, while the
			// lookahead path discards the whole plan on any error.
			return plan{consumedNextUp: true}, gerr
		}
		if !exists {
			return plan{skip: true, consumedNextUp: true}, nil
		}
		it := airItem{track: track}
		if nu.RequestID != "" {
			// A pinned REQUEST (the DJ promised it on air — spec
			// 2026-07-29-radio-dj-seam-breaks §3). Rebuild the same airItem
			// the request path builds below, or the track airs unattributed
			// and the request never leaves the queue.
			req, found, rerr := f.d.Requests.Get(ctx, nu.RequestID)
			if rerr != nil {
				return plan{consumedNextUp: true}, rerr
			}
			// Unusable commitments: cancelled, failed, or ALREADY AIRED —
			// the last one happens when the director's prepare straddled a
			// seam, and without this check next_up would replay a track back
			// to back. Consume and let the queue decide; this is not a
			// request FAILURE, so failRequestID stays empty.
			if !found || req.Status != request.StatusReady {
				return plan{skip: true, consumedNextUp: true}, nil
			}
			it = airItem{track: track, requestID: req.ID, source: req.Source,
				requestedByName: req.DisplayName, reason: req.Reason}
		}
		return plan{item: it, consumedNextUp: true}, nil
	}

	req, found, err := f.d.Requests.NextReady(ctx)
	if err != nil {
		return plan{}, err
	}
	if found {
		track, exists, gerr := f.d.Library.Get(ctx, req.YTID)
		if gerr != nil {
			return plan{}, gerr
		}
		if !exists {
			return plan{skip: true, failRequestID: req.ID}, nil
		}
		return plan{item: airItem{track: track, requestID: req.ID,
			source: req.Source, requestedByName: req.DisplayName, reason: req.Reason}}, nil
	}

	ids, err := f.d.Library.AllIDs(ctx)
	if err != nil {
		return plan{}, err
	}
	if len(ids) == 0 {
		return plan{stop: true, emptyLib: true}, nil
	}
	track, ok, err := f.pickShuffleFrom(ctx, ids)
	if err != nil {
		return plan{}, err
	}
	if !ok {
		return plan{skip: true}, nil // vanished between AllIDs and Get
	}
	return plan{item: airItem{track: track}}, nil
}

// commitNext applies a plan's writes and returns the tuple the seam has always
// handed RunSession. It runs on the feed goroutine, paired with planInline and
// never on the lookahead path, and keeps today's fatality: a MarkFailed store
// error ends the session rather than leaving the same request re-picked
// forever.
func (f *Feeder) commitNext(ctx context.Context, p plan) (airItem, bool, bool, error) {
	if p.consumedNextUp {
		if err := f.d.Sched.ClearNextUp(ctx); err != nil {
			return airItem{}, false, false, err
		}
	}
	if p.failRequestID != "" {
		if err := f.d.Requests.MarkFailed(ctx, p.failRequestID, "track vanished from library"); err != nil {
			// A persistently-failing store would otherwise leave this same
			// ready request re-picked at every seam — an unpaced
			// spin inside the audio goroutine. Surfacing it as fatal lets
			// Engine.Run's 5s poll pace the retry.
			f.d.Logger.ErrorContext(ctx, "mark request failed", "id", p.failRequestID, "err", err)
			return airItem{}, false, false, err
		}
	}
	if p.emptyLib {
		return airItem{}, false, true, f.autoOffAir(ctx)
	}
	return p.item, p.skip, p.stop, nil
}

// planInline is planNext run ON THE FEED GOROUTINE, where a failed plan may
// still commit the one consumption planNext could only report.
//
// planNext can fail with consumedNextUp set (a Library.Get error on the
// next-up's own track). The pre-split code cleared the next-up unconditionally
// before attempting that Get, so the clear has to happen here before the error
// surfaces — otherwise the commitment outlives the failed plan and every
// restart re-picks the same broken next-up forever instead of self-healing.
//
// The lookahead path must NOT call this: a prefetch that is abandoned has to
// consume nothing. That asymmetry is the whole reason planNext and commitNext
// are separate — see startPrefetch.
func (f *Feeder) planInline(ctx context.Context) (plan, error) {
	p, err := f.planNext(ctx)
	if err == nil {
		return p, nil
	}
	if p.consumedNextUp {
		if cerr := f.d.Sched.ClearNextUp(ctx); cerr != nil {
			// Matches the original's precedence: ClearNextUp ran first there,
			// so its own failure was the only error a caller ever saw — the
			// Get failure it would have masked never surfaced.
			return plan{}, cerr
		}
	}
	return plan{}, err
}

// prefetch is one in-flight lookahead: planNext plus openTrack, run off the
// feed goroutine. openTrack is a whole-blob MinIO GET plus an ffmpeg spawn,
// and the feed goroutine is the only thing feeding the encoder — running it
// inline during a track would stall the live stream for the fetch duration.
//
// Nothing here commits anything. commitNext still runs on the feed goroutine
// at the boundary, so an abandoned prefetch loses no state.
//
// Every field is written by the prefetch goroutine before it closes done and
// read by the feed goroutine only after done is closed, so the channel is the
// only synchronization these need.
type prefetch struct {
	done    chan struct{}
	p       plan
	rd      io.ReadCloser
	cleanup func()
	err     error
}

// startPrefetch plans and opens the next item on its own goroutine. The
// returned prefetch is always non-nil; every failure is reported through it
// and handled by the caller as "plan and open inline instead", which is what
// the boundary did before this existed.
//
// It calls planNext, never planInline, and discards the plan whole on any
// error. planNext reports a pending next-up consumption alongside its errors
// so that the feed goroutine can commit it (see planInline); a lookahead is
// the opposite case — it may be abandoned by going off air, by a ctx
// cancellation or by the session ending, and an abandoned lookahead must
// consume nothing. Leaving the consumption unclaimed here costs nothing: the
// inline re-plan that follows a failed prefetch takes exactly the path the
// boundary always took.
func (f *Feeder) startPrefetch(ctx context.Context) *prefetch {
	pf := &prefetch{done: make(chan struct{})}
	go func() {
		defer close(pf.done)
		p, err := f.planNext(ctx)
		if err != nil {
			pf.err = err // p deliberately discarded — see above
			return
		}
		pf.p = p
		if p.skip || p.stop {
			return // nothing to open; the boundary acts on the flags
		}
		rd, cleanup, skip, oerr := f.openTrack(ctx, p.item.track, 0)
		if oerr != nil {
			pf.err = oerr
			return
		}
		if skip {
			// This artifact failed to fetch or decode. Reported as "opened
			// nothing" rather than as an error: the plan is still good, so the
			// boundary commits it and retries the open inline, where the
			// existing mark-failed-and-skip path already lives.
			return
		}
		pf.rd, pf.cleanup = rd, cleanup
	}()
	return pf
}

// takePrefetch produces the boundary's decision, reusing an in-flight
// lookahead's already-open reader when it is still the right one.
//
// The DECISION is always made here, at the seam, from fresh reads — the
// lookahead moves only the fetch. That is not a detail: a lookahead plans a
// whole item plus any talk break early, and a listener request that finishes
// downloading in that window would lose its slot to the stale pick if the
// lookahead's plan were simply trusted. So the plan is re-made inline and the
// prefetched reader is kept only when it opened the same track the fresh plan
// chose; otherwise it is closed and the caller opens inline. Wasting a fetch
// exactly when the pick changed is the same "degrade to today's stream"
// fallback every other failure here takes.
//
// The re-plan uses planInline, never planNext: it runs on the feed goroutine
// and is the call that must commit a broken next-up's consumption so the
// station self-heals rather than wedging on it forever.
//
// It waits unconditionally rather than racing ctx.Done. The prefetch goroutine
// is the sole owner of any reader it has opened until this call hands it over,
// so returning early would strand an ffmpeg process and its temp dir with
// nothing left holding them. Waiting is also exactly the blocking this change
// preserves: a lookahead that is not ready at the boundary costs the same
// stall the inline open cost before it, and planNext/openTrack both take ctx,
// so a cancelled session unblocks this promptly.
func (f *Feeder) takePrefetch(ctx context.Context, pf *prefetch) (plan, io.ReadCloser, func(), error) {
	if pf != nil {
		<-pf.done
		if pf.err != nil {
			f.d.Logger.ErrorContext(ctx, "prefetch failed; planning inline", "err", pf.err)
		}
	}
	p, err := f.planInline(ctx)
	if pf == nil || pf.rd == nil {
		return p, nil, nil, err // nothing was opened ahead; open inline
	}
	// The reader is the lookahead's alone until it is returned here — it was
	// never registered with the session's openSet, which only happens when the
	// boundary adopts it — so discarding it means closing it right here.
	if err != nil || p.skip || p.stop || p.item.track.YTID != pf.p.item.track.YTID {
		f.d.Logger.InfoContext(ctx, "lookahead superseded; opening the new pick inline",
			"prefetched", pf.p.item.track.YTID, "chosen", p.item.track.YTID)
		pf.cleanup()
		return p, nil, nil, err
	}
	// Same track — but p, not pf.p, is what gets committed and aired: the fresh
	// plan carries this item's real provenance (a request id, source and reason
	// the lookahead's shuffle pick would not have had) and the flags commitNext
	// must act on.
	return p, pf.rd, pf.cleanup, nil
}

// autoOffAir persists off-air (engine-side closure) and reports the sentinel;
// store errors are logged, not fatal — the session ends either way.
func (f *Feeder) autoOffAir(ctx context.Context) error {
	if _, err := f.d.Store.GoOffAir(ctx); err != nil {
		f.d.Logger.ErrorContext(ctx, "auto off-air persist failed", "err", err)
	}
	f.d.Logger.InfoContext(ctx, "auto off-air: library is empty")
	return nil
}

// bootResumeEntry is a boot-resume candidate found by findBootResume: the
// air log's latest entry, still mid-flight and whose track still exists in
// the library, that a fresh RunSession should air first (at its
// already-elapsed offset) instead of starting fresh at a shuffle pick.
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
// session's own encoder ticks. A resume candidate needs only a fresh,
// unexpired air-log entry whose track still exists in the library — the
// engine consumes the request queue and the shuffle bed only, so nothing
// else gates what airs. Every failure
// mode here (log/library errors or an expired entry) is treated as "no
// resume" rather than fatal — RunSession's first real plan right after this
// hits the same store/library and will surface any persistent
// problem through its own (fatal) error path.
func (f *Feeder) findBootResume(ctx context.Context) *bootResumeEntry {
	entry, found, err := f.d.Log.Latest(ctx)
	if err != nil {
		f.d.Logger.ErrorContext(ctx, "boot resume: air log read failed", "err", err)
		return nil
	}
	if !found {
		return nil
	}
	offsetS, expired := ResumeOffset(entry, time.Now())
	if expired {
		return nil
	}
	track, ok, err := f.d.Library.Get(ctx, entry.YTID)
	if err != nil {
		f.d.Logger.ErrorContext(ctx, "boot resume: library read failed", "err", err)
		return nil
	}
	if !ok {
		return nil
	}
	return &bootResumeEntry{track: track, entry: entry, offsetS: offsetS}
}

// onAir is everything that describes the item currently being fed.
//
// It exists as one struct rather than six loop variables because a transition
// flips which item is on air *inside* an airTrack call. With the variables
// loose, the crash-resume block reads the OUTGOING item's track, entry and
// start offset and reopens the wrong file past its EOF — no compile error, no
// failing test, and the listener silently loses the song they are hearing.
// One struct makes the flip a single assignment and crash-resume correct by
// construction.
type onAir struct {
	track library.Track
	entry Entry
	item  airItem
	// startSamples is samplesFed when this item's first byte was fed. Crash
	// resume computes its offset from here, so it is fixed once per item and
	// never per crash-restart.
	startSamples int64
	rd           io.ReadCloser
	cleanup      func()
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
	f.skip.Store(false) // a skip requested while off-air must not cut the next session's first track

	var samplesFed int64 // 4 bytes per stereo sample-frame at s16le
	resume := f.findBootResume(ctx)
	if resume != nil {
		f.anchor = resume.entry.StartedAt
		samplesFed = int64(resume.offsetS * 48000)
	}

	var lastFinished Entry // zero until the first music item ends this session
	// awaitMusic enforces at most one talk break per seam: a music item must
	// air between any two talk breaks. Set whenever Take hands over a clip,
	// regardless of how the clip airing ends (aired, crashed, or failed
	// open); cleared only once a music item has actually finished airing —
	// see the TrackFinished call at the loop tail below.
	awaitMusic := false

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

	// Every reader an ITEM holds is registered here as well as owned by that
	// item, so no exit path can leave an ffmpeg decode process and its fetch
	// temp dir behind. Each cleanup is `once`-wrapped, so the ones already run
	// at item end are no-ops. A reader the lookahead has opened but no item has
	// adopted yet is NOT in here — it is owned solely by its prefetch until the
	// boundary registers it, and the drain defer just below is what closes it.
	set := &openSet{}
	defer set.closeAll()

	// pf is the lookahead for the item AFTER the one currently airing, started
	// as soon as that one is announced and consumed at the next boundary. It
	// outlives an iteration of the loop (a talk break or a crash-capped track
	// leaves it in flight), so it lives out here with the session.
	var pf *prefetch
	// A lookahead that is never consumed owns its reader alone — the boundary
	// is what hands it to set — so every exit that leaves one in flight has to
	// wait for it and close whatever it opened. Declared after set.closeAll so
	// it runs BEFORE it: both are backstops, and draining first keeps the
	// ordering "nothing is still being opened by the time the set is emptied".
	defer func() {
		if pf != nil {
			<-pf.done
			if pf.cleanup != nil {
				pf.cleanup()
			}
		}
	}()

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

	tick := f.d.Clock.Tick(250 * time.Millisecond)
	republish := f.d.Clock.Tick(republishEvery)

	for {
		var cur onAir
		// offsetS stays a local because it is per-OPEN, not per-item: the boot
		// resume offset for the first open, then recomputed from
		// cur.startSamples on every crash-resume reopen.
		var offsetS float64
		resumed := resume != nil
		if resumed {
			cur.track, cur.entry, offsetS = resume.track, resume.entry, resume.offsetS
			resume = nil
		} else {
			// Talk break first — take-if-ready, never waits (v2). Placed
			// before the boundary below so a due break airs at the seam
			// between tracks; lastFinished carries the seam clip's freshness
			// anchor. It runs ahead of takePrefetch on purpose: a clip is
			// already on disk, so airing one costs no fetch, and the lookahead
			// it leaves in flight is simply taken at the seam after it.
			if f.d.Talk != nil && !awaitMusic {
				if clip, ok := f.d.Talk.Take(lastFinished); ok {
					awaitMusic = true
					stopClip, crashed, cerr := f.airClip(ctx, sess, clip, &samplesFed, tick, republish)
					if cerr != nil {
						return cerr
					}
					if crashed {
						newDir, newSess, rerr := f.restartSession(ctx, dir)
						if rerr != nil {
							return rerr
						}
						dir, sess = newDir, newSess
						continue // clip remainder skipped by design — no -ss reopen for talk
					}
					if stopClip {
						return nil
					}
					continue
				}
			}
			p, rd, cleanup, err := f.takePrefetch(ctx, pf)
			pf = nil
			if rd != nil {
				// Adopt the prefetched reader into cur AND the session
				// backstop right here, ahead of the checks below: every one of
				// them is a return or a continue, and a reader held only in a
				// local would be stranded by any of them. own folds
				// deregistration into the cleanup, so cur.cleanup is the
				// single owner exactly as on the inline-open path.
				cur.rd, cur.cleanup = rd, set.own(cleanup)
			}
			var it airItem
			var skip, stop bool
			if err == nil {
				// A plan reaches commitNext only when it planned cleanly.
				// planNext returns a ZERO plan alongside its errors, and
				// commitNext turns that into an empty airItem with neither
				// skip nor stop set — which everything below would air as a
				// real track with a blank YTID.
				it, skip, stop, err = f.commitNext(ctx, p)
			}
			if stop || ctx.Err() != nil {
				return nil // the deferred set.closeAll closes an adopted reader
			}
			if err != nil {
				return err
			}
			if skip { // vanished track: commitNext already handled it
				// Unreachable holding a reader today — the prefetch opens only
				// for a plan that airs — but continue is the one exit the
				// session backstop does not cover until the session ends, so
				// close here rather than rely on the argument.
				if cur.cleanup != nil {
					cur.cleanup()
				}
				continue
			}
			cur.item = it
			cur.track = cur.item.track
		}

		// Open the artifact + decoder BEFORE announcing anything: a track
		// that fails to fetch/decode must never be logged or published as
		// now-playing — it never aired, and doing so would poison the
		// sample-clock resume anchor with a track nobody heard.
		//
		// cur.rd is already set when the lookahead opened this item ahead of
		// the boundary. Everything else — the boot resume, and every fallback
		// in takePrefetch — opens here, on the feed goroutine, exactly as
		// before.
		var openSkip bool
		if cur.rd == nil {
			var oerr error
			cur.rd, cur.cleanup, openSkip, oerr = f.openTrack(ctx, cur.track, offsetS)
			if oerr != nil {
				return oerr
			}
			if openSkip {
				if cur.item.requestID != "" {
					if err := f.d.Requests.MarkFailed(ctx, cur.item.requestID, "artifact failed to open"); err != nil {
						f.d.Logger.ErrorContext(ctx, "mark request failed", "id", cur.item.requestID, "err", err)
					}
				}
				continue
			}
			// Hand the reader to the session backstop. cur.cleanup stays the
			// single owner: own folds the deregistration into it, so closing
			// the reader and dropping it from the set are one act that cannot
			// come apart.
			cur.cleanup = set.own(cur.cleanup)
		}

		// cur.startSamples anchors this track's aired offset: on a crash
		// mid-track, offset = (samplesFed at crash - cur.startSamples)/48000.
		// It is fixed once per track (not per crash-restart), so repeated
		// crashes on the same track keep computing offset from the track's
		// true start, not from the previous restart point. For a
		// boot-resumed track, samplesFed already starts pre-loaded at the
		// resumed offset (see above), and the track's true start is 0
		// frames from f.anchor (== the entry's original StartedAt) — not
		// the current samplesFed value.
		if resumed {
			cur.startSamples = 0
		} else {
			startedAt := f.startedAt(samplesFed)
			cur.entry = Entry{YTID: cur.track.YTID, Title: cur.track.Title, Artist: cur.track.Channel,
				StartedAt: startedAt, DurationS: int(cur.track.DurationS),
				Source: cur.item.source, RequestedByName: cur.item.requestedByName, Reason: cur.item.reason}
			if err := f.d.Log.Append(ctx, cur.entry); err != nil {
				f.d.Logger.ErrorContext(ctx, "air log append failed", "err", err)
			}
			cur.startSamples = samplesFed
			// Mark the request aired only now: openTrack succeeded and the
			// air-log entry is written, so this track genuinely aired.
			if cur.item.requestID != "" {
				if err := f.d.Requests.MarkAired(ctx, cur.item.requestID, cur.entry.StartedAt); err != nil {
					f.d.Logger.ErrorContext(ctx, "mark request aired", "id", cur.item.requestID, "err", err)
				}
			}
		}
		f.commitNextUp(ctx)
		// Start the lookahead for the item after this one. It must run AFTER
		// commitNextUp: planNext reads the next-up commitment commitNextUp
		// just made, and that ordering is what keeps the pick identical to the
		// one the boundary made when it planned at the seam.
		pf = f.startPrefetch(ctx)
		n, _ := f.d.Listeners.Count(ctx)
		f.publish(ctx, TopicNowPlaying, NowPlayingPayload(cur.entry, n))
		PublishQueueSnapshot(ctx, f.d.Producer, f.d.Requests, f.d.Sched, f.d.Logger)

		crashRestarts := 0
		stopSession := false
	feedTrack:
		for {
			stopTrack, crashed, aerr := f.airTrack(ctx, sess, cur.rd, &samplesFed, tick, republish,
				func(n int) []byte { return NowPlayingPayload(cur.entry, n) })
			if !crashed {
				cur.cleanup()
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
			cur.cleanup()
			crashRestarts++
			newDir, newSess, rerr := f.restartSession(ctx, dir)
			if rerr != nil {
				return rerr
			}
			dir, sess = newDir, newSess

			if crashRestarts > 3 {
				f.d.Logger.ErrorContext(ctx, "crash-resume attempts exhausted; skipping track",
					"yt_id", cur.track.YTID, "restarts", crashRestarts-1)
				break feedTrack
			}

			offsetS := float64(samplesFed-cur.startSamples) / 48000
			// Assignment, not `:=` — cur.rd/cur.cleanup are the SAME fields
			// read at the top of this loop and at the end of the outer
			// per-track block, and openSkip is the same variable; `:=` here
			// would shadow them inside just this iteration's block and the
			// reopened reader would never actually be fed (classic for-loop
			// redeclaration trap). Routing rd/cleanup through cur makes that
			// trap structurally impossible for them; oerr still needs the care.
			var oerr error
			cur.rd, cur.cleanup, openSkip, oerr = f.openTrack(ctx, cur.track, offsetS)
			if oerr != nil {
				return oerr
			}
			if openSkip {
				f.d.Logger.ErrorContext(ctx, "resume reopen failed; skipping track", "yt_id", cur.track.YTID)
				break feedTrack
			}
			// Same single-owner registration as the first open above.
			cur.cleanup = set.own(cur.cleanup)
		}
		if stopSession {
			return nil
		}
		// One music item finished (EOF, operator skip, or crash-cap skip) —
		// it was announced and air-logged, so it counts on the format clock
		// and becomes the seam clip's freshness anchor. Talk clips never reach
		// here (airClip's branch continues the loop above); failed-open
		// tracks continue before this point and were never announced.
		if f.d.Talk != nil {
			f.d.Talk.TrackFinished(cur.entry)
		}
		lastFinished = cur.entry
		awaitMusic = false // a music item just aired — the seam guard resets
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

// once wraps a cleanup so it runs at most once. Item cleanup is called at the
// item's end AND by the session-end backstop, and once a transition can abort
// mid-item there is no single place that reliably owns the call — so the
// cleanup itself has to tolerate being invoked twice.
func once(fn func()) func() {
	var o sync.Once
	return func() { o.Do(fn) }
}

// openSet is the session-level backstop for every reader STILL OPEN during one
// RunSession. Today one reader is open at a time and the per-item cleanup
// covers it; once a transition holds two, an aborted transition would
// otherwise leak an ffmpeg decode process and a temp dir holding a whole
// audio artifact — permanently, because FFDecoder binds the process to
// RunSession's ctx, which is Engine.Run's ctx, which is process lifetime.
//
// Registrations are dropped as the readers close. Append-only would be
// simpler and wrong: a session lasts as long as the station stays on air —
// weeks — so an entry per track open would accumulate for the whole run, each
// one pinning a *decodeReader and through it an *exec.Cmd and that command's
// stderr buffer. That is unbounded growth in a process designed never to
// restart, so registering and deregistering are one operation: see own.
type openSet struct {
	mu   sync.Mutex
	next int64
	open []openEntry // registration order, oldest first
}

// openEntry is one still-open reader. It carries an id because func values are
// not comparable in Go, so a cleanup cannot be found in the slice by identity.
type openEntry struct {
	id      int64
	cleanup func()
}

// own registers cleanup as an open reader and returns the cleanup the caller
// must hold from here on: running it closes the reader and then drops the
// registration, so the set only ever holds readers still genuinely open.
//
// This is deliberately the ONLY way to register. A bare "add" that handed back
// a separate remove would let an open site register without folding the
// removal back in, silently restoring the unbounded growth described above —
// and nothing would catch it, since the openSet tests exercise the type, not
// its call sites.
//
// The returned cleanup tolerates being called twice, and being called after
// closeAll: cleanup is `once`-wrapped by openTrack, and closeAll invokes the
// registered cleanups only after releasing o.mu, so a late call re-entering
// removeID simply scans an already-emptied set.
func (o *openSet) own(cleanup func()) func() {
	o.mu.Lock()
	id := o.next
	o.next++
	o.open = append(o.open, openEntry{id: id, cleanup: cleanup})
	o.mu.Unlock()
	return func() {
		cleanup()
		o.removeID(id)
	}
}

func (o *openSet) removeID(id int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i, e := range o.open {
		if e.id == id {
			// slices.Delete, not a re-slice: it zeroes the vacated tail, so
			// the backing array stops pinning the removed closure.
			o.open = slices.Delete(o.open, i, i+1)
			return
		}
	}
}

// closeAll runs every cleanup still registered, newest first. Each is
// `once`-wrapped, so one that raced an item's own cleanup is a no-op.
func (o *openSet) closeAll() {
	o.mu.Lock()
	entries := o.open
	o.open = nil
	o.mu.Unlock()
	for i := len(entries) - 1; i >= 0; i-- {
		entries[i].cleanup()
	}
}

// openTrack fetches the artifact and opens the decoder for track at offsetS
// seconds in (0 for a fresh track; >0 when re-opening after a crash-resume),
// BEFORE any air-log entry or publish happens for it. skip=true (err=nil)
// means fetch/decode failed for THIS track specifically — already logged;
// the caller advances past it and re-runs the boundary without announcing
// anything. err != nil is a fatal, session-ending error (e.g. can't even
// create the fetch scratch dir). On success, cleanup must be called once the
// caller is done reading from rd. It is idempotent (`once`-wrapped) because it
// has two owners: the item that opened the reader, and the session-level
// backstop that closes whatever an aborted item left open — either may reach
// it first, and the reader must close exactly once regardless.
func (f *Feeder) openTrack(ctx context.Context, track library.Track, offsetS float64) (rd io.ReadCloser, cleanup func(), skip bool, err error) {
	tmp, err := os.MkdirTemp(f.d.Dir, "fetch-*")
	if err != nil {
		return nil, nil, false, err
	}
	path, ferr := f.d.Fetch(ctx, track.ArtifactID, tmp)
	if ferr != nil {
		_ = os.RemoveAll(tmp)
		f.d.Logger.ErrorContext(ctx, "artifact fetch failed; skipping track", "yt_id", track.YTID, "err", ferr)
		return nil, nil, true, nil
	}
	rd, derr := f.d.Decoder.Open(ctx, path, Loudness{I: track.InputI, TP: track.InputTP, LRA: track.InputLRA}, offsetS)
	if derr != nil {
		_ = os.RemoveAll(tmp)
		f.d.Logger.ErrorContext(ctx, "decoder open failed; skipping track", "yt_id", track.YTID, "err", derr)
		return nil, nil, true, nil
	}
	return rd, once(func() { _ = rd.Close(); _ = os.RemoveAll(tmp) }), false, nil
}

// airClip airs one pre-rendered talk clip: a raw s16le/48kHz/stereo file fed
// through the same airTrack pacing as music. Open-before-announce applies —
// the dj frame publishes only after the file opens — and the clip file is
// deleted on every exit path (aired, cut by skip/off-air, crashed, failed
// open). crashed=true means the encoder died mid-clip: the caller restarts
// the session and skips the remainder (talk clips are never -ss reopened).
// No air-log entry is written — history stays music-only and boot-resume
// anchors on the last real track.
func (f *Feeder) airClip(ctx context.Context, sess Session, clip Clip, samplesFed *int64, tick, republish <-chan time.Time) (stop, crashed bool, err error) {
	rd, oerr := os.Open(clip.Path)
	if oerr != nil {
		f.d.Logger.ErrorContext(ctx, "talk clip open failed; skipping break", "path", clip.Path, "err", oerr)
		_ = os.Remove(clip.Path)
		return false, false, nil
	}
	defer func() {
		_ = rd.Close()
		_ = os.Remove(clip.Path)
	}()
	entry := Entry{Title: "Tiểu Dương Dương",
		StartedAt: f.startedAt(*samplesFed), DurationS: int(clip.DurationS + 0.5)}
	n, _ := f.d.Listeners.Count(ctx)
	f.publish(ctx, TopicNowPlaying, DJPayload(entry, n))
	return f.airTrack(ctx, sess, rd, samplesFed, tick, republish,
		func(listeners int) []byte { return DJPayload(entry, listeners) })
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

// airTrack feeds one item's already-open PCM reader, paced one chunk per
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
//
// frame builds the republish now-playing payload for this item (track or
// talk clip) from a fresh listeners count.
func (f *Feeder) airTrack(ctx context.Context, sess Session, rd io.Reader, samplesFed *int64, tick, republish <-chan time.Time, frame func(listeners int) []byte) (stop, crashed bool, err error) {
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
			f.publish(ctx, TopicNowPlaying, frame(n))
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
			if f.skip.CompareAndSwap(true, false) {
				return false, false, nil // operator skip: treat as track finished
			}
			if rerr != nil { // EOF/short read = track finished
				return false, false, nil
			}
		}
	}
}
