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
	runStoreContract(t, func(t *testing.T) showlog.Store {
		// 0 retention = never prune, so the contract's timings are its own.
		return showlog.NewPGStore(newPGPool(t), 0)
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
