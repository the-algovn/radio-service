//go:build integration

package showlog_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/showlog"
	"github.com/the-algovn/radio-service/internal/testutil"
)

func newPGPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := testutil.StartPostgres(t)
	testutil.Migrate(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func TestPGStoreContract(t *testing.T) {
	runStoreContract(t, func(t *testing.T, music []showlog.Segment) showlog.Store {
		pool := newPGPool(t)
		for _, m := range music {
			_, err := pool.Exec(context.Background(),
				`INSERT INTO air_log (yt_id, title, artist, started_at, duration_s)
				 VALUES ($1, $2, $3, $4, $5)`,
				m.YTID, m.Title, m.Artist, m.StartedAt, m.DurationS)
			require.NoError(t, err)
		}
		// 0 retention = never prune, so the contract's timings are its own.
		return showlog.NewPGStore(pool, 0)
	})
}

func TestPGStorePrunesOnWrite(t *testing.T) {
	ctx := context.Background()
	pool := newPGPool(t)

	_, err := pool.Exec(ctx,
		`INSERT INTO talk_segment (kind, started_at, duration_s)
		 VALUES ('seam', now() - interval '60 days', 30)`)
	require.NoError(t, err)

	s := showlog.NewPGStore(pool, 30*24*time.Hour)
	require.NoError(t, s.Append(ctx, showlog.Talk{
		Kind: showlog.KindSeam, StartedAt: time.Now(), DurationS: 30,
	}))

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM talk_segment`).Scan(&n))
	require.Equal(t, 1, n, "the 60-day-old row should have been pruned on write")
}

func TestPGRecentSumsRetriedCallsAndDoesNotDuplicate(t *testing.T) {
	// generateValid retries once, so one prepare can leave TWO llm_call rows
	// with the same correlation id. A plain LEFT JOIN would emit the segment
	// twice; the lateral aggregate must emit it ONCE with summed cost.
	ctx := context.Background()
	pool := newPGPool(t)
	at := time.Date(2026, 8, 4, 21, 0, 0, 0, time.UTC)

	for _, cost := range []float64{0.001, 0.002} {
		_, err := pool.Exec(ctx,
			`INSERT INTO llm_call (ts, label, model, provider, cost_usd, in_tokens, out_tokens, correlation_id)
			 VALUES ($1, 'director:seam', 'gemini-2.5-flash', 'gemini', $2, 100, 20, 'corr-x')`,
			at, cost)
		require.NoError(t, err)
	}

	s := showlog.NewPGStore(pool, 0)
	require.NoError(t, s.Append(ctx, showlog.Talk{
		Kind: showlog.KindSeam, StartedAt: at, DurationS: 30, CorrelationID: "corr-x",
	}))

	got, err := s.Recent(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, got, 1, "two llm_call rows must not duplicate the segment")
	require.InDelta(t, 0.003, got[0].CostUSD, 1e-9, "cost is the sum of every attempt")
	require.Equal(t, 200, got[0].InTokens)
	require.Equal(t, "gemini-2.5-flash", got[0].Model)
}
