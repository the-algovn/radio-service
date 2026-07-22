// Package request is the station's play queue and request record in one:
// a row is a listener request or an AI pick, moving approved → ready →
// aired | failed (spec §3, statuses from products/radio/architecture.md).
// It mirrors internal/playlist's layout: a Store interface, PGStore
// (prod/local) via sqlc, MemStore for hermetic tests, and a contract suite
// run against both.
package request

import (
	"context"
	"errors"
	"time"
)

var ErrNotFound = errors.New("not found")

const (
	SourceListener = "listener"
	SourceAI       = "ai"

	StatusApproved = "approved" // accepted, awaiting ingest
	StatusReady    = "ready"    // artifact cached, eligible to air
	StatusAired    = "aired"
	StatusFailed   = "failed"
)

type Item struct {
	ID           string
	Source       string // SourceListener | SourceAI
	RequestedBy  string // JWT sub; "" for ai
	DisplayName  string
	YTID         string
	Title        string
	Channel      string
	DurationS    int64
	ThumbnailURL string
	Status       string
	FailReason   string
	Attempts     int
	CreatedAt    time.Time
	AiredAt      *time.Time
}

// Store is the queue + request record. Air/display order everywhere is:
// listener requests FIFO first, then AI picks FIFO.
type Store interface {
	// Create persists it (Source, RequestedBy, DisplayName, YTID, Title,
	// Channel, DurationS, ThumbnailURL, Status are read from it) and returns
	// the stored item with ID and CreatedAt filled.
	Create(ctx context.Context, it Item) (Item, error)
	// NextReady returns what should air next: the oldest ready listener
	// request, else the oldest ready AI pick. found=false when none.
	NextReady(ctx context.Context) (Item, bool, error)
	// OldestApproved returns the oldest approved item (any source) for the
	// ingest worker. found=false when none.
	OldestApproved(ctx context.Context) (Item, bool, error)
	// Pending returns approved+ready items in air order.
	Pending(ctx context.Context) ([]Item, error)
	// ByUser returns the caller's requests, newest first, capped at limit.
	ByUser(ctx context.Context, sub string, limit int) ([]Item, error)
	CountPendingByUser(ctx context.Context, sub string) (int, error)
	// CountSince counts the user's requests created at or after since.
	CountSince(ctx context.Context, sub string, since time.Time) (int, error)
	// HasPendingYTID reports whether ytID is already approved or ready.
	HasPendingYTID(ctx context.Context, ytID string) (bool, error)
	// MarkReady flips approved → ready; ErrNotFound when the id is unknown
	// or not currently approved.
	MarkReady(ctx context.Context, id string) error
	MarkAired(ctx context.Context, id string, at time.Time) error
	MarkFailed(ctx context.Context, id, reason string) error
	// BumpAttempts increments attempts, records reason as the latest
	// fail_reason, and returns the new count. Status is unchanged.
	BumpAttempts(ctx context.Context, id, reason string) (int, error)
}

// DayStart returns the start of the station-local civil day containing now
// (station days — like every station clock — are Asia/Ho_Chi_Minh).
func DayStart(now time.Time, loc *time.Location) time.Time {
	n := now.In(loc)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
}
