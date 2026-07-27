// Package programmer is the AI DJ (spec §5): when the station is on-air,
// listeners are tuned in, the queue is shallow and the daily budget has
// room, one persona-brief brain call picks what plays next — a YouTube
// search or a library re-spin — and enqueues it as an AI request.
package programmer

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand"
	"strings"
	"time"

	"github.com/eino-contrib/jsonschema"

	"github.com/the-algovn/radio-service/internal/audit"
	"github.com/the-algovn/radio-service/internal/brain"
	"github.com/the-algovn/radio-service/internal/ingest"
	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/persona"
	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/schedule"
	"github.com/the-algovn/radio-service/internal/spend"
	"github.com/the-algovn/radio-service/internal/station"
)

const (
	tickEvery        = 60 * time.Second
	queueDepthTarget = 3   // pending items (approved+ready, both sources)
	recentWindow     = 50  // AI-pick no-recent-air filter (air-log entries)
	maxTrackSeconds  = 600 // spec §5: AI picks ≤ 10 min
	minTrackSeconds  = 60
	briefPlays       = 10
	searchN          = 10
	maxReasonRunes   = 200
	retryBackoff     = 2 * time.Second
)

type Searcher interface {
	Search(ctx context.Context, query string, n int) ([]ingest.Candidate, error)
}

type Ledger interface {
	Append(ctx context.Context, line spend.Line) error
	SpentSince(ctx context.Context, since time.Time) (float64, error)
}

type Deps struct {
	Model      brain.Model
	Fake       bool // fake model wired: skip the LLM, re-spin one random library track
	PersonaDir string
	Station    station.Store
	Requests   request.Store
	Sched      schedule.Store
	Library    library.Library
	Log        live.AirLog
	Listeners  live.Listeners
	Search     Searcher
	Ledger     Ledger
	BudgetUSD  float64
	Producer   live.Producer // nil = feeds disabled
	Clock      live.Clock
	Rand       func(n int) int // nil → math/rand.Intn
	Location   *time.Location  // station civil clock; required
	Logger     *slog.Logger
	Backoff    time.Duration // model retry backoff; zero → retryBackoff
}

// cursor is the rotating library-window offset (see librarySample). It is only
// touched from buildBrief, which runs on the single programmer goroutine.
type Programmer struct {
	d      Deps
	cursor int
}

func New(d Deps) *Programmer {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Rand == nil {
		d.Rand = rand.Intn
	}
	if d.Location == nil {
		d.Location = time.UTC
	}
	if d.Backoff == 0 {
		d.Backoff = retryBackoff
	}
	return &Programmer{d: d}
}

// capReason trims and caps the model's stated reason — it is a UI string;
// the cap bounds every downstream payload (spec §3).
func capReason(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= maxReasonRunes {
		return s
	}
	return string(r[:maxReasonRunes])
}

func (p *Programmer) Run(ctx context.Context) error {
	tick := p.d.Clock.Tick(tickEvery)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
			p.RunOnce(ctx)
		}
	}
}

// RunOnce evaluates the wake gates (spec §5) and, when all pass, makes one
// programming decision. Every early return is a quiet skip — shuffle
// covers the air.
func (p *Programmer) RunOnce(ctx context.Context) {
	st, err := p.d.Station.GetStation(ctx)
	if err != nil || !st.OnAir {
		return
	}
	if !st.AIEnabled {
		return // operator paused the DJ (v1.2) — requests and shuffle unaffected
	}
	n, err := p.d.Listeners.Count(ctx)
	if err != nil || n == 0 {
		return
	}
	pending, err := p.d.Requests.Pending(ctx)
	if err != nil || len(pending) >= queueDepthTarget {
		return
	}
	if p.d.Fake {
		// Keyless mode: no LLM, no spend — the same deterministic re-spin the
		// failure ladder ends in.
		if p.respin(ctx) {
			live.PublishQueueSnapshot(ctx, p.d.Producer, p.d.Requests, p.d.Sched, p.d.Logger)
		}
		return
	}
	spent, err := p.d.Ledger.SpentSince(ctx, request.DayStart(p.d.Clock.Now(), p.d.Location))
	if err != nil {
		p.d.Logger.Error("programmer: spend read failed", "err", err)
		return
	}
	if spent >= p.d.BudgetUSD {
		p.d.Logger.Warn("programmer: daily budget reached; idling", "spent_usd", spent)
		return
	}
	p.decide(ctx, len(pending))
}

// decide makes one whole programming decision: phase 1 proposes intent, resolve
// turns it into real candidates, phase 2 chooses from them and writes the
// reason for the track it actually saw. Every exit path programs something —
// the ladder ends in respin(), never in a silent skip.
func (p *Programmer) decide(ctx context.Context, pending int) {
	enqueued := 0
	defer func() {
		if enqueued > 0 {
			live.PublishQueueSnapshot(ctx, p.d.Producer, p.d.Requests, p.d.Sched, p.d.Logger)
		}
	}()
	// respinOnly is every exit path's landing spot below: whatever went
	// wrong, the ladder still ends in a deterministic re-spin rather than a
	// silent skip. respin needs neither the persona nor the brief, so it is
	// reachable from every failure, not just the LLM-facing ones.
	respinOnly := func() {
		if p.respin(ctx) {
			enqueued++
		}
	}

	pers, err := persona.Load(p.d.PersonaDir)
	if err != nil {
		p.d.Logger.Error("programmer: persona load failed", "err", err)
		respinOnly()
		return
	}
	brief, err := p.buildBrief(ctx)
	if err != nil {
		p.d.Logger.Error("programmer: brief failed", "err", err)
		respinOnly()
		return
	}
	briefJSON, err := json.Marshal(brief)
	if err != nil {
		p.d.Logger.Error("programmer: brief marshal failed", "err", err)
		respinOnly()
		return
	}

	pool, ok := p.proposeAndResolve(ctx, pers, string(briefJSON))
	if !ok || len(pool) == 0 {
		respinOnly()
		return
	}

	choices := p.chooseFrom(ctx, pers, string(briefJSON), pool, wantPicks(pending))
	if len(choices) == 0 {
		respinOnly()
		return
	}
	for _, c := range choices {
		if p.enqueueChoice(ctx, c) {
			enqueued++
		}
	}
	if enqueued == 0 {
		respinOnly()
	}
}

// proposeAndResolve runs phase 1 and resolve. ok is false only when the
// decision cannot continue at all.
func (p *Programmer) proposeAndResolve(ctx context.Context, pers, briefJSON string) ([]Candidate, bool) {
	system, user := BuildIntentPrompts(pers, briefJSON)
	raw, err := p.generate(ctx, "programmer:intent", system, user, brain.IntentSchema)
	if err != nil {
		p.d.Logger.Error("programmer: intent call failed", "err", err)
		return nil, false
	}
	in, err := ParseIntent(raw)
	if err != nil {
		p.d.Logger.Error("programmer: intent parse failed", "err", err, "raw", clip(raw))
		return nil, false
	}
	if in.empty() {
		p.d.Logger.Info("programmer: no intent this decision", "note", in.Note)
		return nil, true
	}
	pool, err := p.resolve(ctx, in)
	if err != nil {
		p.d.Logger.Error("programmer: resolve failed", "err", err)
		return nil, false
	}
	p.d.Logger.Info("programmer: pool resolved", "note", in.Note, "candidates", len(pool))
	return pool, true
}

// chooseFrom runs phase 2 with one repair turn. An empty result means the
// caller should fall through to respin().
func (p *Programmer) chooseFrom(ctx context.Context, pers, briefJSON string, pool []Candidate, want int) []Choice {
	poolJSON, err := json.Marshal(pool)
	if err != nil {
		p.d.Logger.Error("programmer: pool marshal failed", "err", err)
		return nil
	}
	system, user := BuildChoosePrompts(pers, briefJSON, string(poolJSON), want)

	raw, err := p.generate(ctx, "programmer:choose", system, user, brain.ChoiceSchema)
	if err != nil {
		p.d.Logger.Error("programmer: choose call failed", "err", err)
		return nil
	}
	choices, perr := ParseChoice(raw, pool, want)
	if perr == nil {
		return choices
	}

	// One repair turn, naming the violation.
	p.d.Logger.Warn("programmer: choice invalid; repairing", "err", perr)
	raw2, err := p.generate(ctx, "programmer:repair", system, RepairUser(user, raw, perr.Error()), brain.ChoiceSchema)
	if err != nil {
		p.d.Logger.Error("programmer: repair call failed", "err", err)
		return nil
	}
	choices, perr = ParseChoice(raw2, pool, want)
	if perr != nil {
		p.d.Logger.Warn("programmer: choice still invalid after repair; falling back", "err", perr)
		return nil
	}
	return choices
}

// generate is tier 1 of the failure ladder: one retry after a fixed backoff.
// Errors are not classified — the call is idempotent and cheap, so retrying any
// failure once is simpler and no less correct than picking apart provider SDK
// error types.
func (p *Programmer) generate(ctx context.Context, label, system, user string, s *jsonschema.Schema) (string, error) {
	ctx = audit.WithLabel(ctx, label)
	raw, err := p.d.Model.Generate(ctx, system, user, s)
	if err == nil {
		return raw, nil
	}
	p.d.Logger.Warn("programmer: model call failed; retrying once", "label", label, "err", err)
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(p.d.Backoff):
	}
	return p.d.Model.Generate(ctx, system, user, s)
}

// enqueueChoice writes one validated choice to the request queue. The choice
// already carries its resolved Candidate, so there is nothing to look up and
// nothing to fail.
func (p *Programmer) enqueueChoice(ctx context.Context, c Choice) bool {
	cand := c.Candidate
	status := request.StatusApproved
	if cand.Cached {
		status = request.StatusReady
	}
	if _, err := p.d.Requests.Create(ctx, request.Item{
		Source: request.SourceAI, YTID: cand.YTID, Title: cand.Title, Channel: cand.Channel,
		DurationS: cand.DurationS, Status: status, Reason: c.Reason,
	}); err != nil {
		p.d.Logger.Error("programmer: enqueue failed", "err", err)
		return false
	}
	p.d.Logger.Info("ai pick queued", "yt_id", cand.YTID, "reason", c.Reason, "from", cand.Source)
	return true
}

// filtered reports whether ytID must be skipped: recently aired, already
// queued, or out of duration bounds.
func (p *Programmer) filtered(ctx context.Context, ytID string, durationS int64, recent map[string]bool) (bool, error) {
	if durationS < minTrackSeconds || durationS > maxTrackSeconds {
		return true, nil
	}
	if recent[ytID] {
		return true, nil
	}
	queued, err := p.d.Requests.HasPendingYTID(ctx, ytID)
	if err != nil {
		return true, err
	}
	return queued, nil
}

// respin is tier 3: a deterministic library re-spin through the same filters.
// It is used by keyless mode, an empty candidate pool, and an exhausted repair.
//
// It sets NO reason on purpose. Since v2 the DJ speaks pick reasons on air, so
// fabricating one here would put a lie in the DJ's mouth; an empty reason is
// already the convention for shuffle plays.
func (p *Programmer) respin(ctx context.Context) bool {
	recent, err := p.d.Log.RecentYTIDs(ctx, recentWindow)
	if err != nil {
		return false
	}
	recentSet := map[string]bool{}
	for _, id := range recent {
		recentSet[id] = true
	}
	ids, err := p.d.Library.AllIDs(ctx)
	if err != nil || len(ids) == 0 {
		return false
	}
	// one random probe per decision — enough, and no retry loops
	id := ids[p.d.Rand(len(ids))]
	tr, ok, err := p.d.Library.Get(ctx, id)
	if err != nil || !ok {
		return false
	}
	if skip, _ := p.filtered(ctx, tr.YTID, int64(tr.DurationS), recentSet); skip {
		return false
	}
	if _, err := p.d.Requests.Create(ctx, request.Item{
		Source: request.SourceAI, YTID: tr.YTID, Title: tr.Title, Channel: tr.Channel,
		DurationS: int64(tr.DurationS), Status: request.StatusReady,
	}); err != nil {
		return false
	}
	p.d.Logger.Info("ai respin queued", "yt_id", tr.YTID)
	return true
}

func clip(s string) string {
	if len(s) <= 200 {
		return s
	}
	return s[:200]
}
