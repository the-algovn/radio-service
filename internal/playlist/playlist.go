// Package playlist is the radio product's playlist + station-state store.
// It mirrors internal/library's layout: a Store interface, PGStore
// (prod/local) backed by Postgres via sqlc, and MemStore for hermetic tests.
// State guards (empty-playlist activation, delete/empty-while-on-air, stale
// reorder) live in the implementations behind sentinel errors so the pg impl
// can enforce them inside the mutation's transaction (SELECT … FOR UPDATE on
// the singleton station row); internal/radioserver maps them to gRPC codes.
// GoOnAir itself is an unconditional idempotent flip — the engine falls back
// to library shuffle when there's no active playlist (spec §4.2).
package playlist

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrEmptyPlaylist = errors.New("playlist is empty")
	ErrActiveOnAir   = errors.New("playlist is active while on air")
	ErrStale         = errors.New("stale track list")
)

type Summary struct {
	ID, Name       string
	TrackCount     int
	TotalDurationS int64
	IsActive       bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Item struct {
	Position  int
	YTID      string
	Title     string
	Channel   string
	DurationS int64
}

type Station struct {
	ActivePlaylistID   string
	ActivePlaylistName string
	ActiveTrackCount   int
	OnAir              bool
	OnAirSince         *time.Time
}

type Store interface {
	Create(ctx context.Context, name string) (Summary, error)
	List(ctx context.Context) ([]Summary, error)
	Get(ctx context.Context, id string) (Summary, []Item, error)
	Rename(ctx context.Context, id, name string) (Summary, error)
	Delete(ctx context.Context, id string) error
	AddTrack(ctx context.Context, playlistID, ytID string) (Summary, []Item, error)
	RemoveTrack(ctx context.Context, playlistID, ytID string) (Summary, []Item, error)
	Reorder(ctx context.Context, playlistID string, ytIDs []string) (Summary, []Item, error)
	GetStation(ctx context.Context) (Station, error)
	SetActive(ctx context.Context, playlistID string) (Station, error)
	GoOnAir(ctx context.Context) (Station, error)
	GoOffAir(ctx context.Context) (Station, error)
}
