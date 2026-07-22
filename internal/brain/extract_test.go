package brain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractJSON(t *testing.T) {
	// Bare object passes through untouched.
	require.Equal(t, `{"a":1}`, ExtractJSON(`{"a":1}`))

	// Code fences are stripped.
	require.Equal(t, `{"a":1}`, ExtractJSON("```json\n{\"a\":1}\n```"))

	// Prose on either side of the object is discarded (Claude, unlike
	// Gemini's JSON mode, may narrate around the object).
	require.Equal(t, `{"a":1}`, ExtractJSON(`Đây là lựa chọn: {"a":1} Hy vọng bạn thích!`))

	// No object present: return trimmed input so the json error stays
	// meaningful ("beginning of value"), not silently blank.
	require.Equal(t, "Sáng sớm rồi", ExtractJSON("  Sáng sớm rồi  "))
}
