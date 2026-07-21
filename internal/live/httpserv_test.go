package live

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHLSHandler(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "live.m3u8"), []byte("#EXTM3U\nseg-0.ts\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "seg-0.ts"), []byte("TS"), 0o644))

	current := dir
	h := NewHLSHandler(func() string { return current })
	srv := httptest.NewServer(h)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/live/radio.m3u8")
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode)
	require.Equal(t, "no-cache", res.Header.Get("Cache-Control"))
	require.Equal(t, "*", res.Header.Get("Access-Control-Allow-Origin"))
	require.Contains(t, res.Header.Get("Content-Type"), "mpegurl")

	res, err = http.Get(srv.URL + "/live/seg-0.ts")
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode)
	require.Contains(t, res.Header.Get("Cache-Control"), "immutable")

	// 404s: unknown file, traversal, directory
	for _, p := range []string{"/live/nope.ts", "/live/../secrets", "/live/", "/other"} {
		res, err = http.Get(srv.URL + p)
		require.NoError(t, err)
		require.Equal(t, 404, res.StatusCode, p)
	}

	// POST → 405
	res, err = http.Post(srv.URL+"/live/radio.m3u8", "", nil)
	require.NoError(t, err)
	require.Equal(t, 405, res.StatusCode)

	// off-air → 404
	current = ""
	res, err = http.Get(srv.URL + "/live/radio.m3u8")
	require.NoError(t, err)
	require.Equal(t, 404, res.StatusCode)
}
