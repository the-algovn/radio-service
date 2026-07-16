package artifact

import (
	"net/http"
	"strings"
)

var contentTypes = map[string]string{
	"mp3": "audio/mpeg", "wav": "audio/wav", "m4a": "audio/mp4",
	"webm": "audio/webm", "opus": "audio/ogg", "json": "application/json",
}

// Handler serves GET /artifacts/{id} and GET /healthz. Dev-only server:
// permissive CORS so the console (5174) can fetch bytes directly.
func Handler(s Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/artifacts/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		id := strings.TrimPrefix(r.URL.Path, "/artifacts/")
		a, err := s.Get(id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if ct, ok := contentTypes[a.Ext]; ok {
			w.Header().Set("Content-Type", ct)
		}
		p, _ := s.Path(id)
		http.ServeFile(w, r, p)
	})
	return mux
}
