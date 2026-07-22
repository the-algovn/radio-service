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
	"github.com/the-algovn/radio-service/internal/spend"
	"github.com/the-algovn/radio-service/internal/station"
	"github.com/the-algovn/radio-service/internal/voice"
)

const (
	// tickEvery is deliberately faster than the programmer's 60s — the
	// director must notice "break due" within one track.
	tickEvery = 20 * time.Second
	// prepDeadline bounds one whole prepare attempt (the provider clients
	// carry their own 25s HTTP timeouts); a timeout is an ordinary failure.
	prepDeadline = 60 * time.Second
	ringCap      = 5
	teaserCap    = 3
)

// Ledger is the director's consumer-side ledger contract (the exported
// spend.Ledger lacks SpentSince; PGLedger/MemLedger both satisfy this —
// same pattern as programmer.Ledger).
type Ledger interface {
	Append(ctx context.Context, line spend.Line) error
	SpentSince(ctx context.Context, since time.Time) (float64, error)
}

type Deps struct {
	Model     brain.Model
	Voice     voice.Provider
	VoiceFake bool // zero the tts ledger cost, mirroring server.SynthesizeVoice
	Ledger    Ledger
	Station   station.Store
	Listeners live.Listeners
	AirLog    live.AirLog
	Requests  request.Store // queue teasers for the brief

	PersonaDir     string
	StationIDsPath string
	DataDir        string // clip scratch dir (LAB_DATA_DIR/dj), swept at boot by main

	BudgetUSD    float64
	VoiceID      string
	Rate         float64
	BreakEvery   int // backsell due after N tracks; 0 disables backsells
	StationIDMin int // minutes between station IDs; 0 disables
	MaxChars     int

	Render   RenderFunc // nil → FFRender
	Clock    live.Clock
	Location *time.Location
	Logger   *slog.Logger
}

type memEntry struct {
	summary string
	phrases []string
}

// Director prepares talk breaks ahead of air and hands them to the feeder
// through the live.TalkSource seam. One goroutine (Run) prepares; the feeder
// goroutine calls Take/TrackFinished; everything shared sits under mu.
type Director struct {
	d   Deps
	ids *stationIDs
	seq atomic.Int64 // clip filename counter (deterministic in tests)

	mu                    sync.Mutex
	slot                  *live.Clip
	finishedSinceBacksell int
	lastStationID         time.Time
	wasOnAir              bool
	ring                  []memEntry // last 5 backsell summaries+phrases
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
	if d.Rate <= 0 {
		d.Rate = 1.0
	}
	if d.MaxChars <= 0 {
		d.MaxChars = 450
	}
	return &Director{d: d, ids: loadStationIDs(d.StationIDsPath, d.MaxChars, d.Logger)}
}

// TrackFinished advances the format clock: called by the feeder once per
// announced music item (never for talk clips).
func (dr *Director) TrackFinished(_ live.Entry) {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	dr.finishedSinceBacksell++
}

// Take hands over the prepared clip, if any. Never blocks. Staleness is
// owned here: a backsell whose anchor is not the entry that just finished is
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
	if c.Kind == live.ClipBacksell && !anchorFresh(c.AnchorYTID, c.AnchorStartedAt, justFinished) {
		dr.slot = nil
		_ = os.Remove(c.Path)
		dr.d.Logger.Info("stale backsell discarded", "anchor_ytid", c.AnchorYTID, "finished_ytid", justFinished.YTID)
		return live.Clip{}, false
	}
	dr.slot = nil
	if c.Kind == live.ClipStationID {
		dr.lastStationID = dr.d.Clock.Now()
	} else {
		dr.finishedSinceBacksell = 0
	}
	return c, true
}

// anchorFreshTolerance bounds the StartedAt comparison in anchorFresh.
const anchorFreshTolerance = time.Second

// anchorFresh reports whether the clip was prepared against the entry that
// just finished. StartedAt is compared with a 1s tolerance: the anchor
// round-trips through Postgres TIMESTAMPTZ (microsecond precision) while the
// feeder's entry carries the sample clock's nanoseconds — exact equality
// would discard every backsell on a PG-backed deployment. Two airings of the
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
// station_id wins when both are due; the backsell counter carries over and
// stays due. The +1 counts the currently-airing track — the one the break
// will describe — so the default cadence is truly every BreakEvery tracks.
func (dr *Director) dueKindLocked(now time.Time) string {
	if dr.d.StationIDMin > 0 && dr.ids.available() &&
		now.Sub(dr.lastStationID) >= time.Duration(dr.d.StationIDMin)*time.Minute {
		return live.ClipStationID
	}
	if dr.d.BreakEvery > 0 && dr.finishedSinceBacksell+1 >= dr.d.BreakEvery {
		return live.ClipBacksell
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

// pushRing records one backsell's show memory (station_id never touches it).
func (dr *Director) pushRing(summary string, phrases []string) {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	dr.ring = append(dr.ring, memEntry{summary: summary, phrases: phrases})
	if len(dr.ring) > ringCap {
		dr.ring = dr.ring[len(dr.ring)-ringCap:]
	}
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
		dr.d.Logger.Error("director: station read failed", "err", err)
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
		dr.d.Logger.Error("director: spend read failed", "err", err)
		return
	}
	if spent >= dr.d.BudgetUSD {
		dr.d.Logger.Warn("director: daily budget reached; idling", "spent_usd", spent)
		return
	}

	dr.mu.Lock()
	kind := ""
	if dr.slot == nil {
		kind = dr.dueKindLocked(now)
	}
	dr.mu.Unlock()
	if kind == "" {
		return
	}

	clip, ok := dr.prepare(ctx, kind)
	if !ok {
		return
	}
	dr.mu.Lock()
	dr.slot = &clip
	dr.mu.Unlock()
}
