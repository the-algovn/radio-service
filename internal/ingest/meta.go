package ingest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Meta is the music-identifying half of yt-dlp's full extraction. Download
// already performs that extraction to resolve -f bestaudio, so writing the
// info-json sidecar costs no additional network request.
type Meta struct {
	Categories []string `json:"categories"`
	Track      string   `json:"track"`
	Artists    []string `json:"artists"`
	Album      string   `json:"album"`
	MediaType  string   `json:"media_type"`
}

// NotMusic reports positive evidence that this is not a song, plus a reason.
//
// It fails OPEN on purpose: absent metadata returns false. Rejecting on missing
// data is exactly the mistake that made unknown durations empty the candidate
// pool, and it would be worse here — the bytes are already downloaded.
func (m Meta) NotMusic() (bool, string) {
	switch m.MediaType {
	case "livestream":
		return true, "livestream, không phải bài hát"
	case "short":
		return true, "video ngắn, không phải bài hát đầy đủ"
	}
	// Auto-generated "Music in this video" metadata is near-certain evidence
	// that this is one specific released song, whatever the category says.
	if m.Track != "" {
		return false, ""
	}
	for _, c := range m.Categories {
		if strings.EqualFold(c, "Music") {
			return false, ""
		}
	}
	// A category YouTube positively assigned, and it isn't Music.
	if len(m.Categories) > 0 {
		return true, "YouTube xếp loại: " + strings.Join(m.Categories, ", ")
	}
	return false, ""
}

func metaFrom(raw []byte) (Meta, error) {
	var m Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		return Meta{}, fmt.Errorf("parse yt-dlp info json: %w", err)
	}
	return m, nil
}

// readMeta loads the sidecar yt-dlp wrote next to the audio file. A missing or
// unparseable sidecar yields a zero Meta and no error — the caller fails open.
func readMeta(destDir, ytID string) Meta {
	b, err := os.ReadFile(filepath.Join(destDir, ytID+".info.json"))
	if err != nil {
		return Meta{}
	}
	m, err := metaFrom(b)
	if err != nil {
		return Meta{}
	}
	return m
}
