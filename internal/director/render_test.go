package director

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRenderArgsSpeechLoudnorm(t *testing.T) {
	args := strings.Join(RenderArgs("in.mp3", "out.pcm", -23.4, -5.0, 3.2, false), " ")
	require.Contains(t, args, "loudnorm=I=-16:TP=-1.5:LRA=11:measured_I=-23.4:measured_TP=-5.0:measured_LRA=3.2:linear=true")
	require.Contains(t, args, "-f s16le -ar 48000 -ac 2 out.pcm")
}

func TestRenderArgsSilenceSkipsLoudnorm(t *testing.T) {
	args := strings.Join(RenderArgs("in.wav", "out.pcm", -70, 0, 0, true), " ")
	require.NotContains(t, args, "loudnorm")
	require.Contains(t, args, "-f s16le -ar 48000 -ac 2 out.pcm")
}
