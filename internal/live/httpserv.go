package live

import (
	"net/http"
	"path/filepath"
	"regexp"
)

var segRe = regexp.MustCompile(`^seg-\d+\.ts$`)

// NewHLSHandler serves the live session's manifest and segments read-only.
// sessionDir returns the CURRENT session dir, or "" when off-air (404).
// Only two shapes exist: /live/radio.m3u8 (the manifest, no-cache — the SPA
// polls it) and /live/seg-<n>.ts (immutable). Everything else 404s; the
// allowlist doubles as traversal protection.
func NewHLSHandler(sessionDir func() string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		dir := sessionDir()
		if dir == "" {
			http.NotFound(w, r)
			return
		}
		switch {
		case r.URL.Path == "/live/radio.m3u8":
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			http.ServeFile(w, r, filepath.Join(dir, "live.m3u8"))
		case len(r.URL.Path) > len("/live/") && segRe.MatchString(r.URL.Path[len("/live/"):]) &&
			r.URL.Path[:len("/live/")] == "/live/":
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			w.Header().Set("Content-Type", "video/mp2t")
			http.ServeFile(w, r, filepath.Join(dir, r.URL.Path[len("/live/"):]))
		default:
			http.NotFound(w, r)
		}
	})
}
