package brain_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/the-algovn/radio-service/internal/brain"
)

// The schemas are handed verbatim to Anthropic and Gemini, so they must be
// marshalable and must name every field the parsers read.
func TestSchemasMarshalAndNameTheirFields(t *testing.T) {
	cases := []struct {
		name   string
		schema any
		fields []string
	}{
		{"script", brain.ScriptSchema, []string{"script", "summary", "used_phrases"}},
		{"callin", brain.CallinSchema, []string{"song_query", "recipient", "message", "verdict", "reject_reason", "digest", "weight"}},
		{"intent", brain.IntentSchema, []string{"searches", "library_query", "respins", "note"}},
		{"choice", brain.ChoiceSchema, []string{"picks", "yt_id", "reason"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.schema)
			require.NoError(t, err)
			for _, f := range tc.fields {
				require.Contains(t, string(b), `"`+f+`"`, "schema must mention %q", f)
			}
		})
	}
}
