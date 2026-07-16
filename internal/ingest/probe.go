package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

func Probe(ctx context.Context, path string) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "quiet", "-print_format", "json", "-show_format", path).Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe: %w", err)
	}
	var doc struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return 0, err
	}
	return strconv.ParseFloat(doc.Format.Duration, 64)
}

var loudnormJSONRe = regexp.MustCompile(`(?s)\{[^{}]*"input_i"[^{}]*\}`)

// Loudnorm runs a one-pass measurement and returns integrated loudness,
// true peak, and loudness range. ffmpeg prints the JSON block on stderr.
func Loudnorm(ctx context.Context, path string) (i, tp, lra float64, err error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-nostats", "-i", path,
		"-af", "loudnorm=I=-14:TP=-1.5:LRA=11:print_format=json", "-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, 0, 0, fmt.Errorf("ffmpeg loudnorm: %v", err)
	}
	m := loudnormJSONRe.FindString(stderr.String())
	if m == "" {
		return 0, 0, 0, fmt.Errorf("loudnorm JSON not found in ffmpeg output")
	}
	var doc struct {
		InputI   string `json:"input_i"`
		InputTP  string `json:"input_tp"`
		InputLRA string `json:"input_lra"`
	}
	if err := json.Unmarshal([]byte(m), &doc); err != nil {
		return 0, 0, 0, err
	}
	pf := func(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }
	return pf(doc.InputI), pf(doc.InputTP), pf(doc.InputLRA), nil
}
