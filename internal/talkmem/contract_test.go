package talkmem_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/talkmem"
)

type storeFactory func(t *testing.T) talkmem.Store

func runStoreContract(t *testing.T, newStore storeFactory) {
	ctx := context.Background()

	t.Run("empty store returns nothing", func(t *testing.T) {
		s := newStore(t)
		got, err := s.Recent(ctx, time.Now().Add(-time.Hour), 8)
		require.NoError(t, err)
		require.Empty(t, got)
	})

	t.Run("recent returns OLDEST first — the narrative order", func(t *testing.T) {
		s := newStore(t)
		since := time.Now().Add(-time.Hour)
		for _, sum := range []string{"một", "hai", "ba"} {
			require.NoError(t, s.Append(ctx, talkmem.Entry{Kind: "seam", Summary: sum}))
			time.Sleep(2 * time.Millisecond) // distinct created_at ordering
		}
		got, err := s.Recent(ctx, since, 8)
		require.NoError(t, err)
		require.Len(t, got, 3)
		require.Equal(t, []string{"một", "hai", "ba"},
			[]string{got[0].Summary, got[1].Summary, got[2].Summary})
	})

	t.Run("limit keeps the NEWEST n, still oldest-first", func(t *testing.T) {
		s := newStore(t)
		since := time.Now().Add(-time.Hour)
		for _, sum := range []string{"một", "hai", "ba", "bốn"} {
			require.NoError(t, s.Append(ctx, talkmem.Entry{Kind: "seam", Summary: sum}))
			time.Sleep(2 * time.Millisecond)
		}
		got, err := s.Recent(ctx, since, 2)
		require.NoError(t, err)
		require.Len(t, got, 2)
		require.Equal(t, "ba", got[0].Summary)
		require.Equal(t, "bốn", got[1].Summary)
	})

	t.Run("since excludes older sessions", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.Append(ctx, talkmem.Entry{Kind: "seam", Summary: "đêm trước"}))
		time.Sleep(5 * time.Millisecond)
		cut := time.Now()
		time.Sleep(5 * time.Millisecond)
		require.NoError(t, s.Append(ctx, talkmem.Entry{Kind: "seam", Summary: "đêm nay"}))

		got, err := s.Recent(ctx, cut, 8)
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, "đêm nay", got[0].Summary)
	})

	t.Run("phrases round-trip, including empty", func(t *testing.T) {
		s := newStore(t)
		since := time.Now().Add(-time.Hour)
		require.NoError(t, s.Append(ctx, talkmem.Entry{
			Kind: "seam", Summary: "s", Phrases: []string{"bạn nghe đài", "khuya rồi"}}))
		time.Sleep(2 * time.Millisecond)
		require.NoError(t, s.Append(ctx, talkmem.Entry{Kind: "seam", Summary: "t"}))

		got, err := s.Recent(ctx, since, 8)
		require.NoError(t, err)
		require.Len(t, got, 2)
		require.Equal(t, []string{"bạn nghe đài", "khuya rồi"}, got[0].Phrases)
		require.Empty(t, got[1].Phrases)
	})
}
