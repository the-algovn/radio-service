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
	"strings"
	"time"
)

type Candidate struct {
	YTID      string
	Title     string
	Channel   string
	DurationS int64
	// DurationKnown is false when yt-dlp reported no duration at all — a live
	// stream, an upcoming premiere, or a deleted entry. It is NOT the same as
	// a duration of zero, and callers must not treat it as "too short".
	DurationKnown bool
	// Live is a stream happening now or announced for later. Never a song.
	Live bool
	// ShortForm is a /shorts/ upload — capped at three minutes and almost
	// never a full track.
	ShortForm    bool
	ViewCount    int64
	ThumbnailURL string
}

type Runner struct {
	Bin string // yt-dlp path; "yt-dlp" from PATH by default
}

type flatEntry struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Channel  string `json:"channel"`
	Uploader string `json:"uploader"`
	// Pointer so an explicit null and an absent field are both distinguishable
	// from a real zero. This is the whole point of the type.
	Duration   *float64 `json:"duration"`
	ViewCount  int64    `json:"view_count"`
	LiveStatus string   `json:"live_status"`
	URL        string   `json:"url"`
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
	return candidatesFrom(out)
}

// candidatesFrom maps yt-dlp's flat-playlist JSON onto Candidates. Split out of
// Search so the mapping — which is where the null-duration bug lived — is
// testable against fixtures without a network.
func candidatesFrom(raw []byte) ([]Candidate, error) {
	var doc struct {
		Entries []flatEntry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse yt-dlp search output: %w", err)
	}
	var cs []Candidate
	for _, e := range doc.Entries {
		ch := e.Channel
		if ch == "" {
			ch = e.Uploader
		}
		c := Candidate{
			YTID: e.ID, Title: e.Title, Channel: ch, ViewCount: e.ViewCount,
			Live:      e.LiveStatus == "is_live" || e.LiveStatus == "is_upcoming",
			ShortForm: strings.Contains(e.URL, "/shorts/"),
		}
		if e.Duration != nil {
			c.DurationS, c.DurationKnown = int64(*e.Duration), true
		}
		if len(e.Thumbnails) > 0 {
			c.ThumbnailURL = e.Thumbnails[len(e.Thumbnails)-1].URL
		}
		cs = append(cs, c)
	}
	return cs, nil
}

// Download fetches bestaudio into destDir and returns the file path plus the
// music metadata from the same extraction. -f bestaudio already forces a full
// extraction, so --write-info-json costs no extra request.
func (r Runner) Download(ctx context.Context, ytID, destDir string) (string, Meta, error) {
	ctx, cancel := context.WithTimeout(ctx, 110*time.Second)
	defer cancel()
	tpl := filepath.Join(destDir, "%(id)s.%(ext)s")
	_, err := r.run(ctx, "https://www.youtube.com/watch?v="+ytID,
		"-f", "bestaudio", "-o", tpl, "--no-playlist", "--no-warnings", "--write-info-json")
	if err != nil {
		return "", Meta{}, err
	}
	matches, _ := filepath.Glob(filepath.Join(destDir, ytID+".*"))
	for _, m := range matches {
		if filepath.Ext(m) != ".json" && filepath.Ext(m) != ".part" {
			return m, readMeta(destDir, ytID), nil
		}
	}
	return "", Meta{}, fmt.Errorf("download produced no audio file for %s", ytID)
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
