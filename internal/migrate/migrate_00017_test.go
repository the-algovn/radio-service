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

// TestMusingCadenceMigration pins the one thing 00017 must leave behind: the
// musing timer on the EXISTING singleton row. A NOT NULL column with a
// non-volatile default backfills in place, so unlike 00014 no UPDATE is
// needed - but "unlike 00014" is exactly the kind of claim worth a test.
func TestMusingCadenceMigration(t *testing.T) {
	url := testutil.StartPostgres(t)
	testutil.Migrate(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	var musingEveryMin int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT dj_musing_every_min FROM station WHERE id = TRUE`).Scan(&musingEveryMin))
	require.Equal(t, 10, musingEveryMin)
}
