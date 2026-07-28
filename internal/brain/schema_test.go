package brain_test

import (
	"encoding/json"
	"fmt"
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

// Anthropic rejects a structured-output request whose schema has an object
// without an explicit "additionalProperties": false:
//
//	output_config.format.schema: For 'object' type, 'additionalProperties'
//	must be explicitly set to false
//
// This shipped broken once because the round-trip tests use an httptest stub,
// and a stub accepts any schema — only the real API enforces this. The guard
// therefore inspects the MARSHALLED schema (what actually goes on the wire)
// and recurses, so a nested object added later cannot slip through.
func TestSchemasDeclareAdditionalPropertiesFalse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema any
	}{
		{"script", brain.ScriptSchema},
		{"callin", brain.CallinSchema},
		{"intent", brain.IntentSchema},
		{"choice", brain.ChoiceSchema},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.schema)
			require.NoError(t, err)
			var doc any
			require.NoError(t, json.Unmarshal(b, &doc))

			var objects int
			var walk func(node any, path string)
			walk = func(node any, path string) {
				m, ok := node.(map[string]any)
				if !ok {
					if arr, isArr := node.([]any); isArr {
						for i, v := range arr {
							walk(v, fmt.Sprintf("%s[%d]", path, i))
						}
					}
					return
				}
				if m["type"] == "object" {
					objects++
					ap, present := m["additionalProperties"]
					require.True(t, present,
						"object at %s has no additionalProperties; Anthropic will reject the request", path)
					require.Equal(t, false, ap,
						"object at %s must set additionalProperties to exactly false, got %v", path, ap)
				}
				for k, v := range m {
					walk(v, path+"/"+k)
				}
			}
			walk(doc, tc.name)
			require.Positive(t, objects, "walk found no objects — the guard is not actually inspecting anything")
		})
	}
}
