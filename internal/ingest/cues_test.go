package ingest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCueArgsDecodesToTheAirFormat(t *testing.T) {
	got := strings.Join(CueArgs("/tmp/x.webm"), " ")

	// The air format, matching internal/live/ffmpeg.go's decode: s16le,
	// 48kHz, stereo, on stdout. A mismatch here would measure cues against
	// a different sample rate than the feeder plays at.
	require.Contains(t, got, "-f s16le")
	require.Contains(t, got, "-ar 48000")
	require.Contains(t, got, "-ac 2")
	require.Contains(t, got, "pipe:1")
	require.Contains(t, got, "-i /tmp/x.webm")

	// No loudnorm, no ebur128, no silencedetect. Cue measurement is pure Go
	// over the decoded samples; production runs ffmpeg 5.1.9, where ebur128
	// prints the literal token "nan" in the column a decay scan would read.
	require.NotContains(t, got, "loudnorm")
	require.NotContains(t, got, "ebur128")
	require.NotContains(t, got, "silencedetect")
}
