package callin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

var nameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// SaveFixture records a reviewer-blessed case into the committed corpus
// (internal/callin/testdata/fixtures/) — Phase 1's regression tests.
func SaveFixture(dir, name, rawText, expectedJSON string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("fixture name must be kebab-case, got %q", name)
	}
	if !json.Valid([]byte(expectedJSON)) {
		return "", fmt.Errorf("expected_json is not valid JSON")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	doc := map[string]json.RawMessage{
		"raw_text": mustJSON(rawText),
		"expected": json.RawMessage(expectedJSON),
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	p := filepath.Join(dir, name+".json")
	return p, os.WriteFile(p, append(b, '\n'), 0o644)
}

func mustJSON(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
