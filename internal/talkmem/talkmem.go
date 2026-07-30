// Package talkmem is the DJ's show memory: one self-summary per aired talk
// break, persisted so the night's narrative arc survives a pod restart. The
// feeder already resumes mid-track from the air log; before this the DJ's
// memory was the only thing a restart silently lost.
//
// Layout mirrors internal/schedule and internal/request: Store interface,
// MemStore, PGStore, one contract suite for both.
package talkmem

import (
	"context"
	"time"
)

// Entry is one break's memory. Summary is what she may build on and call back
// to; Phrases feeds the separate don't-repeat blocklist.
type Entry struct {
	Kind      string // live.ClipSeam
	Summary   string
	Phrases   []string
	CreatedAt time.Time // set by the store on Append
}

// defaultLimit bounds Recent when the caller passes limit <= 0. BOTH stores
// apply it, so the two backends cannot drift on the edge case — a shared
// contract whose implementations disagree at the boundary is not a contract.
const defaultLimit = 8

type Store interface {
	// Append records one break's memory. CreatedAt is assigned by the store.
	Append(ctx context.Context, e Entry) error
	// Recent returns entries created at or after since, OLDEST FIRST — the
	// order she reads them as a narrative. When more than limit qualify, the
	// NEWEST limit entries are returned (still oldest-first).
	//
	// limit <= 0 means defaultLimit, never "unbounded": these entries go
	// straight into a prompt with a token budget, so an accidental zero must
	// not become a full-table read.
	Recent(ctx context.Context, since time.Time, limit int) ([]Entry, error)
}
