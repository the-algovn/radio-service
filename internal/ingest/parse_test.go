package ingest

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// yt-dlp emits `"duration": null` for live streams, upcoming premieres and
// deleted entries. Unmarshalling null into a plain float64 is a silent no-op
// that yields 0 — indistinguishable from a genuine 0-second video, which the
// programmer's duration floor then rejects as "too short". The parser must
// report "unknown" instead.
func TestCandidatesFromDistinguishesUnknownDuration(t *testing.T) {
	raw := []byte(`{"entries":[
		{"id":"known","title":"Known","channel":"C","duration":212.0,"view_count":900},
		{"id":"null","title":"Null","channel":"C","duration":null,"view_count":900},
		{"id":"absent","title":"Absent","channel":"C","view_count":900}
	]}`)

	cs, err := candidatesFrom(raw)
	require.NoError(t, err)
	require.Len(t, cs, 3)

	byID := map[string]Candidate{}
	for _, c := range cs {
		byID[c.YTID] = c
	}

	require.True(t, byID["known"].DurationKnown)
	require.Equal(t, int64(212), byID["known"].DurationS)

	require.False(t, byID["null"].DurationKnown, "explicit null must be unknown, not zero")
	require.False(t, byID["absent"].DurationKnown, "an absent field must be unknown, not zero")
}

func TestCandidatesFromReadsLiveAndShortForm(t *testing.T) {
	raw := []byte(`{"entries":[
		{"id":"live","title":"L","duration":null,"live_status":"is_live"},
		{"id":"soon","title":"U","duration":null,"live_status":"is_upcoming"},
		{"id":"past","title":"P","duration":300,"live_status":"was_live"},
		{"id":"short","title":"S","duration":45,"url":"https://www.youtube.com/shorts/short"},
		{"id":"plain","title":"N","duration":240,"url":"https://www.youtube.com/watch?v=plain"}
	]}`)

	cs, err := candidatesFrom(raw)
	require.NoError(t, err)

	byID := map[string]Candidate{}
	for _, c := range cs {
		byID[c.YTID] = c
	}

	require.True(t, byID["live"].Live)
	require.True(t, byID["soon"].Live)
	require.False(t, byID["past"].Live, "was_live is a finished stream, not a live one")
	require.True(t, byID["short"].ShortForm)
	require.False(t, byID["plain"].ShortForm)
}

// Channel falls back to uploader, and the largest thumbnail is kept — existing
// behaviour that must survive the refactor.
func TestCandidatesFromFallsBackToUploader(t *testing.T) {
	raw := []byte(`{"entries":[
		{"id":"a","title":"A","uploader":"Up","duration":200,
		 "thumbnails":[{"url":"small.jpg"},{"url":"large.jpg"}]}
	]}`)

	cs, err := candidatesFrom(raw)
	require.NoError(t, err)
	require.Len(t, cs, 1)
	require.Equal(t, "Up", cs[0].Channel)
	require.Equal(t, "large.jpg", cs[0].ThumbnailURL)
}
