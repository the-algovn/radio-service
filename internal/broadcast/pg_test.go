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

	// at must land after the computed end (start + 34m) so the clamp does not
	// fire — the fixture times are all in the future of the real wall clock.
	s := broadcast.NewPGStore(pool)
	n, err := s.CloseOrphans(ctx, start.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	got, err := s.Overlapping(ctx, start.Add(-time.Hour), start.Add(3*time.Hour), 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].EndedAt)
	require.WithinDuration(t, start.Add(34*time.Minute), *got[0].EndedAt, time.Second,
		"the session must close at the END of the last item that aired inside it")
}

func TestPGCloseOrphansClampedToAt(t *testing.T) {
	// A process killed mid-item wrote the item's INTENDED duration at start, so
	// started_at + duration_s can overshoot the kill by up to one whole item.
	// The LEAST clamp caps the reconciled end at the instant reconciliation
	// runs — the session provably ended no later than now.
	ctx := context.Background()
	url := testutil.StartPostgres(t)
	testutil.Migrate(t, url)
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	start := time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC)
	_, err = pool.Exec(ctx, `INSERT INTO broadcast_session (started_at) VALUES ($1)`, start)
	require.NoError(t, err)
	// Track started at 20:30, duration 240s → computed end = 20:34:00
	_, err = pool.Exec(ctx,
		`INSERT INTO air_log (yt_id, title, artist, started_at, duration_s)
		 VALUES ('y1', 't', 'a', $1, 240)`, start.Add(30*time.Minute))
	require.NoError(t, err)

	// Kill at 20:33:00 — earlier than the item's computed end (20:34:00).
	killAt := start.Add(33 * time.Minute)

	s := broadcast.NewPGStore(pool)
	n, err := s.CloseOrphans(ctx, killAt)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	got, err := s.Overlapping(ctx, start.Add(-time.Hour), start.Add(3*time.Hour), 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].EndedAt)
	require.WithinDuration(t, killAt, *got[0].EndedAt, time.Second,
		"ended_at must be clamped to the reconciliation time, not the item's full intended duration")
}
