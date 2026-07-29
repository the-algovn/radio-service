package songkey

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The three uploads of one song that defeat every yt_id-keyed guard today.
func TestOfCollapsesTheSameSongAcrossUploads(t *testing.T) {
	want := Of("Sơn Tùng M-TP", "Nắng Ấm Xa Dần")
	require.NotEmpty(t, want)

	require.Equal(t, want, Of("Sơn Tùng M-TP", "Nắng Ấm Xa Dần (Official MV)"))
	require.Equal(t, want, Of("Sơn Tùng M-TP - Topic", "Nắng Ấm Xa Dần"))
	require.Equal(t, want, Of("Sơn Tùng M-TP", "Nắng Ấm Xa Dần [Lyric Video]"))
	require.Equal(t, want, Of("Sơn Tùng M-TP", "Nắng Ấm Xa Dần - Audio"))
}

// NFD decomposition alone does not split đ; it needs an explicit mapping.
func TestOfFoldsDStroke(t *testing.T) {
	require.Equal(t, Of("Dan Truong", "Di Ve Noi Dau"), Of("Đan Trường", "Đi Về Nơi Đâu"))
}

func TestOfDistinguishesDifferentSongs(t *testing.T) {
	require.NotEqual(t, Of("Ca Sĩ", "Bài Một"), Of("Ca Sĩ", "Bài Hai"))
	require.NotEqual(t, Of("Ca Sĩ A", "Cùng Tên"), Of("Ca Sĩ B", "Cùng Tên"))
}

// Tone-folding genuinely collapses distinct Vietnamese words. This is TOLERATED
// and is exactly why the database index built on this key is non-unique: a false
// merge must cost variety, never a hard duplicate error shown to a listener.
func TestOfToleratesToneCollisions(t *testing.T) {
	require.Equal(t, Of("Ca Sĩ", "Chờ"), Of("Ca Sĩ", "Chợ"),
		"tone collision is accepted; the index must therefore not be UNIQUE")
}

func TestOfReturnsEmptyWhenUnusable(t *testing.T) {
	require.Empty(t, Of("", "Bài Gì Đó"))
	require.Empty(t, Of("Ca Sĩ", ""))
	require.Empty(t, Of("Ca Sĩ", "(Official MV)"), "nothing survives the strip")
}

// "Longest segment wins" picked the qualifier over the title whenever the real
// title was shorter than its qualifier, folding unrelated songs onto one key.
// The separator rule must keep the first segment instead.
func TestOfDistinguishesShortTitlesSharingALongQualifier(t *testing.T) {
	require.NotEqual(t,
		Of("Ca Sĩ", "Mưa | Official Audio"), Of("Ca Sĩ", "Sao | Official Audio"))
	require.NotEqual(t,
		Of("Ca Sĩ", "Mưa - Lyrics Video"), Of("Ca Sĩ", "Sao - Lyrics Video"))
}

// The same "longest wins" bug corrupted the artist side, reducing the collab
// "Karik - Only C" to "only-c" and losing Karik entirely.
func TestOfPreservesCollabArtistFirstName(t *testing.T) {
	require.NotEqual(t, Of("Karik - Only C", "X"), Of("Only C", "X"))
}

// The over-merge Of must never be asked to make: calling it on a channel name
// and a raw video title, the only inputs a search result offers. Vietnamese
// YouTube overwhelmingly titles uploads "Artist - Title", so first-segment
// folds the TITLE onto the ARTIST — these two unrelated Đen Vâu songs
// genuinely collapse to the same key, "den-vau-official/den". That is not a
// bug in fold() to chase with a third heuristic (two have already failed
// here); it is proof that Of is unsafe on a (channel, raw title) pair and
// must only ever be called on a real (artist, track) pair from metadata.
// programmer.factsFrom (internal/programmer/guards.go) is the caller this
// documents: it must leave SongKey "" for search results rather than call Of
// on this shape.
func TestOfOverMergesAChannelAndRawTitlePair(t *testing.T) {
	require.Equal(t,
		Of("Đen Vâu Official", "Đen - Trốn Tìm ft. MTV Band (M/V)"),
		Of("Đen Vâu Official", "Đen - Mang Tiền Về Cho Mẹ ft. Nguyên Thảo (M/V)"),
		"this collision is exactly why factsFrom must never call Of on channel+title")
}
