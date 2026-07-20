package library

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMemLibraryGetAddListDelete(t *testing.T) {
	ctx := context.Background()
	l := NewMemLibrary()

	_, found, err := l.Get(ctx, "abc123")
	require.NoError(t, err)
	require.False(t, found)

	t1 := Track{YTID: "abc123", Title: "Lo-fi Beats", Channel: "Chillhop Music", ArtifactID: "art-1", DurationS: 180, InputI: -14, InputTP: -1.5, InputLRA: 7, AddedAt: time.Now().Add(-time.Hour)}
	t2 := Track{YTID: "def456", Title: "Synthwave Drive", Channel: "Retro Waves", ArtifactID: "art-2", DurationS: 240, InputI: -13, InputTP: -1.2, InputLRA: 6, AddedAt: time.Now()}
	require.NoError(t, l.Add(ctx, t1))
	require.NoError(t, l.Add(ctx, t2))

	got, found, err := l.Get(ctx, "abc123")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, t1, got)

	// dedup: re-adding an existing yt_id is a no-op (mirrors InsertTrack's
	// ON CONFLICT DO NOTHING).
	require.NoError(t, l.Add(ctx, Track{YTID: "abc123", Title: "changed"}))
	got, _, err = l.Get(ctx, "abc123")
	require.NoError(t, err)
	require.Equal(t, "Lo-fi Beats", got.Title)

	all, err := l.List(ctx, "", 0)
	require.NoError(t, err)
	require.Len(t, all, 2)
	require.Equal(t, "def456", all[0].YTID) // newest first

	// ILIKE-style substring match, case-insensitive, against title or channel.
	byTitle, err := l.List(ctx, "lo-fi", 10)
	require.NoError(t, err)
	require.Len(t, byTitle, 1)
	require.Equal(t, "abc123", byTitle[0].YTID)

	byChannel, err := l.List(ctx, "RETRO", 10)
	require.NoError(t, err)
	require.Len(t, byChannel, 1)
	require.Equal(t, "def456", byChannel[0].YTID)

	none, err := l.List(ctx, "nonexistent", 10)
	require.NoError(t, err)
	require.Empty(t, none)

	artifactID, found, err := l.Delete(ctx, "abc123")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "art-1", artifactID)

	_, found, err = l.Get(ctx, "abc123")
	require.NoError(t, err)
	require.False(t, found)

	_, found, err = l.Delete(ctx, "abc123")
	require.NoError(t, err)
	require.False(t, found)
}

func TestMemLibraryListDefaultLimit(t *testing.T) {
	ctx := context.Background()
	l := NewMemLibrary()
	for i := 0; i < 60; i++ {
		require.NoError(t, l.Add(ctx, Track{YTID: fmt.Sprintf("yt-%02d", i), AddedAt: time.Now().Add(time.Duration(i) * time.Second)}))
	}
	out, err := l.List(ctx, "", 0)
	require.NoError(t, err)
	require.Len(t, out, 50)
}
