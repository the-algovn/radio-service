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

// notMusicCategories are YouTube categories that are unambiguously not a
// song. The category is uploader-chosen and defaults to "People & Blogs", so
// this must be a denylist, not an allowlist of "Music": a large share of
// genuine Vietnamese indie, OST and cover uploads sit under "Entertainment",
// "People & Blogs" or "Film & Animation". Rejecting anything that wasn't
// literally "Music" threw away those real songs after paying to download and
// decode them, and since a rejected acquire never reaches the library, the
// same track got re-downloaded and re-rejected forever.
var notMusicCategories = map[string]bool{
	"news & politics":       true,
	"education":             true,
	"science & technology":  true,
	"sports":                true,
	"gaming":                true,
	"autos & vehicles":      true,
	"pets & animals":        true,
	"travel & events":       true,
	"howto & style":         true,
	"nonprofits & activism": true,
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
		if notMusicCategories[strings.ToLower(c)] {
			return true, "YouTube xếp loại: " + strings.Join(m.Categories, ", ")
		}
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
