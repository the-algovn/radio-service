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
	queueDepthTarget = 3  // pending items (approved+ready, both sources)
	recentWindow     = 50 // AI-pick no-recent-air filter (air-log entries)
	// failedWindow is how many terminal requests are scanned for failures. A
	// track that failed to download will fail again; without this the
	// programmer re-picks it every tick forever.
	failedWindow    = 50
	maxTrackSeconds = 600 // spec §5: AI picks ≤ 10 min
	minTrackSeconds = 60
	briefPlays      = 10
	// searchN is deliberately 20, not 10: ytsearch issues one innertube POST for
	// the whole batch and only paginates beyond ~20, so doubling the depth costs
	// no extra request.
	searchN        = 20
	maxReasonRunes = 200
	retryBackoff   = 2 * time.Second
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
	// Wake only when the queue can absorb a FULL batch (maxPicks), not merely
	// when it has any room at all. wantPicks fills toward queueDepthTarget in
	// one decision, and it's that fill-to-target behaviour that halves how
	// often the programmer wakes — which is what keeps two LLM calls per
	// decision at roughly the old one-call daily cost. Gating on
	// `pending >= queueDepthTarget` instead would pass at pending==2 in
	// steady state (one track airs, one leaves the queue), where wantPicks
	// only asks for 1: two calls spent to enqueue one track, silently
	// doubling spend against the shared daily budget. Gating on
	// queueDepthTarget-maxPicks means the queue is allowed to drain to 1
	// before refilling; tracks are at least minTrackSeconds (60s) and the
	// tick is also 60s, so there's time to refill, and if it ever underruns,
	// shuffle covers the air by design.
	pending, err := p.d.Requests.Pending(ctx)
	if err != nil || len(pending) > queueDepthTarget-maxPicks {
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
		p.d.Logger.ErrorContext(ctx, "programmer: spend read failed", "err", err)
		return
	}
	if spent >= p.d.BudgetUSD {
		p.d.Logger.WarnContext(ctx, "programmer: daily budget reached; idling", "spent_usd", spent)
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
		p.d.Logger.ErrorContext(ctx, "programmer: persona load failed", "err", err)
		respinOnly()
		return
	}
	brief, err := p.buildBrief(ctx)
	if err != nil {
		p.d.Logger.ErrorContext(ctx, "programmer: brief failed", "err", err)
		respinOnly()
		return
	}
	briefJSON, err := json.Marshal(brief)
	if err != nil {
		p.d.Logger.ErrorContext(ctx, "programmer: brief marshal failed", "err", err)
		respinOnly()
		return
	}

	pool, ok := p.proposeAndResolve(ctx, pers, string(briefJSON))
	if !ok || len(pool) == 0 {
		respinOnly()
		return
	}

	// Phase 2 gets a trimmed brief: Library.Sample is redundant once the pool
	// exists (the candidates themselves are the concrete choices), so leaving
	// it in would ride ~25 library rows into phase 2 on top of the pool,
	// inflating input well past the design's sizing. Library.Total stays —
	// the shelf size is still useful context for the model.
	choiceBrief := brief
	choiceBrief.Library.Sample = nil
	choiceBriefJSON, err := json.Marshal(choiceBrief)
	if err != nil {
		p.d.Logger.ErrorContext(ctx, "programmer: phase-2 brief marshal failed", "err", err)
		respinOnly()
		return
	}

	// Clamp to what the pool can actually satisfy. wantPicks only knows the
	// queue depth, so against a thin pool it would instruct the model to choose
	// more tracks than exist — which invites an invented yt_id, dropped at
	// ParseChoice, at the cost of a billed repair turn.
	want := wantPicks(pending)
	if want > len(pool) {
		want = len(pool)
	}
	choices := p.chooseFrom(ctx, pers, string(choiceBriefJSON), pool, want)
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
		p.d.Logger.ErrorContext(ctx, "programmer: intent call failed", "err", err)
		return nil, false
	}
	in, err := ParseIntent(raw)
	if err != nil {
		p.d.Logger.ErrorContext(ctx, "programmer: intent parse failed", "err", err, "raw", clip(raw))
		return nil, false
	}
	if in.empty() {
		p.d.Logger.InfoContext(ctx, "programmer: no intent this decision", "note", in.Note)
		return nil, true
	}
	pool, err := p.resolve(ctx, in)
	if err != nil {
		p.d.Logger.ErrorContext(ctx, "programmer: resolve failed", "err", err)
		return nil, false
	}
	p.d.Logger.InfoContext(ctx, "programmer: pool resolved", "note", in.Note, "candidates", len(pool))
	return pool, true
}

// chooseFrom runs phase 2 with one repair turn. An empty result means the
// caller should fall through to respin().
func (p *Programmer) chooseFrom(ctx context.Context, pers, briefJSON string, pool []Candidate, want int) []Choice {
	poolJSON, err := json.Marshal(pool)
	if err != nil {
		p.d.Logger.ErrorContext(ctx, "programmer: pool marshal failed", "err", err)
		return nil
	}
	system, user := BuildChoosePrompts(pers, briefJSON, string(poolJSON), want)

	raw, err := p.generate(ctx, "programmer:choose", system, user, brain.ChoiceSchema)
	if err != nil {
		p.d.Logger.ErrorContext(ctx, "programmer: choose call failed", "err", err)
		return nil
	}
	choices, perr := ParseChoice(raw, pool, want)
	if perr == nil {
		return choices
	}

	// One repair turn, naming the violation.
	p.d.Logger.WarnContext(ctx, "programmer: choice invalid; repairing", "err", perr)
	raw2, err := p.generate(ctx, "programmer:repair", system, RepairUser(user, raw, perr.Error()), brain.ChoiceSchema)
	if err != nil {
		p.d.Logger.ErrorContext(ctx, "programmer: repair call failed", "err", err)
		return nil
	}
	choices, perr = ParseChoice(raw2, pool, want)
	if perr != nil {
		p.d.Logger.WarnContext(ctx, "programmer: choice still invalid after repair; falling back", "err", perr)
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
	p.d.Logger.WarnContext(ctx, "programmer: model call failed; retrying once", "label", label, "err", err)
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
		ThumbnailURL: cand.ThumbnailURL,
	}); err != nil {
		p.d.Logger.ErrorContext(ctx, "programmer: enqueue failed", "err", err)
		return false
	}
	p.d.Logger.InfoContext(ctx, "ai pick queued", "yt_id", cand.YTID, "reason", c.Reason, "from", cand.Source)
	return true
}

// respin is tier 3: a deterministic library re-spin through the same guards.
// It is used by keyless mode, an empty candidate pool, and an exhausted repair.
//
// It sets NO reason on purpose. Since v2 the DJ speaks pick reasons on air, so
// fabricating one here would put a lie in the DJ's mouth; an empty reason is
// already the convention for shuffle plays.
func (p *Programmer) respin(ctx context.Context) bool {
	g, err := p.buildGuards(ctx)
	if err != nil {
		return false
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
	if why, _ := p.classify(ctx, factsOfTrack(tr.YTID, int64(tr.DurationS), tr.SongKey), g); why != dropNone {
		return false
	}
	if _, err := p.d.Requests.Create(ctx, request.Item{
		Source: request.SourceAI, YTID: tr.YTID, Title: tr.Title, Channel: tr.Channel,
		DurationS: int64(tr.DurationS), Status: request.StatusReady,
	}); err != nil {
		return false
	}
	p.d.Logger.InfoContext(ctx, "ai respin queued", "yt_id", tr.YTID)
	return true
}

// clip bounds a raw model reply for logging. Rune-safe like capReason above —
// byte-slicing (s[:200]) can cut a multi-byte UTF-8 rune in half and log
// invalid UTF-8 at the boundary (the model replies in Vietnamese).
func clip(s string) string {
	r := []rune(s)
	if len(r) <= 200 {
		return s
	}
	return string(r[:200])
}
