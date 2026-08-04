//go:build integration

package broadcast_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/broadcast"
	"github.com/the-algovn/radio-service/internal/testutil"
)

func newPGStore(t *testing.T) broadcast.Store {
	t.Helper()
	url := testutil.StartPostgres(t)
	testutil.Migrate(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return broadcast.NewPGStore(pool)
}

func TestPGStoreContract(t *testing.T) {
	runStoreContract(t, newPGStore)
}

func TestPGCloseOrphansUsesTheLastAiredItem(t *testing.T) {
	// The SQL fallback (close at started_at) is what MemStore models. The PG
	// store additionally consults air_log + talk_segment, and THAT is the
	// behaviour the console's dividers actually depend on, so it needs its own
	// test — the shared contract cannot express it.
	ctx := context.Background()
	url := testutil.StartPostgres(t)
	testutil.Migrate(t, url)
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	start := time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC)
	_, err = pool.Exec(ctx, `INSERT INTO broadcast_session (started_at) VALUES ($1)`, start)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO air_log (yt_id, title, artist, started_at, duration_s)
		 VALUES ('y1', 't', 'a', $1, 240)`, start.Add(30*time.Minute))
	require.NoError(t, err)

	s := broadcast.NewPGStore(pool)
	n, err := s.CloseOrphans(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	got, err := s.Overlapping(ctx, start.Add(-time.Hour), start.Add(3*time.Hour), 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].EndedAt)
	require.WithinDuration(t, start.Add(34*time.Minute), *got[0].EndedAt, time.Second,
		"the session must close at the END of the last item that aired inside it")
}
