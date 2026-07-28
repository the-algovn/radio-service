package audit_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	claude "github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"
	"github.com/the-algovn/radio-service/internal/audit"
	"github.com/the-algovn/radio-service/internal/spend"
)

// TestCallbackPricesRealClaudeComponentTraffic closes the gap an earlier
// review left open: TestCallbackRecordsAuditAndSpend (below) proves CostUSD >
// 0, but only by hand-building a RunInfo{Name: ...} and a CallbackInput with
// no Config — a shape real traffic never produces (RunInfo.Name is always ""
// in this codebase; see the comment on callbackHandler.OnStart). Nothing
// previously exercised Config.Model -> brain.CostUSD -> non-zero through the
// actual Eino plumbing, so a provider-binding upgrade that changed how
// Config/TokenUsage are populated could silently zero out the budget cap
// again with every test still green.
//
// This test drives the REAL eino-ext Claude ChatModel (not brain.NewFake,
// which the existing real-component test uses, and whose cost is always
// skipped since Fake==true) against an httptest Anthropic server, through
// callbacks.InitCallbacks (never AppendGlobalHandlers, which would leak into
// sibling tests) — the same wiring production uses, with RunInfo.Name left
// blank like real traffic.
//
// It lives in internal/audit rather than internal/brain/roundtrip_test.go
// (which already stands up an equivalent httptest Claude server) because
// internal/audit imports internal/brain for CostUSD: a brain-package test
// file importing internal/audit is a real import cycle (verified: "import
// cycle not allowed in test"). An external `package brain_test` file doesn't
// cycle, but can't reach brain's unexported newClaudeBase (the only way to
// point the SDK at a test server), so the httptest Claude wiring is
// duplicated here instead of reused, matching this file's suggested fallback.
func TestCallbackPricesRealClaudeComponentTraffic(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = b
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":     []any{map[string]any{"type": "text", "text": `{"picks":[]}`}},
			"usage":       map[string]any{"input_tokens": 1200, "output_tokens": 200},
			"stop_reason": "end_turn",
		})
	}))
	defer ts.Close()

	baseURL := ts.URL
	cm, err := claude.NewChatModel(context.Background(), &claude.Config{
		APIKey: "k", Model: "claude-haiku-4-5-20251001", MaxTokens: 2048, BaseURL: &baseURL,
	})
	require.NoError(t, err)

	store, ledger := audit.NewMemStore(), spend.NewMemLedger()
	h := audit.NewCallback(store, ledger, &stubClock{}, nil)

	// Name left blank on purpose: real traffic never sets it (no
	// compose.WithNodeName() call anywhere in this codebase's orchestration).
	info := &callbacks.RunInfo{Type: cm.GetType(), Component: components.ComponentOfChatModel}
	ctx := callbacks.InitCallbacks(context.Background(), info, h)
	ctx = audit.WithLabel(ctx, "programmer:choose")

	_, err = cm.Generate(ctx, []*schema.Message{schema.SystemMessage("sys"), schema.UserMessage("usr")})
	require.NoError(t, err)

	recs, err := store.List(context.Background(), audit.Filter{}, 10, 0)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.Equal(t, "claude-haiku-4-5-20251001", recs[0].Model,
		"Model must come from the component's own Config, not a hand-built RunInfo.Name")
	require.Equal(t, "anthropic", recs[0].Provider)
	require.Greater(t, recs[0].CostUSD, 0.0, "a real, paying call must be priced non-zero")
	require.False(t, recs[0].Fake)

	lines, err := ledger.All(context.Background())
	require.NoError(t, err)
	require.Len(t, lines, 1)
	require.InDelta(t, recs[0].CostUSD, lines[0].CostUSD, 1e-12,
		"the spend line appended for this call must carry the same non-zero cost")
}
