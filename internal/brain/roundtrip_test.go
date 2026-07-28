package brain

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// These replace the wire-level coverage the pre-Eino anthropic_test.go and
// gemini_test.go provided: that Generate sends the system prompt and the
// requested JSON schema, and that it extracts the reply text.

func TestClaudeGenerateRoundTrip(t *testing.T) {
	var gotBody map[string]any
	var gotKey string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		b, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(b, &gotBody))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":     []any{map[string]any{"type": "text", "text": `{"script":"ok","summary":"s","used_phrases":[]}`}},
			"usage":       map[string]any{"input_tokens": 100, "output_tokens": 20},
			"stop_reason": "end_turn",
		})
	}))
	defer ts.Close()

	m, err := newClaudeBase(context.Background(), "k", "claude-haiku-4-5-20251001", ts.URL)
	require.NoError(t, err)

	raw, err := m.Generate(context.Background(), "SYSPROMPT", "USERPROMPT", ScriptSchema)
	require.NoError(t, err)

	out, err := ParseOutput(raw)
	require.NoError(t, err)
	require.Equal(t, "ok", out.Script)

	require.Equal(t, "k", gotKey, "the API key must reach the provider")
	body, _ := json.Marshal(gotBody)
	require.Contains(t, string(body), "SYSPROMPT", "the system prompt must be sent")
	require.Contains(t, string(body), "USERPROMPT", "the user prompt must be sent")
	require.Contains(t, string(body), "used_phrases", "the response schema must be sent")
}

func TestClaudeGenerateSurfacesHTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer ts.Close()

	m, err := newClaudeBase(context.Background(), "k", "claude-haiku-4-5-20251001", ts.URL)
	require.NoError(t, err)

	_, err = m.Generate(context.Background(), "s", "u", ScriptSchema)
	require.Error(t, err, "a 5xx must surface as an error, not an empty success")
}

func TestGeminiGenerateRoundTrip(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(b, &gotBody))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": `{"script":"ok","summary":"s","used_phrases":[]}`}}},
			}},
			"usageMetadata": map[string]any{"promptTokenCount": 100, "candidatesTokenCount": 20},
		})
	}))
	defer ts.Close()

	m, err := newGeminiBase(context.Background(), "k", "gemini-2.5-flash", ts.URL)
	require.NoError(t, err)

	raw, err := m.Generate(context.Background(), "SYSPROMPT", "USERPROMPT", ScriptSchema)
	require.NoError(t, err)

	out, err := ParseOutput(raw)
	require.NoError(t, err)
	require.Equal(t, "ok", out.Script)

	require.True(t, strings.Contains(gotPath, "gemini-2.5-flash"), "model id must be in the path, got %q", gotPath)
	body, _ := json.Marshal(gotBody)
	require.Contains(t, string(body), "SYSPROMPT")
	require.Contains(t, string(body), "USERPROMPT")
	require.Contains(t, string(body), "used_phrases", "the response schema must be sent")
}

// The pre-Eino Anthropic client returned an explicit
// "output truncated at max_tokens" error (592bfcd:internal/brain/anthropic.go:66);
// the Eino migration silently dropped that check, and no test caught it because
// every stub returns stop_reason "end_turn". A truncated reply is partial JSON,
// so without this the failure surfaces as an opaque unmarshal error and the
// programmer burns a repair turn on a response that could never parse.
func TestClaudeGenerateRejectsTruncatedOutput(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			// Valid-looking but truncated JSON, exactly as a real cap-hit yields.
			"content":     []any{map[string]any{"type": "text", "text": `{"script":"đêm nay tr`}},
			"usage":       map[string]any{"input_tokens": 100, "output_tokens": 2048},
			"stop_reason": "max_tokens",
		})
	}))
	defer ts.Close()

	m, err := newClaudeBase(context.Background(), "k", "claude-haiku-4-5-20251001", ts.URL)
	require.NoError(t, err)

	_, err = m.Generate(context.Background(), "s", "u", ScriptSchema)
	require.Error(t, err, "a max_tokens truncation must be an error, not partial JSON")
	require.Contains(t, err.Error(), "truncated",
		"the error must name truncation so the cause is diagnosable in logs")
}
