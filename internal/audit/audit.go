// Package audit records every LLM call (prompts, output, tokens, cost,
// latency, error) for the console inspector. Layout mirrors internal/station:
// Store interface, MemStore, PGStore, one contract suite for both.
package audit

import (
	"context"
	"time"
)

type Rec struct {
	ID        int64
	TS        time.Time
	Label     string // director:seam | director:station_id | programmer:pick | script:<type> | callin
	// CorrelationID groups every call made for one unit of work — one
	// director prepare makes up to two (the script validation loop retries
	// once). Set from ctx by the callback; "" for call sites that set none.
	CorrelationID string
	Model     string // full model id
	Provider  string // anthropic | gemini | fake
	System    string
	User      string
	Output    string // "" on error
	InTokens  int
	OutTokens int
	CostUSD   float64
	LatencyMS int
	Error     string // "" on success
	Fake      bool
}

type Filter struct {
	Label         string // "" = all
	CorrelationID string // "" = all
	ErrorsOnly    bool
}

type Stat struct {
	Label, Model        string
	Count               int
	InTokens, OutTokens int
	CostUSD             float64
}

// Recorder is the write side the decorator needs (interface-segregation: the
// decorator must not depend on the read methods).
type Recorder interface {
	Record(ctx context.Context, r Rec) error
}

type Store interface {
	Recorder
	List(ctx context.Context, f Filter, limit, offset int) ([]Rec, error)
	Count(ctx context.Context, f Filter) (int64, error)
	Stats(ctx context.Context, since time.Time) ([]Stat, error)
}
