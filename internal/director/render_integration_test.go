//go:build integration

package director

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
}

// measureRawLUFS measures integrated loudness of a raw air-format PCM file
// (ingest.Loudnorm can't — raw PCM needs explicit input format flags).
func measureRawLUFS(t *testing.T, path string) float64 {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-hide_banner", "-nostats",
		"-f", "s16le", "-ar", "48000", "-ac", "2", "-i", path,
		"-af", "loudnorm=I=-16:TP=-1.5:LRA=11:print_format=json", "-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Run())
	m := regexp.MustCompile(`"input_i"\s*:\s*"([-\d.]+)"`).FindStringSubmatch(stderr.String())
	require.NotNil(t, m, "loudnorm JSON not found: %s", stderr.String())
	f, err := strconv.ParseFloat(m[1], 64)
	require.NoError(t, err)
	return f
}

// The audio golden test (products/radio.md): render a speech-shaped mp3,
// assert duration and integrated LUFS within tolerance — no human ear in CI.
func TestFFRenderToneToMinus16(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.mp3")
	gen := exec.Command("ffmpeg", "-hide_banner", "-nostats", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3", "-b:a", "48k", in)
	require.NoError(t, gen.Run())

	out := filepath.Join(dir, "out.pcm")
	dur, err := FFRender(context.Background(), in, out)
	require.NoError(t, err)
	require.InDelta(t, 3.0, dur, 0.2)
	require.InDelta(t, -16.0, measureRawLUFS(t, out), 1.5)
}

// Silence (the voice.Fake path) takes the plain-decode branch and still
// yields a valid air-format file.
func TestFFRenderSilencePlainDecodes(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "silence.wav")
	gen := exec.Command("ffmpeg", "-hide_banner", "-nostats", "-y",
		"-f", "lavfi", "-i", "anullsrc=r=8000:cl=mono:d=1", in)
	require.NoError(t, gen.Run())

	out := filepath.Join(dir, "out.pcm")
	dur, err := FFRender(context.Background(), in, out)
	require.NoError(t, err)
	require.InDelta(t, 1.0, dur, 0.2)
}
