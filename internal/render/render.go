// Package render mixes a voice take over a music bed — the lab's
// mini-render bench. The knob values chosen by ear here graduate into
// Phase 1's renderer defaults (spec products/radio/lab.md).
package render

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/the-algovn/radio-service/internal/ingest"
)

type Knobs struct {
	OffsetS float64 // voice starts this far into the music (default 3)
	DuckDB  float64 // music attenuation while voice speaks (default 10)
	TailS   float64 // music alone after the voice (default 3)
}

func (k Knobs) withDefaults() Knobs {
	if k.OffsetS == 0 {
		k.OffsetS = 3
	}
	if k.DuckDB == 0 {
		k.DuckDB = 10
	}
	if k.TailS == 0 {
		k.TailS = 3
	}
	return k
}

// BuildArgs is pure and deterministic: music trimmed to the preview
// window, volume-ducked exactly while the voice speaks, voice delayed by
// the offset, 1s fade-out at the end.
func BuildArgs(trackPath, voicePath, outPath string, voiceDurS float64, k Knobs) []string {
	k = k.withDefaults()
	total := k.OffsetS + voiceDurS + k.TailS
	duckGain := math.Pow(10, -k.DuckDB/20)
	offMS := int(k.OffsetS * 1000)
	fc := fmt.Sprintf(
		"[0:a]atrim=0:%.2f,volume=enable='between(t,%.2f,%.2f)':volume=%.3f[m];"+
			"[1:a]adelay=%d|%d[v];"+
			"[m][v]amix=inputs=2:duration=first:normalize=0,afade=t=out:st=%.2f:d=1.0[out]",
		total, k.OffsetS, k.OffsetS+voiceDurS, duckGain, offMS, offMS, total-1)
	return []string{
		"-y", "-i", trackPath, "-i", voicePath,
		"-filter_complex", fc, "-map", "[out]",
		"-c:a", "libmp3lame", "-q:a", "4", outPath,
	}
}

func Preview(ctx context.Context, trackPath, voicePath, outDir string, k Knobs) (string, float64, error) {
	vdur, err := ingest.Probe(ctx, voicePath)
	if err != nil {
		return "", 0, fmt.Errorf("probe voice: %w", err)
	}
	out := filepath.Join(outDir, fmt.Sprintf("render-%d.mp3", time.Now().UnixMilli()))
	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", BuildArgs(trackPath, voicePath, out, vdur, k)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", 0, fmt.Errorf("ffmpeg: %v — %.300s", err, stderr.String())
	}
	dur, err := ingest.Probe(ctx, out)
	if err != nil {
		return "", 0, err
	}
	return out, dur, nil
}
