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

// TestSeamBreakMigration pins the three things 00014 must leave behind: the
// talk_memory table, the next_up.request_id default, and the new DJ cadence
// on the EXISTING singleton row (not just the column default — an already
// deployed station has a row, and a default alone would not move it).
func TestSeamBreakMigration(t *testing.T) {
	url := testutil.StartPostgres(t)
	testutil.Migrate(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM talk_memory`).Scan(&n))
	require.Equal(t, 0, n)

	var reqID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT request_id FROM next_up WHERE id = TRUE`).Scan(&reqID))
	require.Equal(t, "", reqID)

	var breakEvery, maxChars int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT dj_break_every, dj_max_chars FROM station WHERE id = TRUE`).
		Scan(&breakEvery, &maxChars))
	require.Equal(t, 2, breakEvery)
	require.Equal(t, 1500, maxChars)
}
