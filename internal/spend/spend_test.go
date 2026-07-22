package spend

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMemLedgerAppendAndAll(t *testing.T) {
	ctx := context.Background()
	l := NewMemLedger()
	require.NoError(t, l.Append(ctx, Line{TS: time.Now(), Kind: "tts", Provider: "google", Label: "take-1", Chars: 120, CostUSD: 0.0036}))
	require.NoError(t, l.Append(ctx, Line{TS: time.Now(), Kind: "llm", Provider: "gemini", Label: "intro", InTokens: 900, OutTokens: 220, CostUSD: 0.0008}))
	lines, err := l.All(ctx)
	require.NoError(t, err)
	require.Len(t, lines, 2)
	require.Equal(t, "google", lines[0].Provider)
	require.InDelta(t, 0.0044, Total(lines), 1e-9)
}

func TestMemLedgerSpentSince(t *testing.T) {
	l := NewMemLedger()
	ctx := context.Background()
	base := time.Date(2026, 7, 22, 0, 30, 0, 0, time.UTC)
	require.NoError(t, l.Append(ctx, Line{TS: base.Add(-time.Hour), Kind: "llm", Provider: "gemini", CostUSD: 0.40}))
	require.NoError(t, l.Append(ctx, Line{TS: base, Kind: "llm", Provider: "gemini", CostUSD: 0.25}))
	got, err := l.SpentSince(ctx, base.Add(-time.Minute))
	require.NoError(t, err)
	require.InDelta(t, 0.25, got, 1e-9)
	got, err = l.SpentSince(ctx, base.Add(-2*time.Hour))
	require.NoError(t, err)
	require.InDelta(t, 0.65, got, 1e-9)
}

func TestMemLedgerEmpty(t *testing.T) {
	lines, err := NewMemLedger().All(context.Background())
	require.NoError(t, err)
	require.Empty(t, lines)
}
