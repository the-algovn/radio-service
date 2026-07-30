// Package schedule is the singleton "next up" store: the one track the
// feeder has committed to air next when the request queue is otherwise
// empty (design 2026-07-23-always-scheduled-next-up). Layout mirrors
// internal/station: Store interface, MemStore, PGStore, one contract suite
// for both.
package schedule

import "context"

// NextUp is the committed next track — the one thing planNext will not let
// anything preempt (feeder.go:243-266), which is what makes it the DJ's
// promise mechanism (spec 2026-07-29-radio-dj-seam-breaks §3).
//
// RequestID is set when the committed track is a REQUEST pinned by the
// director; "" means a shuffle pick (a library re-spin), which is all
// commitNextUp ever writes. A pinned request must carry its id so planNext
// can rebuild the request's provenance and mark it aired — without it the
// track would air unattributed and the request would never leave the queue.
//
// Display fields only — the feeder re-fetches the full track by YTID.
type NextUp struct {
	YTID      string
	Title     string
	Channel   string
	RequestID string
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
