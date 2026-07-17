//go:build integration

package migrate_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/testutil"
)

func TestMigrateFreshIsIdempotent(t *testing.T) {
	url := testutil.StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	testutil.Migrate(t, url)
	testutil.Migrate(t, url) // second run: nothing pending, must not error

	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	defer pool.Close()

	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'ledger_line'`).Scan(&n))
	require.Equal(t, 1, n, "ledger_line must exist after migrate")
}
