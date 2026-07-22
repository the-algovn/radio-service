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

	"github.com/the-algovn/radio-service/internal/brain"
	"github.com/the-algovn/radio-service/internal/ingest"
	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/persona"
	"github.com/the-algovn/radio-service/internal/request"
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
	briefSample      = 10
	searchN          = 10
	maxReasonRunes   = 200
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
}

type Programmer struct{ d Deps }

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
		p.fakePick(ctx)
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

	brief, err := p.buildBrief(ctx)
	if err != nil {
		p.d.Logger.Error("programmer: brief failed", "err", err)
		return
	}
	pers, err := persona.Load(p.d.PersonaDir)
	if err != nil {
		p.d.Logger.Error("programmer: persona load failed", "err", err)
		return
	}
	briefJSON, err := json.Marshal(brief)
	if err != nil {
		p.d.Logger.Error("programmer: brief marshal failed", "err", err)
		return
	}
	system, user := BuildPrompts(pers, string(briefJSON))
	raw, usage, err := p.d.Model.Generate(ctx, system, user)
	if err != nil {
		p.d.Logger.Error("programmer: model call failed", "err", err)
		return
	}
	// Price the call BEFORE parsing — tokens were spent either way.
	cost := brain.CostUSD(p.d.Model.Name(), usage)
	if lerr := p.d.Ledger.Append(ctx, spend.Line{
		TS: time.Now(), Kind: "llm", Provider: p.d.Model.Name(), Label: "programmer:pick",
		InTokens: usage.In, OutTokens: usage.Out, CostUSD: cost,
	}); lerr != nil {
		p.d.Logger.Error("programmer: ledger append failed", "err", lerr)
	}
	picks, err := ParsePicks(raw)
	if err != nil {
		p.d.Logger.Error("programmer: parse failed", "err", err, "raw", raw[:min(len(raw), 200)])
		return
	}
	enqueued := 0
	for _, pk := range picks {
		if p.enqueue(ctx, pk) {
			enqueued++
		}
	}
	if enqueued > 0 {
		live.PublishQueueSnapshot(ctx, p.d.Producer, p.d.Requests, p.d.Logger)
	}
}

// enqueue turns one pick into an AI request row. Returns false when the
// pick is filtered out (recently aired, already queued, no usable
// candidate) — a quiet skip, never an error.
func (p *Programmer) enqueue(ctx context.Context, pk Pick) bool {
	recent, err := p.d.Log.RecentYTIDs(ctx, recentWindow)
	if err != nil {
		p.d.Logger.Error("programmer: recent read failed", "err", err)
		return false
	}
	recentSet := map[string]bool{}
	for _, id := range recent {
		recentSet[id] = true
	}

	if pk.YTID != "" { // library re-spin
		tr, ok, err := p.d.Library.Get(ctx, pk.YTID)
		if err != nil || !ok {
			return false
		}
		if skip, _ := p.filtered(ctx, tr.YTID, int64(tr.DurationS), recentSet); skip {
			return false
		}
		_, err = p.d.Requests.Create(ctx, request.Item{
			Source: request.SourceAI, YTID: tr.YTID, Title: tr.Title, Channel: tr.Channel,
			DurationS: int64(tr.DurationS), Status: request.StatusReady, Reason: capReason(pk.Reason),
		})
		if err != nil {
			p.d.Logger.Error("programmer: enqueue failed", "err", err)
			return false
		}
		p.d.Logger.Info("ai pick queued", "yt_id", tr.YTID, "reason", pk.Reason, "from", "library")
		return true
	}

	cs, err := p.d.Search.Search(ctx, pk.Query, searchN)
	if err != nil {
		p.d.Logger.Error("programmer: search failed", "query", pk.Query, "err", err)
		return false
	}
	for _, sc := range ingest.Rank(pk.Query, cs) {
		if sc.DurationS < minTrackSeconds || sc.DurationS > maxTrackSeconds {
			continue
		}
		if skip, err := p.filtered(ctx, sc.YTID, sc.DurationS, recentSet); skip || err != nil {
			continue
		}
		status := request.StatusApproved
		if _, cached, _ := p.d.Library.Get(ctx, sc.YTID); cached {
			status = request.StatusReady
		}
		_, err := p.d.Requests.Create(ctx, request.Item{
			Source: request.SourceAI, YTID: sc.YTID, Title: sc.Title, Channel: sc.Channel,
			DurationS: sc.DurationS, ThumbnailURL: sc.ThumbnailURL, Status: status, Reason: capReason(pk.Reason),
		})
		if err != nil {
			p.d.Logger.Error("programmer: enqueue failed", "err", err)
			return false
		}
		p.d.Logger.Info("ai pick queued", "yt_id", sc.YTID, "reason", pk.Reason, "query", pk.Query)
		return true
	}
	return false
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

// fakePick is keyless-mode programming (dev parity, prod degradation): no
// LLM, no spend — re-spin one random library track through the same
// filters, so the Tilt e2e can watch the AI DJ work for $0.
func (p *Programmer) fakePick(ctx context.Context) {
	recent, err := p.d.Log.RecentYTIDs(ctx, recentWindow)
	if err != nil {
		return
	}
	recentSet := map[string]bool{}
	for _, id := range recent {
		recentSet[id] = true
	}
	ids, err := p.d.Library.AllIDs(ctx)
	if err != nil || len(ids) == 0 {
		return
	}
	// one random probe per wake — enough for a demo, no retry loops
	id := ids[p.d.Rand(len(ids))]
	if skip, _ := p.filtered(ctx, id, minTrackSeconds, recentSet); skip {
		return
	}
	tr, ok, err := p.d.Library.Get(ctx, id)
	if err != nil || !ok {
		return
	}
	if int64(tr.DurationS) > maxTrackSeconds {
		return
	}
	if _, err := p.d.Requests.Create(ctx, request.Item{
		Source: request.SourceAI, YTID: tr.YTID, Title: tr.Title, Channel: tr.Channel,
		DurationS: int64(tr.DurationS), Status: request.StatusReady,
	}); err != nil {
		return
	}
	live.PublishQueueSnapshot(ctx, p.d.Producer, p.d.Requests, p.d.Logger)
}

// buildBrief assembles the model's data block: station-local time, recent
// plays, pending listener requests, and a random library sample.
func (p *Programmer) buildBrief(ctx context.Context) (Brief, error) {
	now := p.d.Clock.Now().In(p.d.Location)
	b := Brief{LocalTime: now.Format("Monday 15:04")}

	plays, err := p.d.Log.History(ctx, briefPlays)
	if err != nil {
		return Brief{}, err
	}
	for _, e := range plays {
		b.RecentPlays = append(b.RecentPlays, e.Title+" — "+e.Artist)
	}

	pending, err := p.d.Requests.Pending(ctx)
	if err != nil {
		return Brief{}, err
	}
	for _, it := range pending {
		if it.Source == request.SourceListener {
			b.RecentRequests = append(b.RecentRequests, it.Title)
		}
	}

	ids, err := p.d.Library.AllIDs(ctx)
	if err != nil {
		return Brief{}, err
	}
	for len(b.LibrarySample) < briefSample && len(ids) > 0 {
		i := p.d.Rand(len(ids))
		id := ids[i]
		ids = append(ids[:i], ids[i+1:]...)
		tr, ok, err := p.d.Library.Get(ctx, id)
		if err != nil {
			return Brief{}, err
		}
		if ok {
			b.LibrarySample = append(b.LibrarySample, BriefTrack{YTID: tr.YTID, Title: tr.Title, Channel: tr.Channel})
		}
	}
	return b, nil
}
