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
}
