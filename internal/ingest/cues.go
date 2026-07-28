package ingest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/the-algovn/radio-service/internal/live/pcm"
)

// CueArgs builds the decode that feeds the tail scan: whatever the source
// container is, out comes raw s16le at the air format on stdout. Deliberately
// the same shape internal/live/ffmpeg.go already trusts, so a cue measured
// here maps onto the feeder's sample clock with no correction.
func CueArgs(path string) []string {
	return []string{"-hide_banner", "-nostats", "-i", path,
		"-f", "s16le", "-ar", "48000", "-ac", "2", "pipe:1"}
}

// Cues measures a track's tail-silence and tail-decay by decoding it once and
// scanning the samples in Go.
//
// This is a separate entry point from Loudnorm on purpose. Loudnorm is also
// called by internal/director/render.go for every DJ talk break, where its
// error silently kills the break — widening it to also carry cue measurement
// would put an ingest-only failure on air as dead air.
//
// It also deliberately does not use ffmpeg's own ebur128/silencedetect
// filters: production runs ffmpeg 5.1.9, where ebur128 prints the literal
// token "nan" in the short-term column, and any stderr-parsing approach fails
// on the only runtime that matters.
func Cues(ctx context.Context, path string) (tailSilenceS, tailDecayS float64, err error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffmpeg", CueArgs(path)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, 0, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return 0, 0, fmt.Errorf("ffmpeg start: %w", err)
	}

	sil, dec, scanErr := pcm.TailCues(stdout, 48000, 2)

	// Drain anything the scan left, or ffmpeg blocks writing into a full
	// pipe and Wait never returns. TailCues reads to EOF on success, so this
	// only does work on the error path — but on that path it is the
	// difference between an error and a hung ingest worker.
	_, _ = io.Copy(io.Discard, stdout)

	if werr := cmd.Wait(); werr != nil {
		return 0, 0, fmt.Errorf("ffmpeg decode: %v: %s", werr, lastStderrLine(stderr.String()))
	}
	if scanErr != nil {
		return 0, 0, scanErr
	}
	return sil, dec, nil
}

// lastStderrLine trims ffmpeg's output to its final non-empty line, which is
// where the actual failure is. Deliberately a local copy: the equivalent
// helper lives in internal/director (render.go), and importing the director
// from ingest would invert the dependency — the director already imports
// ingest.
func lastStderrLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
