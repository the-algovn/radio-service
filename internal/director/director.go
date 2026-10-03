package director

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/the-algovn/radio-service/internal/brain"
	"github.com/the-algovn/radio-service/internal/cadence"
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
	// defaultPrepDeadline bounds one whole prepare attempt (overridable via
	// Deps.PrepDeadline). ttsclient sets no timeout of its own and relies
	// entirely on this deadline; a timeout is an ordinary failure.
	defaultPrepDeadline = 60 * time.Second
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

	// PrepDeadline bounds one prepare attempt; 0 means defaultPrepDeadline.
	// Self-hosted CPU voices render far slower than a cloud voice.
	PrepDeadline time.Duration

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

	// StationIDsAvailable mirrors the ids.available() term of dueKindLocked.
	// Without it a reader cannot reproduce the due test: with no usable
	// station-ID lines the engine falls through to BreakEvery and airs a seam
	// where a reader would expect a station_id.
	StationIDsAvailable bool

	// Forced mirrors the operator-armed term of dueKindLocked. internal/timeline
	// re-implements the due test off this snapshot, so omitting it leaves the
	// console blind to a forced break until the clip is prepared a tick later.
	Forced bool

	// SessionHasMusic mirrors the gate of the same name in cadence.DueKind.
	// Without it the projector cannot tell a session that has aired music
	// from one that has not, and promises a seam the engine will not make.
	SessionHasMusic bool

	// LastMusing, PendingDaypart and PendingWake are the remaining terms of
	// cadence.DueKind. internal/timeline re-evaluates the whole ladder off this
	// snapshot, so a term left out here is a segment the console silently never
	// projects - and, worse, a seam it projects in that segment's place.
	LastMusing     time.Time
	PendingDaypart time.Time
	PendingWake    bool
}

// Director prepares talk breaks ahead of air and hands them to the feeder
// through the live.TalkSource seam. One goroutine (Run) prepares; the feeder
// goroutine calls Take/TrackFinished; everything shared sits under mu.
type Director struct {
	d   Deps
	ids *stationIDs
	seq atomic.Int64 // clip filename counter (deterministic in tests)

	mu       sync.Mutex
	slot     *live.Clip
	cad      cadence.State
	wasOnAir bool

	// wasAIEnabled is the edge detector for the RESUME transition, which is
	// deliberately not the same thing as a new broadcast session.
	wasAIEnabled bool
	// lastDaypart is the rollover edge detector. Seeded at session open, which
	// is what stops a bogus transition on a broadcast's first tick.
	lastDaypart string
	// daypartFrom is the daypart the night just left, held for the transition's
	// brief so she can name both sides of the hinge.
	daypartFrom string
	// aiDisabledSince times the silence; silentForMin is the answer, kept
	// separately because the answer has to survive until the greeting is
	// actually prepared, which can be several ticks after the resume.
	aiDisabledSince time.Time
	silentForMin    int
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
	dr.cad = cadence.AdvanceMusic(dr.cad)
}

// Take hands over the prepared clip, if any. Never blocks. Staleness is
// owned here: an ANCHORED clip whose anchor is not the entry that just
// finished is deleted and cleared before returning ok=false (the slot-empty
// wake gate must never livelock); every other kind is always fresh, having
// named no track. A successful hand-off resets the matching format-clock
// counter — a stale discard does NOT (the break is still owed and re-preps
// against the new anchor).
func (dr *Director) Take(justFinished live.Entry) (live.Clip, bool) {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	if dr.slot == nil {
		return live.Clip{}, false
	}
	c := *dr.slot
	sp, _ := specFor(c.Kind)
	if sp.Anchored && !anchorFresh(c.AnchorYTID, c.AnchorStartedAt, justFinished) {
		dr.slot = nil
		_ = os.Remove(c.Path)
		dr.d.Logger.Info("stale seam discarded", "anchor_ytid", c.AnchorYTID, "finished_ytid", justFinished.YTID)
		return live.Clip{}, false
	}
	dr.slot = nil
	dr.cad = cadence.Advance(dr.cad, c.Kind, dr.d.Clock.Now())
	return c, true
}

// ForceBreak arms a seam break for the next wake tick, bypassing the CADENCE
// gate only - budget, listeners and on-air still apply, because spending
// money on a break nobody can hear is what the wake gates exist to prevent.
// Reports whether a break is now due; false means a clip was already
// prepared, so there was nothing to arm.
func (dr *Director) ForceBreak() bool {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	if dr.slot != nil {
		return false
	}
	dr.cad.Forced = true
	return true
}

// CancelPrepared discards a prepared-but-unaired clip AND clears an arming
// that has not been prepared yet. Reports whether either existed.
func (dr *Director) CancelPrepared() bool {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	had := dr.slot != nil || dr.cad.Forced
	dr.cancelPendingLocked("operator cancelled")
	return had
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
// Now and StationIDsAvailable are filled per evaluation rather than stored:
// one is the caller's instant, the other lives behind the ids mutex.
func (dr *Director) dueKindLocked(now time.Time, dj station.DJSettings) string {
	s := dr.cad
	s.Now = now
	s.StationIDsAvailable = dr.ids.available()
	return cadence.DueKind(s, dj)
}

// cancelPendingLocked discards a prepared-but-unaired clip (operator paused
// the DJ or the station went off-air). Caller holds mu.
func (dr *Director) cancelPendingLocked(reason string) {
	// Above the nil check on purpose: an arming with no clip yet is still
	// something an operator can cancel, and pause/off-air must disarm too.
	dr.cad.Forced = false
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

// RunOnce evaluates the wake gates (spec section 3) in order: on-air ->
// ai_enabled -> listeners>0 -> daily budget -> segment due -> slot empty;
// every failure is a quiet skip (music covers the air). Pause/off-air
// additionally cancel a pending clip; the off->on transition now restarts the
// whole format clock and owes a greeting, an AIEnabled off->on owes a
// greeting without restarting the session, and the daypart rollover is
// checked every tick while on air.
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
	// Going on air starts a new broadcast session. The counter must restart
	// with it: a seam owed from the last session would be anchored to that
	// session's last track and discarded by Take against the zero Entry.
	if st.OnAir && !dr.wasOnAir {
		dr.cad.LastStationID = now
		dr.cad.LastMusing = now
		dr.cad.Forced = false
		dr.cad.SessionHasMusic = false
		dr.cad.FinishedSinceSeam = 0
		dr.cad.PendingDaypart = time.Time{}
		dr.cad.PendingWake = true
		dr.lastDaypart = daypart(now.In(dr.d.Location).Hour())
		dr.daypartFrom = ""
		dr.silentForMin = 0 // she was not silent; the station was off
		dr.aiDisabledSince = time.Time{}
	}
	dr.wasOnAir = st.OnAir

	// An AIEnabled resume is NOT a new session: the station stayed on air and
	// the feeder kept calling TrackFinished. SessionHasMusic and the seam
	// counter are deliberately left alone - PendingWake outranks the owed seam
	// in the ladder and Advance resets the counter when the greeting airs, so
	// the ladder handles it with no special case.
	if st.AIEnabled && !dr.wasAIEnabled {
		dr.cad.PendingWake = true
		dr.cad.LastStationID = now
		dr.cad.LastMusing = now
		if !dr.aiDisabledSince.IsZero() {
			dr.silentForMin = int(now.Sub(dr.aiDisabledSince).Minutes())
			dr.aiDisabledSince = time.Time{}
		}
	}
	if !st.AIEnabled && dr.wasAIEnabled {
		dr.aiDisabledSince = now
	}
	dr.wasAIEnabled = st.AIEnabled

	if st.OnAir {
		if d := daypart(now.In(dr.d.Location).Hour()); dr.lastDaypart != "" && d != dr.lastDaypart {
			dr.cad.PendingDaypart = now
			dr.daypartFrom = dr.lastDaypart
			dr.lastDaypart = d
		} else {
			dr.lastDaypart = d
		}
	}
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
//   - It must not touch dr.cad.FinishedSinceSeam or dr.cad.LastStationID - a
//     read that mutates the format clock would silently change the cadence.
//   - Script is deliberately NOT copied here. live.Clip.Script is "logs only,
//     never published"; the aired script reaches the console from the stored
//     talk_segment row instead, which is admin-gated at the route.
func (dr *Director) Snapshot() Snapshot {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	s := Snapshot{
		Present:           true,
		FinishedSinceSeam: dr.cad.FinishedSinceSeam,
		LastStationID:     dr.cad.LastStationID,
		// ids has its own mutex and is never held while taking dr.mu, so this
		// is a len() under an uncontended lock — no I/O, no clock read.
		StationIDsAvailable: dr.ids.available(),
		Forced:              dr.cad.Forced,
		SessionHasMusic:     dr.cad.SessionHasMusic,
		LastMusing:          dr.cad.LastMusing,
		PendingDaypart:      dr.cad.PendingDaypart,
		PendingWake:         dr.cad.PendingWake,
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
