package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	radiolabv1 "github.com/the-algovn/protos/gen/go/algovn/radiolab/v1"
	"github.com/the-algovn/radio-service/internal/audit"
	"github.com/the-algovn/radio-service/internal/brain"
	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/spend"
)

// recAuditStore wraps a MemStore to record the paging/window arguments the
// handlers pass through, and to optionally inject a store failure.
type recAuditStore struct {
	*audit.MemStore
	lastLimit, lastOffset int
	lastSince             time.Time
	failErr               error
}

func (r *recAuditStore) List(ctx context.Context, f audit.Filter, limit, offset int) ([]audit.Rec, error) {
	r.lastLimit, r.lastOffset = limit, offset
	if r.failErr != nil {
		return nil, r.failErr
	}
	return r.MemStore.List(ctx, f, limit, offset)
}

func (r *recAuditStore) Count(ctx context.Context, f audit.Filter) (int64, error) {
	if r.failErr != nil {
		return 0, r.failErr
	}
	return r.MemStore.Count(ctx, f)
}

func (r *recAuditStore) Stats(ctx context.Context, since time.Time) ([]audit.Stat, error) {
	r.lastSince = since
	if r.failErr != nil {
		return nil, r.failErr
	}
	return r.MemStore.Stats(ctx, since)
}

func seedAudit(t *testing.T) *audit.MemStore {
	t.Helper()
	ctx := context.Background()
	m := audit.NewMemStore()
	now := time.Now()
	require.NoError(t, m.Record(ctx, audit.Rec{TS: now.Add(-2 * time.Minute), Label: "director:backsell", Model: "gemini-2.5-flash", Provider: "gemini", System: "s1", User: "u1", Output: "o1", InTokens: 100, OutTokens: 40, CostUSD: 0.001, LatencyMS: 800}))
	require.NoError(t, m.Record(ctx, audit.Rec{TS: now.Add(-time.Minute), Label: "programmer:pick", Model: "gemini-2.5-flash", Provider: "gemini", System: "s2", User: "u2", Error: "boom", InTokens: 200, CostUSD: 0.002}))
	require.NoError(t, m.Record(ctx, audit.Rec{TS: now, Label: "director:backsell", Model: "claude-x", Provider: "anthropic", System: "s3", User: "u3", Output: "o3", InTokens: 300, OutTokens: 60, CostUSD: 0.004}))
	return m
}

func TestListLLMCalls(t *testing.T) {
	s := New(Deps{Audit: seedAudit(t)})
	ctx := context.Background()

	resp, err := s.ListLLMCalls(ctx, &radiolabv1.ListLLMCallsRequest{})
	require.NoError(t, err)
	require.Equal(t, int64(3), resp.GetTotal())
	require.Len(t, resp.GetCalls(), 3)
	require.Equal(t, "director:backsell", resp.GetCalls()[0].GetLabel()) // newest first
	require.Equal(t, "o3", resp.GetCalls()[0].GetOutput())
	require.Equal(t, "anthropic", resp.GetCalls()[0].GetProvider())
	require.NotZero(t, resp.GetCalls()[0].GetId())

	byLabel, err := s.ListLLMCalls(ctx, &radiolabv1.ListLLMCallsRequest{Label: "director:backsell"})
	require.NoError(t, err)
	require.Equal(t, int64(2), byLabel.GetTotal())

	errs, err := s.ListLLMCalls(ctx, &radiolabv1.ListLLMCallsRequest{ErrorsOnly: true})
	require.NoError(t, err)
	require.Len(t, errs.GetCalls(), 1)
	require.Equal(t, "boom", errs.GetCalls()[0].GetError())

	// limit clamps to a sane default when unset (3 rows still returned since <=20)
	require.Len(t, resp.GetCalls(), 3)
}

func TestGetLLMStats(t *testing.T) {
	s := New(Deps{Audit: seedAudit(t)})
	resp, err := s.GetLLMStats(context.Background(), &radiolabv1.GetLLMStatsRequest{WindowDays: 1})
	require.NoError(t, err)
	require.Len(t, resp.GetStats(), 3) // (director,gemini),(programmer,gemini),(director,claude-x)
	require.InDelta(t, 0.007, resp.GetTotalUsd(), 1e-9)
}

func TestListLLMCallsClampsPaging(t *testing.T) {
	rec := &recAuditStore{MemStore: seedAudit(t)}
	s := New(Deps{Audit: rec})
	ctx := context.Background()

	_, err := s.ListLLMCalls(ctx, &radiolabv1.ListLLMCallsRequest{Limit: 0})
	require.NoError(t, err)
	require.Equal(t, 20, rec.lastLimit)

	_, err = s.ListLLMCalls(ctx, &radiolabv1.ListLLMCallsRequest{Limit: 500})
	require.NoError(t, err)
	require.Equal(t, 100, rec.lastLimit)

	_, err = s.ListLLMCalls(ctx, &radiolabv1.ListLLMCallsRequest{Offset: -5})
	require.NoError(t, err)
	require.Equal(t, 0, rec.lastOffset)
}

func TestListLLMCallsLabelFilterReturnsMatchingCalls(t *testing.T) {
	s := New(Deps{Audit: seedAudit(t)})
	ctx := context.Background()

	resp, err := s.ListLLMCalls(ctx, &radiolabv1.ListLLMCallsRequest{Label: "director:backsell"})
	require.NoError(t, err)
	require.Equal(t, int64(2), resp.GetTotal())
	require.Len(t, resp.GetCalls(), 2)
	for _, c := range resp.GetCalls() {
		require.Equal(t, "director:backsell", c.GetLabel())
	}
}

func TestGetLLMStatsDefaultWindow(t *testing.T) {
	rec := &recAuditStore{MemStore: seedAudit(t)}
	s := New(Deps{Audit: rec})

	_, err := s.GetLLMStats(context.Background(), &radiolabv1.GetLLMStatsRequest{WindowDays: 0})
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(-30*24*time.Hour), rec.lastSince, time.Minute)
}

func TestLLMAuditStoreErrors(t *testing.T) {
	rec := &recAuditStore{MemStore: audit.NewMemStore(), failErr: errors.New("db down")}
	s := New(Deps{Audit: rec})
	ctx := context.Background()

	_, err := s.ListLLMCalls(ctx, &radiolabv1.ListLLMCallsRequest{})
	require.Error(t, err)
	require.Equal(t, codes.Internal, status.Code(err))

	_, err = s.GetLLMStats(ctx, &radiolabv1.GetLLMStatsRequest{})
	require.Error(t, err)
	require.Equal(t, codes.Internal, status.Code(err))
}

func TestGenerateScriptAuditsWithLabel(t *testing.T) {
	ctx := context.Background()
	store := audit.NewMemStore()
	wrapped := audit.Wrap(brain.Fake{}, store, "fake", live.RealClock(), nil)
	s := New(Deps{
		Audit:        store,
		Ledger:       spend.NewMemLedger(), // GenerateScript always calls s.ledger(); a nil Ledger panics
		Models:       map[string]brain.Model{"fake": wrapped},
		DefaultModel: "fake",
	})

	_, err := s.GenerateScript(ctx, &radiolabv1.GenerateScriptRequest{
		Brief:           &radiolabv1.Brief{Type: "backsell"},
		PersonaOverride: "# test persona",
	})
	require.NoError(t, err)
	recs, err := store.List(ctx, audit.Filter{}, 10, 0)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.Equal(t, "script:backsell", recs[0].Label)
	require.True(t, recs[0].Fake)
}
