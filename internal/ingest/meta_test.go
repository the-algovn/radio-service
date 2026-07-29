package ingest

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMetaNotMusic(t *testing.T) {
	tests := []struct {
		name       string
		m          Meta
		wantReject bool
	}{
		{"youtube says Music", Meta{Categories: []string{"Music"}}, false},
		{"auto-generated track metadata",
			Meta{Track: "Nắng Ấm Xa Dần", Artists: []string{"Sơn Tùng M-TP"}}, false},
		{"a livestream is never a song",
			Meta{Categories: []string{"Music"}, MediaType: "livestream"}, true},
		{"a short is never a full track",
			Meta{Categories: []string{"Music"}, MediaType: "short"}, true},
		{"positively categorized as something unambiguously not music",
			Meta{Categories: []string{"News & Politics"}}, true},
		// The category is uploader-chosen and defaults to "People & Blogs"; a
		// large share of genuine Vietnamese indie/OST/cover uploads sit under
		// these categories, so they must be admitted, not rejected.
		{"Entertainment is admitted", Meta{Categories: []string{"Entertainment"}}, false},
		{"People & Blogs is admitted", Meta{Categories: []string{"People & Blogs"}}, false},
		// Fail open. Absent metadata is not evidence of anything, and rejecting
		// on it would recreate the empty-pool bug one layer down.
		{"no metadata at all is admitted", Meta{}, false},
		{"no category but has a track name", Meta{Track: "Bài Gì Đó"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := tc.m.NotMusic()
			require.Equal(t, tc.wantReject, got)
			if tc.wantReject {
				require.NotEmpty(t, reason, "a rejection must name its reason")
			}
		})
	}
}

func TestMetaFromInfoJSON(t *testing.T) {
	raw := []byte(`{
		"categories":["Music"],
		"track":"Nắng Ấm Xa Dần",
		"artists":["Sơn Tùng M-TP"],
		"album":"Single",
		"media_type":"video"
	}`)
	m, err := metaFrom(raw)
	require.NoError(t, err)
	require.Equal(t, []string{"Music"}, m.Categories)
	require.Equal(t, "Nắng Ấm Xa Dần", m.Track)
	require.Equal(t, []string{"Sơn Tùng M-TP"}, m.Artists)
	require.Equal(t, "video", m.MediaType)
}
