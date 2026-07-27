package brain

import (
	"encoding/json"
	"fmt"

	"github.com/eino-contrib/jsonschema"
)

// The output schemas handed to the providers' native structured-output modes:
// claude.WithResponseFormat and gemini.WithResponseJSONSchema both take this
// same type, so one definition serves both. Together they replace the old
// prompt-only contracts and ExtractJSON brace-scraping.

// mustSchema parses a compiled-in schema literal, panicking on malformed input
// the same way regexp.MustCompile does — a bad literal is a build-time bug, not
// a runtime condition.
func mustSchema(name, literal string) *jsonschema.Schema {
	var s jsonschema.Schema
	if err := json.Unmarshal([]byte(literal), &s); err != nil {
		panic(fmt.Sprintf("brain: %s schema is malformed: %v", name, err))
	}
	return &s
}

// ScriptSchema is the director's talk-break output (brain.Output).
var ScriptSchema = mustSchema("script", `{
  "type": "object",
  "properties": {
    "script":       {"type": "string"},
    "summary":      {"type": "string"},
    "used_phrases": {"type": "array", "items": {"type": "string"}}
  },
  "required": ["script", "summary", "used_phrases"]
}`)

// CallinSchema is the call-in moderation output (callin.Result).
var CallinSchema = mustSchema("callin", `{
  "type": "object",
  "properties": {
    "song_query":    {"type": "string"},
    "recipient":     {"type": "string"},
    "message":       {"type": "string"},
    "verdict":       {"type": "string", "enum": ["allow", "reject"]},
    "reject_reason": {"type": "string"},
    "digest":        {"type": "string"},
    "weight":        {"type": "string", "enum": ["casual", "warm", "heavy"]}
  },
  "required": ["song_query", "recipient", "message", "verdict", "reject_reason", "digest", "weight"]
}`)

// IntentSchema is programmer phase 1: intent only, deliberately no reasons —
// a reason may only be authored in the presence of a real track.
var IntentSchema = mustSchema("intent", `{
  "type": "object",
  "properties": {
    "searches":      {"type": "array", "items": {"type": "string"}},
    "library_query": {"type": "string"},
    "respins":       {"type": "array", "items": {"type": "string"}},
    "note":          {"type": "string"}
  },
  "required": ["searches", "library_query", "respins", "note"]
}`)

// ChoiceSchema is programmer phase 2: a yt_id from the candidate pool plus the
// reason for THAT track.
var ChoiceSchema = mustSchema("choice", `{
  "type": "object",
  "properties": {
    "picks": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "yt_id":  {"type": "string"},
          "reason": {"type": "string"}
        },
        "required": ["yt_id", "reason"]
      }
    }
  },
  "required": ["picks"]
}`)
