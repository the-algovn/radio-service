package render

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildArgsFilterGraph(t *testing.T) {
	args := BuildArgs("track.m4a", "voice.mp3", "out.mp3", 8.0, Knobs{OffsetS: 5, DuckDB: 9, TailS: 4})
	joined := strings.Join(args, " ")
	require.Contains(t, joined, "-i track.m4a")
	require.Contains(t, joined, "-i voice.mp3")
	// total = 5 + 8 + 4 = 17s; duck window 5..13; -9dB ≈ gain 0.355
	require.Contains(t, joined, "atrim=0:17.00")
	require.Contains(t, joined, "between(t,5.00,13.00)")
	require.Contains(t, joined, "volume=0.355")
	require.Contains(t, joined, "adelay=5000|5000")
	require.Contains(t, joined, "afade=t=out:st=16.00:d=1.0")
	require.Equal(t, "out.mp3", args[len(args)-1])
}

func TestBuildArgsDefaults(t *testing.T) {
	args := BuildArgs("t", "v", "o", 10.0, Knobs{})
	joined := strings.Join(args, " ")
	require.Contains(t, joined, "atrim=0:16.00") // defaults offset 3, tail 3
	require.Contains(t, joined, "volume=0.316")  // default duck 10dB
}
