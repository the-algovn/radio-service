// Package schedule is the singleton "next up" store: the one track the
// feeder has committed to air next when the request queue is otherwise
// empty (design 2026-07-23-always-scheduled-next-up). Layout mirrors
// internal/station: Store interface, MemStore, PGStore, one contract suite
// for both.
package schedule

import "context"

// NextUp is the committed next track. It is always a shuffle pick (library
// re-spin); listener/AI picks live in the request queue and need no
// commitment. Display fields only — the feeder re-fetches the full track by
// YTID when it airs.
type NextUp struct {
	YTID    string
	Title   string
	Channel string
}

type Store interface {
	// GetNextUp returns the committed next track. found=false when none is
	// committed (the queue's normal state while requests are flowing).
	GetNextUp(ctx context.Context) (NextUp, bool, error)
	// SetNextUp commits n as the next track (overwrites any prior one).
	SetNextUp(ctx context.Context, n NextUp) error
	// ClearNextUp drops any committed next track.
	ClearNextUp(ctx context.Context) error
}
