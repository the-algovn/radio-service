package ingest

import (
	"sort"
	"strings"
)

type Scored struct {
	Candidate
	Score int
	Notes []string
}

var penaltyTokens = []string{"live", "cover", "remix", "karaoke", "sped up", "nightcore", "8d", "mix", "compilation", "1 hour", "lofi ver"}

// Rank orders search candidates per the ingest spec: prefer Topic/official
// canonical audio; penalize variants the query didn't ask for, absurd
// durations, and near-zero traction. Human confirm is the ground truth —
// failures here are papercuts, never corruption.
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
		if strings.Contains(channel, "official") || strings.Contains(title, "official") {
			add(20, "bonus:official")
		}
		// Collect matched penalty tokens (not in query)
		matched := []string{}
		for _, tok := range penaltyTokens {
			if strings.Contains(title, tok) && !strings.Contains(q, tok) {
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
		// Apply penalty for non-overlapping tokens
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
