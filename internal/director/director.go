package director

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/the-algovn/radio-service/internal/brain"
	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/schedule"
	"github.com/the-algovn/radio-service/internal/spend"
	"github.com/the-algovn/radio-service/internal/station"
	"github.com/the-algovn/radio-service/internal/talkmem"
	"github.com/the-algovn/radio-service/internal/ttsclient"
)

const (
	// tickEvery is deliberately faster than the programmer's 60s — the
	// director must notice "break due" within one track.
	tickEvery = 20 * time.Second
	// prepDeadline bounds one whole prepare attempt. ttsclient sets no
	// timeout of its own and relies entirely on this deadline; a timeout is
	// an ordinary failure.
	prepDeadline = 60 * time.Second
	// stationIDMaxChars is the FIXED boot-time cap for committed station-ID
	// lines. The live max_chars setting governs LLM seam scripts only
	// (spec §4) — station-ID lines are pre-written and validated once at load.
	stationIDMaxChars = 450
)

// Ledger is the director's consumer-side ledger contract (the exported
// spend.Ledger lacks SpentSince; PGLedger/MemLedger both satisfy this —
// same pattern as programmer.Ledger).
type Ledger interface {
	Append(ctx context.Context, line spend.Line) error
	SpentSince(ctx context.Context, since time.Time) (float64, error)
}

// PeekFunc proposes what airs next so the break can open it by name. nil, or
// a false/error result, simply means she promises nothing.
//
// A peek is only a PROPOSAL. What makes it true is the pin (see prepare):
// schedule.NextUp is the one thing planNext will not let the request queue
// preempt. Never trust a peek without pinning — the ready-queue head is not
// stable.
type PeekFunc func(ctx context.Context) (live.Upcoming, bool, error)

// Pinner commits the promised track. schedule.Store satisfies it.
type Pinner interface {
	SetNextUp(ctx context.Context, n schedule.NextUp) error
}

type Deps struct {
	Model     brain.Model
	Voice     ttsclient.Speaker
	Ledger    Ledger
	Station   station.Store
	Listeners live.Listeners
	AirLog    live.AirLog
	TalkMem   talkmem.Store // persisted show memory; nil disables the thread

	PersonaDir     string
	StationIDsPath string
	DataDir        string // clip scratch dir (LAB_DATA_DIR/dj), swept at boot by main

	BudgetUSD float64

	Render   RenderFunc // nil → FFRender
	Clock    live.Clock
	Location *time.Location
	Logger   *slog.Logger

	Peek  PeekFunc // nil → never promise the next track
	Sched Pinner   // nil → never promise the next track
}

// Snapshot is a value copy of the cadence state a projection needs. It
// carries no cadence SETTINGS — BreakEvery and StationIDMin live on the
// station row and are re-read every tick, so the caller supplies those.
type Snapshot struct {
	Present bool

	HasClip             bool
	ClipKind            string
	ClipDurationS       float64
	ClipAnchorYTID      string
	ClipAnchorStartedAt time.Time
	ClipBacksellTitle   string
	ClipPromiseTitle    string
	ClipCorrelationID   string

	FinishedSinceSeam int
	LastStationID     time.Time
}

// Director prepares talk breaks ahead of air and hands them to the feeder
// through the live.TalkSource seam. One goroutine (Run) prepares; the feeder
// goroutine calls Take/TrackFinished; everything shared sits under mu.
type Director struct {
	d   Deps
	ids *stationIDs
	seq atomic.Int64 // clip filename counter (deterministic in tests)

	mu                sync.Mutex
	slot              *live.Clip
	finishedSinceSeam int
	lastStationID     time.Time
	wasOnAir          bool
}

func New(d Deps) *Director {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Location == nil {
		d.Location = time.UTC
	}
	if d.Render == nil {
		d.Render = FFRender
	}
	return &Director{d: d, ids: loadStationIDs(d.StationIDsPath, stationIDMaxChars, d.Logger)}
}

// TrackFinished advances the format clock: called by the feeder once per
// announced music item (never for talk clips).
func (dr *Director) TrackFinished(_ live.Entry) {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	dr.finishedSinceSeam++
}

// Take hands over the prepared clip, if any. Never blocks. Staleness is
// owned here: a seam whose anchor is not the entry that just finished is
// deleted and cleared before returning ok=false (the slot-empty wake gate
// must never livelock). station_id clips are always fresh. A successful
// hand-off resets the matching format-clock counter — a stale discard does
// NOT (the break is still owed and re-preps against the new anchor).
func (dr *Director) Take(justFinished live.Entry) (live.Clip, bool) {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	if dr.slot == nil {
		return live.Clip{}, false
	}
	c := *dr.slot
	if c.Kind == live.ClipSeam && !anchorFresh(c.AnchorYTID, c.AnchorStartedAt, justFinished) {
		dr.slot = nil
		_ = os.Remove(c.Path)
		dr.d.Logger.Info("stale seam discarded", "anchor_ytid", c.AnchorYTID, "finished_ytid", justFinished.YTID)
		return live.Clip{}, false
	}
	dr.slot = nil
	if c.Kind == live.ClipStationID {
		dr.lastStationID = dr.d.Clock.Now()
	} else {
		dr.finishedSinceSeam = 0
	}
	return c, true
}

// anchorFreshTolerance bounds the StartedAt comparison in anchorFresh.
const anchorFreshTolerance = time.Second

// anchorFresh reports whether the clip was prepared against the entry that
// just finished. StartedAt is compared with a 1s tolerance: the anchor
// round-trips through Postgres TIMESTAMPTZ (microsecond precision) while the
// feeder's entry carries the sample clock's nanoseconds — exact equality
// would discard every seam on a PG-backed deployment. Two airings of the
// same track are separated by at least a track length, so 1s is safe.
func anchorFresh(anchorYTID string, anchorStartedAt time.Time, justFinished live.Entry) bool {
	if anchorYTID != justFinished.YTID {
		return false
	}
	delta := anchorStartedAt.Sub(justFinished.StartedAt)
	if delta < 0 {
		delta = -delta
	}
	return delta <= anchorFreshTolerance
}

// dueKindLocked picks the due segment kind ("" = none). Caller holds mu.
// station_id wins when both are due; the seam counter carries over and
// stays due. The +1 counts the currently-airing track — the one the break
// will describe — so the default cadence is truly every BreakEvery tracks.
func (dr *Director) dueKindLocked(now time.Time, dj station.DJSettings) string {
	if dj.StationIDMin > 0 && dr.ids.available() &&
		now.Sub(dr.lastStationID) >= time.Duration(dj.StationIDMin)*time.Minute {
		return live.ClipStationID
	}
	if dj.BreakEvery > 0 && dr.finishedSinceSeam+1 >= dj.BreakEvery {
		return live.ClipSeam
	}
	return ""
}

// cancelPendingLocked discards a prepared-but-unaired clip (operator paused
// the DJ or the station went off-air). Caller holds mu.
func (dr *Director) cancelPendingLocked(reason string) {
	if dr.slot == nil {
		return
	}
	_ = os.Remove(dr.slot.Path)
	dr.d.Logger.Info("pending talk clip cancelled", "reason", reason, "kind", dr.slot.Kind)
	dr.slot = nil
}

// Run ticks the wake loop until ctx cancellation (programmer-shaped).
func (dr *Director) Run(ctx context.Context) error {
	tick := dr.d.Clock.Tick(tickEvery)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
			dr.RunOnce(ctx)
		}
	}
}

// RunOnce evaluates the wake gates (spec §3) in order: on-air → ai_enabled →
// listeners>0 → daily budget → segment due → slot empty; every failure is a
// quiet skip (music covers the air). Pause/off-air additionally cancel a
// pending clip; the off→on transition resets the station-id timer so the
// first ID airs ~StationIDMin into each broadcast session.
func (dr *Director) RunOnce(ctx context.Context) {
	st, err := dr.d.Station.GetStation(ctx)
	if err != nil {
		dr.d.Logger.ErrorContext(ctx, "director: station read failed", "err", err)
		return
	}
	now := dr.d.Clock.Now()

	dr.mu.Lock()
	if !st.OnAir || !st.AIEnabled {
		dr.cancelPendingLocked("paused or off-air")
	}
	if st.OnAir && !dr.wasOnAir {
		dr.lastStationID = now
	}
	dr.wasOnAir = st.OnAir
	dr.mu.Unlock()
	if !st.OnAir || !st.AIEnabled {
		return
	}

	if n, err := dr.d.Listeners.Count(ctx); err != nil || n == 0 {
		return
	}
	spent, err := dr.d.Ledger.SpentSince(ctx, request.DayStart(now, dr.d.Location))
	if err != nil {
		dr.d.Logger.ErrorContext(ctx, "director: spend read failed", "err", err)
		return
	}
	if spent >= dr.d.BudgetUSD {
		dr.d.Logger.WarnContext(ctx, "director: daily budget reached; idling", "spent_usd", spent)
		return
	}

	dr.mu.Lock()
	kind := ""
	if dr.slot == nil {
		kind = dr.dueKindLocked(now, st.DJ)
	}
	dr.mu.Unlock()
	if kind == "" {
		return
	}

	clip, ok := dr.prepare(ctx, kind, st)
	if !ok {
		return
	}
	dr.mu.Lock()
	dr.slot = &clip
	dr.mu.Unlock()
}

// Snapshot copies the mu-guarded fields and returns them by value.
//
// Constraints, all load-bearing:
//   - Pure field copies. Take and cancelPendingLocked already hold mu across
//     an os.Remove syscall on the audio hot path; this must add no I/O, no
//     logging and no clock read.
//   - The slot is copied BY VALUE. Take sets dr.slot = nil while a caller
//     could still hold the pointer.
//   - It must not touch finishedSinceSeam or lastStationID — a read that
//     mutates the format clock would silently change the cadence.
//   - Script is deliberately NOT copied here. live.Clip.Script is "logs only,
//     never published"; the aired script reaches the console from the stored
//     talk_segment row instead, which is admin-gated at the route.
func (dr *Director) Snapshot() Snapshot {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	s := Snapshot{
		Present:           true,
		FinishedSinceSeam: dr.finishedSinceSeam,
		LastStationID:     dr.lastStationID,
	}
	if dr.slot != nil {
		c := *dr.slot
		s.HasClip = true
		s.ClipKind = c.Kind
		s.ClipDurationS = c.DurationS
		s.ClipAnchorYTID = c.AnchorYTID
		s.ClipAnchorStartedAt = c.AnchorStartedAt
		s.ClipBacksellTitle = c.BacksellTitle
		s.ClipPromiseTitle = c.PromiseTitle
		s.ClipCorrelationID = c.CorrelationID
	}
	return s
}
