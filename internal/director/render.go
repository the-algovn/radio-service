// Package director is the v2 talk-break pipeline: a prepare-ahead goroutine
// that scripts (brain), voices (voice) and renders short DJ segments, then
// hands finished clips to the live feeder through the live.TalkSource seam.
// Spec: the-algovn/specs docs/superpowers/specs/
// 2026-07-22-radio-v2-dj-talk-breaks-design.md.
package director

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/the-algovn/radio-service/internal/ingest"
)

// RenderFunc turns a synthesized take (mp3/wav) into a raw s16le/48kHz/stereo
// PCM file ready for the feeder, returning the exact duration from byte size.
// Injectable so unit tests never shell out to ffmpeg.
type RenderFunc func(ctx context.Context, inPath, outPath string) (durationS float64, err error)

const (
	bytesPerSecond = 192000 // s16le 48kHz stereo — the live feed contract
	// silenceFloorLUFS: below this measured integrated loudness the input is
	// effectively silence (voice.Fake's 1s WAV of digital silence measures
	// -inf — ffmpeg's loudnorm JSON prints "-inf" for input_i, which
	// strconv.ParseFloat parses as -Inf); linear loudnorm against an
	// -inf/-70 measurement is undefined, so plain-decode instead.
	silenceFloorLUFS = -50.0
)

// RenderArgs builds the ffmpeg render: decode anything to the air PCM format,
// normalizing speech to -16 LUFS (music airs at -14; voice sits under it).
func RenderArgs(inPath, outPath string, i, tp, lra float64, silent bool) []string {
	args := []string{"-hide_banner", "-nostats", "-y", "-i", inPath}
	if !silent {
		args = append(args, "-af", fmt.Sprintf(
			"loudnorm=I=-16:TP=-1.5:LRA=11:measured_I=%.1f:measured_TP=%.1f:measured_LRA=%.1f:linear=true",
			i, tp, lra))
	}
	return append(args, "-f", "s16le", "-ar", "48000", "-ac", "2", outPath)
}

// FFRender is the real RenderFunc: measure, then one linear-loudnorm decode.
func FFRender(ctx context.Context, inPath, outPath string) (float64, error) {
	i, tp, lra, err := ingest.Loudnorm(ctx, inPath)
	if err != nil {
		return 0, fmt.Errorf("measure: %w", err)
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", RenderArgs(inPath, outPath, i, tp, lra, i < silenceFloorLUFS)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("ffmpeg render: %v: %s", err, lastLine(stderr.String()))
	}
	fi, err := os.Stat(outPath)
	if err != nil {
		return 0, err
	}
	return float64(fi.Size()) / bytesPerSecond, nil
}

func lastLine(s string) string {
	lines := bytes.Split(bytes.TrimSpace([]byte(s)), []byte("\n"))
	if len(lines) == 0 {
		return ""
	}
	return string(lines[len(lines)-1])
}
