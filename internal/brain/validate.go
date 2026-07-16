package brain

import (
	"fmt"
	"regexp"
	"unicode/utf8"
)

var digitRe = regexp.MustCompile(`[0-9]`)

// Validate enforces the on-air script rules: no numerals (TTS reads them
// unreliably in Vietnamese — everything is written out in words), and a
// hard character budget.
func Validate(script string, maxChars int) []string {
	var v []string
	if digitRe.MatchString(script) {
		v = append(v, "digit-lint: script contains numerals — numbers must be written out in words")
	}
	if n := utf8.RuneCountInString(script); maxChars > 0 && n > maxChars {
		v = append(v, fmt.Sprintf("length: %d chars exceeds budget %d", n, maxChars))
	}
	return v
}
