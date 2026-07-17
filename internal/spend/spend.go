// Package spend is the lab's cost ledger. Its arithmetic is what Phase 1's
// budget cap reuses; Spec 2 moved the store from a JSONL file to Postgres
// (PGLedger) behind an interface, with MemLedger keeping bench tests hermetic.
package spend

import (
	"context"
	"time"
)

type Line struct {
	TS        time.Time
	Kind      string // tts | llm
	Provider  string // google | gemini | anthropic | fake
	Label     string
	Chars     int
	InTokens  int
	OutTokens int
	CostUSD   float64
}

// Ledger records and reads bench spend. Implementations: PGLedger (prod/local),
// MemLedger (tests).
type Ledger interface {
	Append(ctx context.Context, line Line) error
	All(ctx context.Context) ([]Line, error)
}

// Total sums cost across lines. Pure; GetLedger uses it after All.
func Total(lines []Line) float64 {
	var t float64
	for _, ln := range lines {
		t += ln.CostUSD
	}
	return t
}
