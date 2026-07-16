// Package ingest wraps yt-dlp + ffprobe/ffmpeg for the lab benches.
// Invocation discipline (spec products/radio/ingest.md): exec-array args,
// JSON output, timeouts, one temp dir per job.
package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"
)

type Candidate struct {
	YTID         string
	Title        string
	Channel      string
	DurationS    int64
	ViewCount    int64
	ThumbnailURL string
}

type Runner struct {
	Bin string // yt-dlp path; "yt-dlp" from PATH by default
}

type flatEntry struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Channel    string  `json:"channel"`
	Uploader   string  `json:"uploader"`
	Duration   float64 `json:"duration"`
	ViewCount  int64   `json:"view_count"`
	Thumbnails []struct {
		URL string `json:"url"`
	} `json:"thumbnails"`
}

func (r Runner) Search(ctx context.Context, query string, n int) ([]Candidate, error) {
	if n <= 0 {
		n = 10
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	out, err := r.run(ctx, fmt.Sprintf("ytsearch%d:%s", n, query), "--flat-playlist", "-J", "--no-warnings")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Entries []flatEntry `json:"entries"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("parse yt-dlp search output: %w", err)
	}
	var cs []Candidate
	for _, e := range doc.Entries {
		ch := e.Channel
		if ch == "" {
			ch = e.Uploader
		}
		c := Candidate{YTID: e.ID, Title: e.Title, Channel: ch, DurationS: int64(e.Duration), ViewCount: e.ViewCount}
		if len(e.Thumbnails) > 0 {
			c.ThumbnailURL = e.Thumbnails[len(e.Thumbnails)-1].URL
		}
		cs = append(cs, c)
	}
	return cs, nil
}

// Download fetches bestaudio into destDir and returns the file path.
func (r Runner) Download(ctx context.Context, ytID, destDir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 110*time.Second)
	defer cancel()
	tpl := filepath.Join(destDir, "%(id)s.%(ext)s")
	_, err := r.run(ctx, "https://www.youtube.com/watch?v="+ytID, "-f", "bestaudio", "-o", tpl, "--no-playlist", "--no-warnings")
	if err != nil {
		return "", err
	}
	matches, _ := filepath.Glob(filepath.Join(destDir, ytID+".*"))
	for _, m := range matches {
		if filepath.Ext(m) != ".json" && filepath.Ext(m) != ".part" {
			return m, nil
		}
	}
	return "", fmt.Errorf("download produced no audio file for %s", ytID)
}

func (r Runner) run(ctx context.Context, target string, flags ...string) ([]byte, error) {
	bin := r.Bin
	if bin == "" {
		bin = "yt-dlp"
	}
	args := append([]string{target}, flags...)
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("yt-dlp: %v — %s", err, truncate(stderr.String(), 400))
	}
	return stdout.Bytes(), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
