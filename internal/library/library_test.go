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

	all, err := l.List(ctx, "", 0, 0)
	require.NoError(t, err)
	require.Len(t, all, 2)
	require.Equal(t, "def456", all[0].YTID) // newest first

	// ILIKE-style substring match, case-insensitive, against title or channel.
	byTitle, err := l.List(ctx, "lo-fi", 10, 0)
	require.NoError(t, err)
	require.Len(t, byTitle, 1)
	require.Equal(t, "abc123", byTitle[0].YTID)

	byChannel, err := l.List(ctx, "RETRO", 10, 0)
	require.NoError(t, err)
	require.Len(t, byChannel, 1)
	require.Equal(t, "def456", byChannel[0].YTID)

	none, err := l.List(ctx, "nonexistent", 10, 0)
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
	out, err := l.List(ctx, "", 0, 0)
	require.NoError(t, err)
	require.Len(t, out, 50)
}

func TestAllIDsSorted(t *testing.T) {
	lib := NewMemLibrary()
	ctx := context.Background()
	for _, id := range []string{"zz", "aa", "mm"} {
		require.NoError(t, lib.Add(ctx, Track{YTID: id, Title: id, ArtifactID: "a-" + id}))
	}
	ids, err := lib.AllIDs(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"aa", "mm", "zz"}, ids)
}

// TestLibraryCuesRoundTripAndUnmeasuredSentinel exercises the SetCues /
// MissingCues contract. Note this deliberately does NOT rely on Add to
// invent -1 from Go's zero value (see the correction to task-3-brief.md
// Step 4): the caller sets the unmeasured sentinel explicitly, exactly as
// acquire.Acquire does on Cues' failure path.
func TestLibraryCuesRoundTripAndUnmeasuredSentinel(t *testing.T) {
	ctx := context.Background()
	lib := NewMemLibrary()

	require.NoError(t, lib.Add(ctx, Track{YTID: "a", ArtifactID: "art-a", DurationS: 60, TailSilenceS: -1, TailDecayS: -1}))

	got, ok, err := lib.Get(ctx, "a")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, -1.0, got.TailSilenceS, "a track added with the unmeasured sentinel stays unmeasured")
	require.Equal(t, -1.0, got.TailDecayS)

	missing, err := lib.MissingCues(ctx, 10)
	require.NoError(t, err)
	require.Len(t, missing, 1)

	require.NoError(t, lib.SetCues(ctx, "a", 1.25, 3.5))

	got, _, err = lib.Get(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, 1.25, got.TailSilenceS)
	require.Equal(t, 3.5, got.TailDecayS)

	missing, err = lib.MissingCues(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, missing, "a measured track is no longer backfill work")
}

// TestLibraryAddDoesNotDefaultGenuineZeroCues is the regression guard for
// the correction to task-3-brief.md Step 4. A cold ending legitimately
// measures 0/0 (see pcm.TestTailCuesColdEndingHasNoSilenceAndNoDecay in
// internal/live/pcm) — that is "measured, no usable tail", a true and useful
// fact, not an absence of measurement. If Add ever defaults a zero-value
// Track's cues to -1, this test fails: got would read back -1/-1 instead of
// 0/0, and the track would wrongly reappear in MissingCues.
func TestLibraryAddDoesNotDefaultGenuineZeroCues(t *testing.T) {
	ctx := context.Background()
	lib := NewMemLibrary()

	require.NoError(t, lib.Add(ctx, Track{YTID: "cold", ArtifactID: "art-cold", DurationS: 60, TailSilenceS: 0, TailDecayS: 0}))

	got, ok, err := lib.Get(ctx, "cold")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 0.0, got.TailSilenceS, "a cold ending measures 0/0 and must survive Add untouched")
	require.Equal(t, 0.0, got.TailDecayS)

	missing, err := lib.MissingCues(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, missing, "0/0 is measured, not unmeasured — must not appear in backfill work")
}

func TestMemLibraryListPaginationAndCount(t *testing.T) {
	ctx := context.Background()
	l := NewMemLibrary()
	// Five tracks, ascending AddedAt so newest-first order is yt-04..yt-00.
	for i := 0; i < 5; i++ {
		require.NoError(t, l.Add(ctx, Track{YTID: fmt.Sprintf("yt-%02d", i), AddedAt: time.Now().Add(time.Duration(i) * time.Second)}))
	}

	page1, err := l.List(ctx, "", 2, 0)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	require.Equal(t, "yt-04", page1[0].YTID) // newest first
	require.Equal(t, "yt-03", page1[1].YTID)

	page2, err := l.List(ctx, "", 2, 2)
	require.NoError(t, err)
	require.Len(t, page2, 2)
	require.Equal(t, "yt-02", page2[0].YTID)

	beyond, err := l.List(ctx, "", 2, 10)
	require.NoError(t, err)
	require.Empty(t, beyond)

	total, err := l.Count(ctx, "")
	require.NoError(t, err)
	require.Equal(t, int64(5), total)

	zero, err := l.Count(ctx, "nonexistent")
	require.NoError(t, err)
	require.Equal(t, int64(0), zero)
}
