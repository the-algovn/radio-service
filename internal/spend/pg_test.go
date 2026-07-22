//go:build integration

package spend_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/spend"
	"github.com/the-algovn/radio-service/internal/testutil"
)

func TestPGLedgerRoundTrip(t *testing.T) {
	url := testutil.StartPostgres(t)
	testutil.Migrate(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	defer pool.Close()

	l := spend.NewPGLedger(pool)
	require.NoError(t, l.Append(ctx, spend.Line{TS: time.Now(), Kind: "tts", Provider: "google", Label: "t", Chars: 100, CostUSD: 0.0036}))
	require.NoError(t, l.Append(ctx, spend.Line{TS: time.Now(), Kind: "llm", Provider: "gemini", Label: "s", InTokens: 900, OutTokens: 220, CostUSD: 0.0008}))

	lines, err := l.All(ctx)
	require.NoError(t, err)
	require.Len(t, lines, 2)
	require.InDelta(t, 0.0044, spend.Total(lines), 1e-9)

	sum, err := l.TotalCost(ctx)
	require.NoError(t, err)
	require.InDelta(t, 0.0044, sum, 1e-9)
}

func TestPGLedgerSpentSince(t *testing.T) {
	url := testutil.StartPostgres(t)
	testutil.Migrate(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	defer pool.Close()

	l := spend.NewPGLedger(pool)
	base := time.Date(2026, 7, 22, 0, 30, 0, 0, time.UTC)
	require.NoError(t, l.Append(ctx, spend.Line{TS: base.Add(-time.Hour), Kind: "llm", Provider: "gemini", CostUSD: 0.40}))
	require.NoError(t, l.Append(ctx, spend.Line{TS: base, Kind: "llm", Provider: "gemini", CostUSD: 0.25}))

	got, err := l.SpentSince(ctx, base.Add(-time.Minute))
	require.NoError(t, err)
	require.InDelta(t, 0.25, got, 1e-9)

	got, err = l.SpentSince(ctx, base.Add(-2*time.Hour))
	require.NoError(t, err)
	require.InDelta(t, 0.65, got, 1e-9)
}
