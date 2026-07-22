package brain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// Anthropic has no JSON mode, so Generate must prefill an assistant turn of
// "{" to force a JSON object, then restore the leading brace on the reply.
func TestAnthropicPrefillsJSONObject(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct{ Role, Content string }
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Len(t, body.Messages, 2)
		require.Equal(t, "assistant", body.Messages[1].Role)
		require.Equal(t, "{", body.Messages[1].Content)
		// The reply is the continuation after the prefilled "{".
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":     []any{map[string]any{"text": `"script":"ok"}`}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 100, "output_tokens": 20},
		})
	}))
	defer ts.Close()
	a := NewAnthropic("k", "claude-haiku-4-5")
	a.base = ts.URL
	raw, u, err := a.Generate(context.Background(), "sys", "user")
	require.NoError(t, err)
	require.Equal(t, `{"script":"ok"}`, raw)
	require.Equal(t, Usage{In: 100, Out: 20}, u)
}

// A response cut off at the token cap must surface as an explicit truncation
// error, not a mislabeled JSON parse failure downstream.
func TestAnthropicTruncationIsAnError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":     []any{map[string]any{"text": `"script":"Sáng sớm rồi, năm giờ`}},
			"stop_reason": "max_tokens",
			"usage":       map[string]any{"input_tokens": 100, "output_tokens": 2048},
		})
	}))
	defer ts.Close()
	a := NewAnthropic("k", "claude-haiku-4-5")
	a.base = ts.URL
	_, _, err := a.Generate(context.Background(), "sys", "user")
	require.Error(t, err)
	require.Contains(t, err.Error(), "truncated")
}
