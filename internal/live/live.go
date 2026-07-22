// Package live is the radio broadcast engine: a Go feeder paces loudnorm'd
// PCM from per-track ffmpeg decoders into one persistent ffmpeg HLS encoder
// per on-air session, publishes now-playing/queue events, and records an air
// log. Spec: the-algovn/specs docs/superpowers/specs/
// 2026-07-21-radio-v0-2-stream-engine-design.md.
package live

import (
	"context"
	"time"
)

// Entry is one aired (or airing) track — denormalized at air time so history
// survives library deletes. The latest Entry is the restart resume anchor.
// Provenance (v1.1): Source ""=shuffle/legacy | "listener" | "ai";
// RequestedByName set for listener requests; Reason for AI picks.
type Entry struct {
	YTID, Title, Artist string
	StartedAt           time.Time
	DurationS           int
	Source              string
	RequestedByName     string
	Reason              string
}

type AirLog interface {
	Append(ctx context.Context, e Entry) error
	Latest(ctx context.Context) (Entry, bool, error)
	History(ctx context.Context, limit int) ([]Entry, error)
	// AiredSince reports whether ytID started airing at or after since —
	// the 2h re-request guard reads this (shuffle plays have no request row).
	AiredSince(ctx context.Context, ytID string, since time.Time) (bool, error)
	// RecentYTIDs returns the yt_ids of the latest n entries, newest first —
	// the shuffle no-repeat window and the AI-pick filters read this.
	RecentYTIDs(ctx context.Context, n int) ([]string, error)
}

type Listeners interface {
	Beat(ctx context.Context, sessionID string) error
	Count(ctx context.Context) (int, error)
}
