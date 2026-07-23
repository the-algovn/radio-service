package schedule_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/schedule"
)

type storeFactory func(t *testing.T) schedule.Store

func runStoreContract(t *testing.T, newStore storeFactory) {
	ctx := context.Background()

	t.Run("default: nothing committed", func(t *testing.T) {
		s := newStore(t)
		_, ok, err := s.GetNextUp(ctx)
		require.NoError(t, err)
		require.False(t, ok)
	})

	t.Run("set then get returns it", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.SetNextUp(ctx, schedule.NextUp{YTID: "abc", Title: "t", Channel: "c"}))
		got, ok, err := s.GetNextUp(ctx)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, schedule.NextUp{YTID: "abc", Title: "t", Channel: "c"}, got)
	})

	t.Run("set overwrites", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.SetNextUp(ctx, schedule.NextUp{YTID: "a", Title: "1"}))
		require.NoError(t, s.SetNextUp(ctx, schedule.NextUp{YTID: "b", Title: "2"}))
		got, ok, err := s.GetNextUp(ctx)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, "b", got.YTID)
	})

	t.Run("clear removes it", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.SetNextUp(ctx, schedule.NextUp{YTID: "a"}))
		require.NoError(t, s.ClearNextUp(ctx))
		_, ok, err := s.GetNextUp(ctx)
		require.NoError(t, err)
		require.False(t, ok)
	})
}
