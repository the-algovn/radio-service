package playlist_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/playlist"
)

// storeFactory returns a fresh Store plus a Library sharing its track source,
// pre-seeded with nothing. Each contract case seeds what it needs.
type storeFactory func(t *testing.T) (playlist.Store, library.Library)

func seed(t *testing.T, lib library.Library, ytID string, durationS float64) {
	t.Helper()
	require.NoError(t, lib.Add(context.Background(), library.Track{
		YTID: ytID, Title: "title-" + ytID, Channel: "chan-" + ytID,
		DurationS: durationS, ArtifactID: "art-" + ytID,
	}))
}

func runStoreContract(t *testing.T, newStore storeFactory) {
	ctx := context.Background()

	t.Run("playlist CRUD round-trip", func(t *testing.T) {
		st, _ := newStore(t)
		p, err := st.Create(ctx, "đêm khuya")
		require.NoError(t, err)
		require.NotEmpty(t, p.ID)
		require.Equal(t, "đêm khuya", p.Name)
		require.Zero(t, p.TrackCount)
		require.False(t, p.IsActive)

		all, err := st.List(ctx)
		require.NoError(t, err)
		require.Len(t, all, 1)

		p2, err := st.Rename(ctx, p.ID, "sáng sớm")
		require.NoError(t, err)
		require.Equal(t, "sáng sớm", p2.Name)

		require.NoError(t, st.Delete(ctx, p.ID))
		_, _, err = st.Get(ctx, p.ID)
		require.ErrorIs(t, err, playlist.ErrNotFound)
	})

	t.Run("newest playlist listed first", func(t *testing.T) {
		st, _ := newStore(t)
		_, err := st.Create(ctx, "first")
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond) // created_at tiebreak
		_, err = st.Create(ctx, "second")
		require.NoError(t, err)
		all, err := st.List(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{"second", "first"}, []string{all[0].Name, all[1].Name})
	})

	t.Run("membership: add appends, dup no-op, remove, unknown track", func(t *testing.T) {
		st, lib := newStore(t)
		seed(t, lib, "a", 100)
		seed(t, lib, "b", 50)
		p, err := st.Create(ctx, "mix")
		require.NoError(t, err)

		_, items, err := st.AddTrack(ctx, p.ID, "a")
		require.NoError(t, err)
		require.Len(t, items, 1)
		sum, items, err := st.AddTrack(ctx, p.ID, "b")
		require.NoError(t, err)
		require.Equal(t, []string{"a", "b"}, []string{items[0].YTID, items[1].YTID})
		require.Equal(t, 2, sum.TrackCount)
		require.Equal(t, int64(150), sum.TotalDurationS)
		require.Equal(t, []int{0, 1}, []int{items[0].Position, items[1].Position})
		require.Equal(t, "title-a", items[0].Title)
		require.Equal(t, int64(100), items[0].DurationS)

		// duplicate add: idempotent, still 2 items
		_, items, err = st.AddTrack(ctx, p.ID, "a")
		require.NoError(t, err)
		require.Len(t, items, 2)

		// unknown track / unknown playlist
		_, _, err = st.AddTrack(ctx, p.ID, "nope")
		require.ErrorIs(t, err, playlist.ErrNotFound)
		_, _, err = st.AddTrack(ctx, "00000000-0000-0000-0000-000000000000", "a")
		require.ErrorIs(t, err, playlist.ErrNotFound)

		// remove re-yields contiguous positions
		_, items, err = st.RemoveTrack(ctx, p.ID, "a")
		require.NoError(t, err)
		require.Len(t, items, 1)
		require.Equal(t, "b", items[0].YTID)
		require.Equal(t, 0, items[0].Position)
		_, _, err = st.RemoveTrack(ctx, p.ID, "a")
		require.ErrorIs(t, err, playlist.ErrNotFound)
	})

	t.Run("reorder: whole-list replace, stale set rejected", func(t *testing.T) {
		st, lib := newStore(t)
		for _, id := range []string{"a", "b", "c"} {
			seed(t, lib, id, 60)
		}
		p, err := st.Create(ctx, "mix")
		require.NoError(t, err)
		for _, id := range []string{"a", "b", "c"} {
			_, _, err = st.AddTrack(ctx, p.ID, id)
			require.NoError(t, err)
		}

		_, items, err := st.Reorder(ctx, p.ID, []string{"c", "a", "b"})
		require.NoError(t, err)
		require.Equal(t, []string{"c", "a", "b"}, []string{items[0].YTID, items[1].YTID, items[2].YTID})
		require.Equal(t, []int{0, 1, 2}, []int{items[0].Position, items[1].Position, items[2].Position})

		_, _, err = st.Reorder(ctx, p.ID, []string{"c", "a"}) // missing b
		require.ErrorIs(t, err, playlist.ErrStale)
		_, _, err = st.Reorder(ctx, p.ID, []string{"c", "a", "b", "x"}) // extra
		require.ErrorIs(t, err, playlist.ErrStale)
		_, _, err = st.Reorder(ctx, p.ID, []string{"c", "a", "a"}) // duplicate, same length
		require.ErrorIs(t, err, playlist.ErrStale)
	})

	t.Run("station state machine", func(t *testing.T) {
		st, lib := newStore(t)
		seed(t, lib, "a", 60)
		seed(t, lib, "b", 60)

		s, err := st.GetStation(ctx)
		require.NoError(t, err)
		require.False(t, s.OnAir)
		require.Empty(t, s.ActivePlaylistID)
		require.Nil(t, s.OnAirSince)

		// v1: GoOnAir no longer needs an active playlist — the engine falls
		// back to library shuffle (spec §4.2). Idempotency and the
		// OnAirSince anchor still hold.
		s, err = st.GoOnAir(ctx)
		require.NoError(t, err)
		require.True(t, s.OnAir)
		require.NotNil(t, s.OnAirSince)

		empty, err := st.Create(ctx, "empty")
		require.NoError(t, err)
		_, err = st.SetActive(ctx, empty.ID)
		require.ErrorIs(t, err, playlist.ErrEmptyPlaylist)
		_, err = st.SetActive(ctx, "00000000-0000-0000-0000-000000000000")
		require.ErrorIs(t, err, playlist.ErrNotFound)

		p, err := st.Create(ctx, "mix")
		require.NoError(t, err)
		_, _, err = st.AddTrack(ctx, p.ID, "a")
		require.NoError(t, err)
		s, err = st.SetActive(ctx, p.ID)
		require.NoError(t, err)
		require.Equal(t, p.ID, s.ActivePlaylistID)
		require.Equal(t, "mix", s.ActivePlaylistName)
		require.Equal(t, 1, s.ActiveTrackCount)

		// active flag shows up in listings
		all, err := st.List(ctx)
		require.NoError(t, err)
		for _, sm := range all {
			require.Equal(t, sm.ID == p.ID, sm.IsActive)
		}

		s, err = st.GoOnAir(ctx)
		require.NoError(t, err)
		require.True(t, s.OnAir)
		require.NotNil(t, s.OnAirSince)
		since := *s.OnAirSince

		// idempotent second GoOnAir preserves the anchor
		s, err = st.GoOnAir(ctx)
		require.NoError(t, err)
		require.True(t, s.OnAir)
		require.WithinDuration(t, since, *s.OnAirSince, time.Millisecond)

		// on-air guards
		require.ErrorIs(t, st.Delete(ctx, p.ID), playlist.ErrActiveOnAir)
		_, _, err = st.RemoveTrack(ctx, p.ID, "a") // would empty the active playlist
		require.ErrorIs(t, err, playlist.ErrActiveOnAir)

		// editing the active playlist on-air is otherwise allowed
		_, _, err = st.AddTrack(ctx, p.ID, "b")
		require.NoError(t, err)
		_, _, err = st.RemoveTrack(ctx, p.ID, "a") // 2 tracks now — fine
		require.NoError(t, err)

		// moving the pointer while on-air is allowed
		p2, err := st.Create(ctx, "other")
		require.NoError(t, err)
		_, _, err = st.AddTrack(ctx, p2.ID, "a")
		require.NoError(t, err)
		s, err = st.SetActive(ctx, p2.ID)
		require.NoError(t, err)
		require.Equal(t, p2.ID, s.ActivePlaylistID)
		require.True(t, s.OnAir)

		s, err = st.GoOffAir(ctx)
		require.NoError(t, err)
		require.False(t, s.OnAir)
		require.Nil(t, s.OnAirSince)
		s, err = st.GoOffAir(ctx) // idempotent
		require.NoError(t, err)
		require.False(t, s.OnAir)

		// off-air: deleting the active playlist clears the pointer
		require.NoError(t, st.Delete(ctx, p2.ID))
		s, err = st.GetStation(ctx)
		require.NoError(t, err)
		require.Empty(t, s.ActivePlaylistID)
	})

	t.Run("go on air with an emptied active playlist", func(t *testing.T) {
		st, lib := newStore(t)
		seed(t, lib, "a", 60)
		p, err := st.Create(ctx, "solo")
		require.NoError(t, err)
		_, _, err = st.AddTrack(ctx, p.ID, "a")
		require.NoError(t, err)
		_, err = st.SetActive(ctx, p.ID)
		require.NoError(t, err)
		_, _, err = st.RemoveTrack(ctx, p.ID, "a") // off-air: emptying active is allowed
		require.NoError(t, err)
		// v1: an emptied (or absent) active playlist no longer blocks
		// GoOnAir — the engine falls back to library shuffle.
		_, err = st.GoOnAir(ctx)
		require.NoError(t, err)
	})
}
