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
type Entry struct {
	YTID, Title, Artist string
	StartedAt           time.Time
	DurationS           int
}

type AirLog interface {
	Append(ctx context.Context, e Entry) error
	Latest(ctx context.Context) (Entry, bool, error)
	History(ctx context.Context, limit int) ([]Entry, error)
}

type Listeners interface {
	Beat(ctx context.Context, sessionID string) error
	Count(ctx context.Context) (int, error)
}
