//go:build integration

package library_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/testutil"
)

func TestPGLibraryRoundTrip(t *testing.T) {
	url := testutil.StartPostgres(t)
	testutil.Migrate(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	defer pool.Close()

	l := library.NewPGLibrary(pool)

	_, found, err := l.Get(ctx, "abc123")
	require.NoError(t, err)
	require.False(t, found)

	t1 := library.Track{YTID: "abc123", Title: "Lo-fi Beats", Channel: "Chillhop Music", ArtifactID: "art-1", DurationS: 180.5, InputI: -14, InputTP: -1.5, InputLRA: 7, AddedAt: time.Now()}
	require.NoError(t, l.Add(ctx, t1))

	got, found, err := l.Get(ctx, "abc123")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, t1.Title, got.Title)
	require.Equal(t, t1.ArtifactID, got.ArtifactID)
	require.Equal(t, t1.Channel, got.Channel)
	require.InDelta(t, t1.DurationS, got.DurationS, 1e-9)
	require.InDelta(t, t1.InputI, got.InputI, 1e-9)
	require.InDelta(t, t1.InputTP, got.InputTP, 1e-9)
	require.InDelta(t, t1.InputLRA, got.InputLRA, 1e-9)
	require.False(t, got.AddedAt.IsZero())

	// dedup: ON CONFLICT DO NOTHING.
	require.NoError(t, l.Add(ctx, library.Track{YTID: "abc123", Title: "changed", DurationS: 1, ArtifactID: "art-x"}))
	got, _, err = l.Get(ctx, "abc123")
	require.NoError(t, err)
	require.Equal(t, "Lo-fi Beats", got.Title)

	require.NoError(t, l.Add(ctx, library.Track{YTID: "def456", Title: "Synthwave Drive", Channel: "Retro Waves", ArtifactID: "art-2", DurationS: 240, InputI: -13, InputTP: -1.2, InputLRA: 6, AddedAt: time.Now()}))

	all, err := l.List(ctx, "", 0)
	require.NoError(t, err)
	require.Len(t, all, 2)

	byTitle, err := l.List(ctx, "lo-fi", 10)
	require.NoError(t, err)
	require.Len(t, byTitle, 1)
	require.Equal(t, "abc123", byTitle[0].YTID)

	artifactID, found, err := l.Delete(ctx, "abc123")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "art-1", artifactID)

	_, found, err = l.Get(ctx, "abc123")
	require.NoError(t, err)
	require.False(t, found)

	_, found, err = l.Delete(ctx, "nope")
	require.NoError(t, err)
	require.False(t, found)
}
