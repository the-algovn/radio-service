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

func TestPGCloseOrphansBoundedByNextSession(t *testing.T) {
	// An orphan session sandwiched between two closed sessions must close at its
	// OWN last item, not at the newer session's start — the upper-bound subquery
	// prevents items from a later session inflating the orphan's ended_at.
	ctx := context.Background()
	url := testutil.StartPostgres(t)
	testutil.Migrate(t, url)
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	start := time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC)

	// Session A: closed, 20:00–20:10, one item
	_, err = pool.Exec(ctx,
		`INSERT INTO broadcast_session (started_at, ended_at) VALUES ($1, $2)`,
		start, start.Add(10*time.Minute))
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO air_log (yt_id, title, artist, started_at, duration_s)
		 VALUES ('a1', 'A', 'a', $1, 120)`, start.Add(2*time.Minute))
	require.NoError(t, err)

	// Session B (orphan): 20:15–???, item at 20:16 with 180s → computed end 20:19
	orphanStart := start.Add(15 * time.Minute)
	_, err = pool.Exec(ctx,
		`INSERT INTO broadcast_session (started_at) VALUES ($1)`, orphanStart)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO air_log (yt_id, title, artist, started_at, duration_s)
		 VALUES ('b1', 'B', 'b', $1, 180)`, orphanStart.Add(1*time.Minute))
	require.NoError(t, err)

	// Session C: closed, 20:30–20:40, one item
	sessionCStart := start.Add(30 * time.Minute)
	_, err = pool.Exec(ctx,
		`INSERT INTO broadcast_session (started_at, ended_at) VALUES ($1, $2)`,
		sessionCStart, sessionCStart.Add(10*time.Minute))
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO air_log (yt_id, title, artist, started_at, duration_s)
		 VALUES ('c1', 'C', 'c', $1, 60)`, sessionCStart.Add(1*time.Minute))
	require.NoError(t, err)

	// at well past everything so the LEAST clamp does not fire.
	s := broadcast.NewPGStore(pool)
	n, err := s.CloseOrphans(ctx, start.Add(2*time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	// The window spans all three sessions because Overlapping returns everything
	// that intersects; the orphan (id=2) is the second-newest.
	got, err := s.Overlapping(ctx, start.Add(-time.Hour), start.Add(3*time.Hour), 0)
	require.NoError(t, err)
	require.Len(t, got, 3)
	// Orphan (middle session, started_at=20:15) is got[1] (newest-first: C, B, A).
	orphan := got[1]
	require.NotNil(t, orphan.EndedAt)
	// Orphan's last item: 20:16 + 180s = 20:19.
	expectedEnd := orphanStart.Add(1*time.Minute + 180*time.Second)
	require.WithinDuration(t, expectedEnd, *orphan.EndedAt, time.Second,
		"orphan must close at its own last item, not at the newer session's start")
	// Guard: must NOT land at session C's start (20:30).
	require.NotEqual(t, sessionCStart, *orphan.EndedAt,
		"orphan ended_at must not be session C's start")
}
