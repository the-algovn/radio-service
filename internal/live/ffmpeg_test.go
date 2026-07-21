package live

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeArgs(t *testing.T) {
	args := DecodeArgs("/tmp/track.webm", Loudness{I: -9.3, TP: -0.2, LRA: 4.1}, 0)
	joined := strings.Join(args, " ")
	require.Contains(t, joined, "-i /tmp/track.webm")
	require.Contains(t, joined,
		"loudnorm=I=-14:TP=-1.5:LRA=11:measured_I=-9.3:measured_TP=-0.2:measured_LRA=4.1:linear=true")
	require.Contains(t, joined, "-f s16le -ar 48000 -ac 2 pipe:1")
	require.NotContains(t, joined, "-ss") // no seek without offset
	require.NotContains(t, joined, "-re") // batch decode; the feeder paces
}

func TestDecodeArgsWithOffset(t *testing.T) {
	args := DecodeArgs("/tmp/track.webm", Loudness{I: -9, TP: -1, LRA: 5}, 12.5)
	joined := strings.Join(args, " ")
	require.Contains(t, joined, "-ss 12.500")
	// -ss must precede -i for fast input seeking
	require.Less(t, strings.Index(joined, "-ss"), strings.Index(joined, "-i "))
}

func TestEncodeArgs(t *testing.T) {
	args := EncodeArgs("/data/hls/s1", 6)
	joined := strings.Join(args, " ")
	require.Contains(t, joined, "-f s16le -ar 48000 -ac 2 -i pipe:0")
	require.Contains(t, joined, "-c:a aac -b:a 128k")
	require.Contains(t, joined, "-f hls -hls_time 6 -hls_list_size 6")
	require.Contains(t, joined, "-hls_flags delete_segments+program_date_time")
	require.Contains(t, joined, "/data/hls/s1/seg-%d.ts")
	require.Contains(t, joined, "/data/hls/s1/live.m3u8")
}
