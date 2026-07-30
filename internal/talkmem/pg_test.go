//go:build integration

package talkmem_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/talkmem"
	"github.com/the-algovn/radio-service/internal/testutil"
)

func newPGStore(t *testing.T) talkmem.Store {
	t.Helper()
	url := testutil.StartPostgres(t)
	testutil.Migrate(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	// 0 retention = never prune, so the contract suite's timings are its own.
	return talkmem.NewPGStore(pool, 0)
}

func TestPGStoreContract(t *testing.T) {
	runStoreContract(t, newPGStore)
}

func TestPGStorePrunesOnWrite(t *testing.T) {
	url := testutil.StartPostgres(t)
	testutil.Migrate(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// An entry backdated well past the retention window.
	_, err = pool.Exec(ctx,
		`INSERT INTO talk_memory (kind, summary, created_at)
		 VALUES ('seam', 'cũ', now() - interval '30 days')`)
	require.NoError(t, err)

	s := talkmem.NewPGStore(pool, 7*24*time.Hour)
	require.NoError(t, s.Append(ctx, talkmem.Entry{Kind: "seam", Summary: "mới"}))

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM talk_memory`).Scan(&n))
	require.Equal(t, 1, n, "the 30-day-old row should have been pruned on write")
}
