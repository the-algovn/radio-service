package brain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGeminiGenerate(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Contains(t, r.URL.Path, "gemini-2.5-flash:generateContent")
		require.Equal(t, "k", r.URL.Query().Get("key"))
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.NotEmpty(t, body["system_instruction"])
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates":    []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": `{"script":"ok"}`}}}}},
			"usageMetadata": map[string]any{"promptTokenCount": 100, "candidatesTokenCount": 20},
		})
	}))
	defer ts.Close()
	g := NewGemini("k", "gemini-2.5-flash")
	g.base = ts.URL
	raw, u, err := g.Generate(context.Background(), "sys", "user")
	require.NoError(t, err)
	require.Contains(t, raw, "ok")
	require.Equal(t, Usage{In: 100, Out: 20}, u)
}
