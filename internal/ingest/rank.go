package ingest

import (
	"sort"
	"strings"
	"unicode"
)

type Scored struct {
	Candidate
	Score int
	Notes []string
}

// variantTokens mark a version of a song the query did not ask for.
var variantTokens = []string{
	"live", "cover", "remix", "karaoke", "sped up", "nightcore", "8d",
	"mix", "compilation", "1 hour", "lofi ver",
	// Vietnamese compilation vocabulary — what a mood-shaped query actually returns.
	"tuyển tập", "liên khúc", "nonstop", "tổng hợp", "mashup",
}

// formatTokens mark something that is not a song at all. Unlike variantTokens
// these are never exempted by the query: a query cannot legitimately ask the
// station to play a podcast episode.
var formatTokens = []string{
	"podcast", "interview", "phỏng vấn", "reaction", "review", "trailer",
	"teaser", "tutorial", "asmr", "vlog", "gameplay", "audiobook", "talk show",
}

// containsToken reports whether s contains tok delimited by non-alphanumeric
// runes. A bare strings.Contains matched "live" inside "Deliver" and "Olive",
// and the boundary must be punctuation-aware or "(Live)" and "[Remix]" stop
// scoring at all.
func containsToken(s, tok string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], tok)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(tok)
		if isBoundary(s, start-1) && isBoundary(s, end) {
			return true
		}
		i = start + 1
		if i >= len(s) {
			return false
		}
	}
}

// isBoundary reports whether the rune at byte offset i is a delimiter. Offsets
// outside the string are boundaries, so a token at either end matches.
func isBoundary(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return true
	}
	r := rune(s[i])
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

// Rank orders search candidates per the ingest spec: prefer Topic/official
// canonical audio; penalize variants the query didn't ask for, formats that
// aren't music, absurd durations, and near-zero traction. Human confirm is the
// ground truth — failures here are papercuts, never corruption.
//
// Rank only ORDERS. It never removes a candidate; rejection is the programmer's
// job, so that a bad ranking can never empty the pool.
func Rank(query string, cs []Candidate) []Scored {
	q := strings.ToLower(query)
	out := make([]Scored, 0, len(cs))
	for _, c := range cs {
		s := Scored{Candidate: c}
		title := strings.ToLower(c.Title)
		channel := strings.ToLower(c.Channel)
		add := func(delta int, note string) {
			s.Score += delta
			s.Notes = append(s.Notes, note)
		}
		if strings.HasSuffix(strings.TrimSpace(channel), "- topic") {
			add(30, "bonus:topic-channel")
		}
		// The "official" bonus must not fire on Official Trailer / Teaser /
		// Behind The Scenes, which are not music but used to score +20 and
		// outrank a real unlabelled song at 0.
		officialish := strings.Contains(channel, "official") || strings.Contains(title, "official")
		if officialish && !hasAny(title, formatTokens) && !strings.Contains(title, "behind the scenes") {
			add(20, "bonus:official")
		}
		// Format tokens are never exempted by the query.
		for _, tok := range formatTokens {
			if containsToken(title, tok) {
				add(-60, "penalty:"+tok)
			}
		}
		// Collect matched variant tokens the query did not itself ask for.
		matched := []string{}
		for _, tok := range variantTokens {
			if containsToken(title, tok) && !strings.Contains(q, tok) {
				matched = append(matched, tok)
			}
		}
		// Remove tokens that are substrings of other (longer) matched tokens
		kept := []string{}
		for _, tok := range matched {
			isSubstring := false
			for _, other := range matched {
				if len(other) > len(tok) && strings.Contains(other, tok) {
					isSubstring = true
					break
				}
			}
			if !isSubstring {
				kept = append(kept, tok)
			}
		}
		for _, tok := range kept {
			add(-25, "penalty:"+tok)
		}
		switch {
		case c.DurationS > 480:
			add(-20, "penalty:too-long")
		case c.DurationS > 0 && c.DurationS < 60:
			add(-40, "penalty:too-short")
		}
		if c.ViewCount > 0 && c.ViewCount < 1000 {
			add(-10, "penalty:low-views")
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

func hasAny(s string, toks []string) bool {
	for _, t := range toks {
		if containsToken(s, t) {
			return true
		}
	}
	return false
}
