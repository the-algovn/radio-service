package audit_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"
	"github.com/the-algovn/radio-service/internal/audit"
	"github.com/the-algovn/radio-service/internal/brain"
	"github.com/the-algovn/radio-service/internal/spend"
)

type stubClock struct {
	t    time.Time
	seen []time.Time
}

func (c *stubClock) Now() time.Time {
	c.t = c.t.Add(150 * time.Millisecond)
	c.seen = append(c.seen, c.t)
	return c.t
}

func runInfo() *callbacks.RunInfo {
	return &callbacks.RunInfo{Name: "claude-haiku-4-5-20251001", Type: "Claude", Component: components.ComponentOfChatModel}
}

// The handler is the sole writer of audit.Rec and spend.Line now. Both shapes
// must match what audit.Wrap produced, or the shipped console inspector breaks.
func TestCallbackRecordsAuditAndSpend(t *testing.T) {
	store, ledger := audit.NewMemStore(), spend.NewMemLedger()
	clk := &stubClock{}
	h := audit.NewCallback(store, ledger, clk, slog.Default())

	ctx := audit.WithLabel(context.Background(), "programmer:choose")
	ctx = h.OnStart(ctx, runInfo(), &einomodel.CallbackInput{
		Messages: []*schema.Message{schema.SystemMessage("sys"), schema.UserMessage("usr")},
		// Config.Model, not RunInfo.Name, is what real traffic carries the
		// model id on (RunInfo.Name is always "" — see the OnStart comment).
		Config: &einomodel.Config{Model: "claude-haiku-4-5-20251001"},
	})
	h.OnEnd(ctx, runInfo(), &einomodel.CallbackOutput{
		Message:    schema.AssistantMessage(`{"picks":[]}`, nil),
		TokenUsage: &einomodel.TokenUsage{PromptTokens: 1200, CompletionTokens: 200},
	})

	recs, err := store.List(context.Background(), audit.Filter{}, 10, 0)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	r := recs[0]
	require.Equal(t, "programmer:choose", r.Label)
	require.Equal(t, "claude-haiku-4-5-20251001", r.Model)
	require.Equal(t, "anthropic", r.Provider, "RunInfo.Type must normalise to the console's provider label")
	require.Equal(t, "sys", r.System)
	require.Equal(t, "usr", r.User)
	require.Equal(t, `{"picks":[]}`, r.Output)
	require.Equal(t, 1200, r.InTokens)
	require.Equal(t, 200, r.OutTokens)
	require.Greater(t, r.CostUSD, 0.0)
	require.Greater(t, r.LatencyMS, 0, "latency must be measured across OnStart→OnEnd")
	require.Empty(t, r.Error)
	require.False(t, r.Fake)
	require.Equal(t, clk.seen[0], r.TS, "Rec.TS must be the OnStart instant, not the OnEnd one")

	lines, err := ledger.All(context.Background())
	require.NoError(t, err)
	require.Len(t, lines, 1)
	require.Equal(t, "llm", lines[0].Kind)
	require.Equal(t, "anthropic", lines[0].Provider)
	require.Equal(t, "programmer:choose", lines[0].Label)
	require.Equal(t, 1200, lines[0].InTokens)
	require.Equal(t, 200, lines[0].OutTokens)
	require.InDelta(t, r.CostUSD, lines[0].CostUSD, 1e-12)
	require.Equal(t, r.TS, lines[0].TS, "spend.Line.TS must match the Rec it was derived from")
}

// On error the Rec records the message and blanks Output, and no spend line
// is written — a failed call was never billed, matching every pre-Eino call
// site (Ledger.Append always sat after the `if err != nil { return }` check).
func TestCallbackRecordsError(t *testing.T) {
	store, ledger := audit.NewMemStore(), spend.NewMemLedger()
	clk := &stubClock{}
	h := audit.NewCallback(store, ledger, clk, slog.Default())

	ctx := audit.WithLabel(context.Background(), "programmer:intent")
	ctx = h.OnStart(ctx, runInfo(), &einomodel.CallbackInput{
		Messages: []*schema.Message{schema.SystemMessage("sys"), schema.UserMessage("usr")},
	})
	h.OnError(ctx, runInfo(), context.DeadlineExceeded)

	recs, err := store.List(context.Background(), audit.Filter{}, 10, 0)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.Equal(t, context.DeadlineExceeded.Error(), recs[0].Error)
	require.Empty(t, recs[0].Output)
	require.Equal(t, clk.seen[0], recs[0].TS, "Rec.TS must be the OnStart instant even on the error path")

	lines, err := ledger.All(context.Background())
	require.NoError(t, err)
	require.Empty(t, lines, "a failed call must not append spend")
}

// The fake provider must be flagged and free, so the inspector can exclude it.
// It must still append a zero-cost spend line — a successful call is always
// billed (even at $0), matching the pre-Eino call sites, which appended
// unconditionally after success regardless of token count.
func TestCallbackFlagsFakeProvider(t *testing.T) {
	store, ledger := audit.NewMemStore(), spend.NewMemLedger()
	h := audit.NewCallback(store, ledger, &stubClock{}, slog.Default())

	ri := &callbacks.RunInfo{Name: "fake", Type: "fake", Component: components.ComponentOfChatModel}
	ctx := audit.WithLabel(context.Background(), "director:backsell")
	ctx = h.OnStart(ctx, ri, &einomodel.CallbackInput{
		Messages: []*schema.Message{schema.SystemMessage("s"), schema.UserMessage("u")},
	})
	h.OnEnd(ctx, ri, &einomodel.CallbackOutput{
		Message:    schema.AssistantMessage("{}", nil),
		TokenUsage: &einomodel.TokenUsage{},
	})

	recs, _ := store.List(context.Background(), audit.Filter{}, 10, 0)
	require.Len(t, recs, 1)
	require.True(t, recs[0].Fake)
	require.Equal(t, "fake", recs[0].Provider)
	require.Zero(t, recs[0].CostUSD)

	lines, err := ledger.All(context.Background())
	require.NoError(t, err)
	require.Len(t, lines, 1, "a zero-cost fake call must still append a spend line, so call-volume history isn't silently dropped")
	require.Zero(t, lines[0].CostUSD)
}

// Real Eino traffic never sets RunInfo.Name (this plan's plain-Go
// orchestration has no compose.WithNodeName() call), so a handler that read
// the model id from info.Name would silently record a blank Model on every
// real call while still passing hand-constructed tests that set Name
// themselves. This test drives the handler with an actual brain.Model
// (brain.NewFake) through the real Eino callback plumbing
// (callbacks.InitCallbacks, ctx-scoped — not AppendGlobalHandlers, which would
// leak into sibling tests) with RunInfo.Name left blank, proving Rec.Model
// instead comes from the component's Config.
func TestCallbackRecordsModelFromRealComponentTraffic(t *testing.T) {
	store, ledger := audit.NewMemStore(), spend.NewMemLedger()
	h := audit.NewCallback(store, ledger, &stubClock{}, slog.Default())

	info := &callbacks.RunInfo{Type: "fake", Component: components.ComponentOfChatModel} // Name left blank, like real traffic
	ctx := callbacks.InitCallbacks(context.Background(), info, h)
	ctx = audit.WithLabel(ctx, "director:backsell")

	m := brain.NewFake(`{"script":"ok"}`)
	raw, err := m.Generate(ctx, "sys", "usr", nil)
	require.NoError(t, err)
	require.Equal(t, `{"script":"ok"}`, raw)

	recs, err := store.List(context.Background(), audit.Filter{}, 10, 0)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.NotEmpty(t, recs[0].Model, "Rec.Model must not be blank even though RunInfo.Name is")
	require.Equal(t, "fake", recs[0].Model, "Rec.Model must come from the component's Config, not the always-blank RunInfo.Name")
	require.True(t, recs[0].Fake)

	lines, err := ledger.All(context.Background())
	require.NoError(t, err)
	require.Len(t, lines, 1, "a successful real-component call must append a spend line")
}

// OnError has no output payload (unlike OnEnd, which carries the response's
// own Config), so the model id can only come from what OnStart stashed off
// the request's Config. This isolates that path: RunInfo.Name is blank (like
// real traffic) and only the input Config carries the model id.
func TestCallbackRecordsModelOnErrorFromStashedConfig(t *testing.T) {
	store, ledger := audit.NewMemStore(), spend.NewMemLedger()
	h := audit.NewCallback(store, ledger, &stubClock{}, slog.Default())

	info := &callbacks.RunInfo{Type: "Claude", Component: components.ComponentOfChatModel} // Name left blank, like real traffic
	ctx := audit.WithLabel(context.Background(), "programmer:intent")
	ctx = h.OnStart(ctx, info, &einomodel.CallbackInput{
		Messages: []*schema.Message{schema.SystemMessage("sys"), schema.UserMessage("usr")},
		Config:   &einomodel.Config{Model: "claude-haiku-4-5-20251001"},
	})
	h.OnError(ctx, info, context.DeadlineExceeded)

	recs, err := store.List(context.Background(), audit.Filter{}, 10, 0)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.Equal(t, "claude-haiku-4-5-20251001", recs[0].Model,
		"OnError has no output payload; Model must come from the Config stashed at OnStart, not the blank RunInfo.Name")
}
