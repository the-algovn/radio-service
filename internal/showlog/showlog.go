// Package showlog is the show log: what actually AIRED, merged across music
// and the AI MC's talk breaks.
//
// internal/live's air_log stays music-only — five consumers depend on that
// (boot resume anchor, the 2h re-request guard, the shuffle no-repeat window,
// the DJ's own brief, History). Talk breaks get their own table instead, and
// the two are read back as one ordered sequence. They are disjoint by kind,
// never duplicates, so the two records cannot diverge.
//
// Layout mirrors internal/talkmem: Store interface, MemStore, PGStore, one
// contract suite for both.
package showlog

import (
	"context"
	"time"
)

// Clip kinds. These mirror live.ClipSeam and live.ClipStationID by value and
// are redeclared here rather than imported: internal/live imports THIS package
// (for FeederDeps.TalkSegments), so the dependency runs one way only.
const (
	KindSeam      = "seam"
	KindStationID = "station_id"
)

// Row origins — which table a Segment came from.
const (
	OriginAir  = "air"
	OriginTalk = "talk"
)

// Paging bounds. BOTH stores apply them, so the backends cannot drift.
const (
	defaultLimit = 50
	maxLimit     = 200
)

// Segment is one item of the merged show log. Music rows carry YTID/Artist and
// air_log's provenance; talk rows carry the script and the LLM provenance
// joined from llm_call. Fields that do not apply to a row are zero.
type Segment struct {
	ID        int64
	Origin    string // OriginAir | OriginTalk — which table
	Kind      string // "track" | KindSeam | KindStationID
	YTID      string
	Title     string
	Artist    string
	StartedAt time.Time
	DurationS int

	// air_log provenance
	Source          string // "" | "listener" | "ai"
	RequestedByName string
	Reason          string

	// talk_segment
	Script        string
	BacksellTitle string
	PromiseTitle  string
	CorrelationID string

	// llm_call, summed across every attempt sharing the correlation id
	Model     string
	InTokens  int
	OutTokens int
	CostUSD   float64
	LatencyMS int
}

// clampLimit is the shared paging rule.
func clampLimit(limit int) int {
	if limit <= 0 {
		return defaultLimit
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}

// MusicFunc supplies the music half of the show log to MemStore, newest-first
// order not required. It exists because air_log lives in internal/live, which
// imports THIS package — so a mem store cannot read music directly without a
// cycle. A plain function value couples nothing. nil means talk-only.
type MusicFunc func(ctx context.Context) ([]Segment, error)

// Talk is one aired talk segment, recorded when the clip opens — the same
// instant air_log records a track, and for the same reason. A break cut short
// by an operator skip therefore still appears, at its intended duration, which
// is exactly the semantics music already has.
type Talk struct {
	Kind          string // KindSeam | KindStationID
	StartedAt     time.Time
	DurationS     int
	Script        string
	BacksellTitle string // the track she talked back over ("" for a station ID)
	PromiseTitle  string // the track she promised ("" when she promised nothing)
	CorrelationID string // links to llm_call; "" for a station ID (no LLM call)
}

type Store interface {
	// Append records one aired talk segment.
	Append(ctx context.Context, t Talk) error
	// Recent returns the merged show log, newest first. limit <= 0 means
	// defaultLimit and is capped at maxLimit — never unbounded, because the
	// gateway hard-caps responses at 4 MiB with no paging of its own.
	Recent(ctx context.Context, limit, offset int) ([]Segment, error)
	// Count is the total row count across both tables, for paging.
	Count(ctx context.Context) (int64, error)
}
