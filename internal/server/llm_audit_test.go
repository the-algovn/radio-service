package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	radiolabv1 "github.com/the-algovn/protos/gen/go/algovn/radiolab/v1"
	"github.com/the-algovn/radio-service/internal/audit"
)

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
