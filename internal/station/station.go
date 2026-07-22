// Package station is the singleton station-state store: the on-air flag
// (with its ear-sync anchor) and the AI pause switch. It replaces the
// playlist-era station half of internal/playlist (v1.2 — the engine
// programs itself; playlists are gone). Layout mirrors internal/request:
// Store interface, MemStore, PGStore, one contract suite for both.
package station

import (
	"context"
	"time"
)

type Station struct {
	OnAir      bool
	OnAirSince *time.Time
	AIEnabled  bool
}

type Store interface {
	GetStation(ctx context.Context) (Station, error)
	// GoOnAir is an unconditional idempotent flip (v1 semantics): the
	// OnAirSince anchor is set only on the off→on transition and preserved
	// on repeat calls. The library-non-empty guard lives in radioserver.
	GoOnAir(ctx context.Context) (Station, error)
	GoOffAir(ctx context.Context) (Station, error)
	// SetAIEnabled flips the programmer's pause switch (persisted).
	SetAIEnabled(ctx context.Context, enabled bool) (Station, error)
}
