// Package songkey folds an artist and title into a stable identity for one
// song, so the same recording uploaded as an official MV, a "- Topic" track and
// a lyric video is recognised as one thing. Every dedup guard in the station
// keys on the YouTube video id, which those three uploads defeat.
package songkey

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

var (
	bracketed = regexp.MustCompile(`[\(\[\{][^\)\]\}]*[\)\]\}]`)
	nonAlnum  = regexp.MustCompile(`[^a-z0-9]+`)
	separator = regexp.MustCompile(`\s[-–—|]\s`)
)

// Of returns the folded identity of one song, or "" when either side folds away
// to nothing — the sentinel for "not computed", matching the unmeasured-is-not-
// zero convention migration 00012 established for track cues.
func Of(artist, title string) string {
	a, t := fold(artist), fold(title)
	if a == "" || t == "" {
		return ""
	}
	return a + "/" + t
}

func fold(s string) string {
	s = strings.ToLower(s)
	// A "- Topic" channel is the same artist as the bare name.
	s = strings.TrimSuffix(strings.TrimSpace(s), "- topic")
	// Drop parenthesised/bracketed qualifiers: (Official MV), [Lyric Video].
	s = bracketed.ReplaceAllString(s, " ")
	// Keep the longest segment around a separator: "Song - Audio" → "song".
	if parts := separator.Split(s, -1); len(parts) > 1 {
		longest := ""
		for _, p := range parts {
			if len(strings.TrimSpace(p)) > len(longest) {
				longest = strings.TrimSpace(p)
			}
		}
		s = longest
	}
	// đ does not decompose under NFD, so it needs an explicit mapping BEFORE
	// the combining marks are stripped.
	s = strings.NewReplacer("đ", "d", "Đ", "d").Replace(s)
	// Stripping combining marks (Mn) also strips Vietnamese tone marks, so
	// distinct words that differ only by tone — chờ, chợ, cho — collapse to
	// the same key. That is accepted, not a bug: this key backs a non-unique
	// index used as a soft dedup signal, so a false merge only costs variety
	// (one song occasionally skipped as a near-duplicate). Preserving tones
	// would let real duplicates slip through, which is worse.
	res, _, err := transform.String(
		transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC), s)
	if err == nil {
		s = res
	}
	s = nonAlnum.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}
