package voice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGoogleSynthesizeRequestShape(t *testing.T) {
	var got map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/text:synthesize", r.URL.Path)
		require.Equal(t, "test-key", r.URL.Query().Get("key"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		_ = json.NewEncoder(w).Encode(map[string]string{
			"audioContent": base64.StdEncoding.EncodeToString([]byte("mp3bytes")),
		})
	}))
	defer ts.Close()

	g := NewGoogle("test-key")
	g.base = ts.URL
	data, ext, err := g.Synthesize(context.Background(), "chào buổi tối", "vi-VN-Neural2-A", 1.05)
	require.NoError(t, err)
	require.Equal(t, "mp3", ext)
	require.Equal(t, []byte("mp3bytes"), data)

	voice := got["voice"].(map[string]any)
	require.Equal(t, "vi-VN", voice["languageCode"])
	require.Equal(t, "vi-VN-Neural2-A", voice["name"])
	audio := got["audioConfig"].(map[string]any)
	require.Equal(t, "MP3", audio["audioEncoding"])
	require.InDelta(t, 1.05, audio["speakingRate"].(float64), 1e-9)
}
