package broadcast_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/broadcast"
)

type storeFactory func(t *testing.T) broadcast.Store

func runStoreContract(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	base := time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC)

	t.Run("an opened session is open until closed", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.Open(ctx, base))

		got, err := s.Overlapping(ctx, base.Add(-time.Hour), base.Add(time.Hour), 0)
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Nil(t, got[0].EndedAt, "a freshly opened session must read as open")

		require.NoError(t, s.Close(ctx, base.Add(90*time.Minute)))
		got, err = s.Overlapping(ctx, base.Add(-time.Hour), base.Add(3*time.Hour), 0)
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.NotNil(t, got[0].EndedAt)
		require.WithinDuration(t, base.Add(90*time.Minute), *got[0].EndedAt, time.Second)
	})

	t.Run("close is a no-op when nothing is open", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.Close(ctx, base), "closing with no open session must not error")
	})

	t.Run("overlapping returns newest first and honours the window", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.Open(ctx, base))
		require.NoError(t, s.Close(ctx, base.Add(time.Hour)))
		require.NoError(t, s.Open(ctx, base.Add(5*time.Hour)))
		require.NoError(t, s.Close(ctx, base.Add(6*time.Hour)))

		got, err := s.Overlapping(ctx, base.Add(-time.Hour), base.Add(7*time.Hour), 0)
		require.NoError(t, err)
		require.Len(t, got, 2)
		require.True(t, got[0].StartedAt.After(got[1].StartedAt), "newest first")

		// A window entirely before both sessions selects neither.
		got, err = s.Overlapping(ctx, base.Add(-3*time.Hour), base.Add(-2*time.Hour), 0)
		require.NoError(t, err)
		require.Empty(t, got)
	})

	t.Run("an open session overlaps any window that starts after it", func(t *testing.T) {
		// The live session has ended_at NULL; a window an hour into it must
		// still find it, or the console loses its divider mid-broadcast.
		s := newStore(t)
		require.NoError(t, s.Open(ctx, base))
		got, err := s.Overlapping(ctx, base.Add(time.Hour), base.Add(2*time.Hour), 0)
		require.NoError(t, err)
		require.Len(t, got, 1)
	})

	t.Run("limit <= 0 uses the shared default in every store", func(t *testing.T) {
		s := newStore(t)
		for i := range defaultLimitProbe + 5 {
			at := base.Add(time.Duration(i) * time.Hour)
			require.NoError(t, s.Open(ctx, at))
			require.NoError(t, s.Close(ctx, at.Add(30*time.Minute)))
		}
		got, err := s.Overlapping(ctx, base.Add(-time.Hour), base.Add(1000*time.Hour), 0)
		require.NoError(t, err)
		require.Len(t, got, defaultLimitProbe)
	})

	t.Run("close orphans closes an open session and reports the count", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.Open(ctx, base))

		n, err := s.CloseOrphans(ctx, base)
		require.NoError(t, err)
		require.Equal(t, int64(1), n)

		got, err := s.Overlapping(ctx, base.Add(-time.Hour), base.Add(time.Hour), 0)
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.NotNil(t, got[0].EndedAt, "boot reconciliation must leave no open row")

		n, err = s.CloseOrphans(ctx, base)
		require.NoError(t, err)
		require.Zero(t, n, "a second run must find nothing left to close")
	})
}

// defaultLimitProbe mirrors the package's unexported defaultLimit. Kept as a
// literal so a change to the constant fails this test loudly rather than
// silently agreeing with itself.
const defaultLimitProbe = 20
