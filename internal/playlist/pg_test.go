//go:build integration

package playlist_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/playlist"
	"github.com/the-algovn/radio-service/internal/testutil"
)

func newPGFixture(t *testing.T) (playlist.Store, library.Library) {
	t.Helper()
	url := testutil.StartPostgres(t)
	testutil.Migrate(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return playlist.NewPGStore(pool), library.NewPGLibrary(pool)
}

func TestPGStoreContract(t *testing.T) {
	runStoreContract(t, newPGFixture)
}

// TestPGStoreLibraryCascade is pg-only: deleting a library track cascades out
// of playlists via FK, and read positions stay contiguous (row_number).
func TestPGStoreLibraryCascade(t *testing.T) {
	st, lib := newPGFixture(t)
	ctx := context.Background()
	seed(t, lib, "a", 60)
	seed(t, lib, "b", 60)
	seed(t, lib, "c", 60)
	p, err := st.Create(ctx, "mix")
	require.NoError(t, err)
	for _, id := range []string{"a", "b", "c"} {
		_, _, err = st.AddTrack(ctx, p.ID, id)
		require.NoError(t, err)
	}

	_, found, err := lib.Delete(ctx, "b")
	require.NoError(t, err)
	require.True(t, found)

	sum, items, err := st.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, 2, sum.TrackCount)
	require.Equal(t, []string{"a", "c"}, []string{items[0].YTID, items[1].YTID})
	require.Equal(t, []int{0, 1}, []int{items[0].Position, items[1].Position})
}
