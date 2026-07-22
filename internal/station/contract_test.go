package station_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/station"
)

type storeFactory func(t *testing.T) station.Store

func runStoreContract(t *testing.T, newStore storeFactory) {
	ctx := context.Background()

	t.Run("defaults: off air, ai enabled", func(t *testing.T) {
		s := newStore(t)
		st, err := s.GetStation(ctx)
		require.NoError(t, err)
		require.False(t, st.OnAir)
		require.Nil(t, st.OnAirSince)
		require.True(t, st.AIEnabled)
	})

	t.Run("on-air flip is idempotent and preserves the anchor", func(t *testing.T) {
		s := newStore(t)
		on, err := s.GoOnAir(ctx)
		require.NoError(t, err)
		require.True(t, on.OnAir)
		require.NotNil(t, on.OnAirSince)
		anchor := *on.OnAirSince

		again, err := s.GoOnAir(ctx)
		require.NoError(t, err)
		require.True(t, again.OnAir)
		require.NotNil(t, again.OnAirSince)
		require.Equal(t, anchor.UTC().Truncate(0), again.OnAirSince.UTC().Truncate(0))

		off, err := s.GoOffAir(ctx)
		require.NoError(t, err)
		require.False(t, off.OnAir)
		require.Nil(t, off.OnAirSince)
	})

	t.Run("ai pause round-trips independently of air state", func(t *testing.T) {
		s := newStore(t)
		st, err := s.SetAIEnabled(ctx, false)
		require.NoError(t, err)
		require.False(t, st.AIEnabled)
		st, err = s.GetStation(ctx)
		require.NoError(t, err)
		require.False(t, st.AIEnabled)

		_, err = s.GoOnAir(ctx)
		require.NoError(t, err)
		st, err = s.SetAIEnabled(ctx, true)
		require.NoError(t, err)
		require.True(t, st.AIEnabled)
		require.True(t, st.OnAir) // air state untouched by the toggle
	})
}
