// Package station is the singleton station-state store: the on-air flag
// (with its ear-sync anchor) and the AI pause switch. It replaces the
// playlist-era station store (removed in v1.2 — the engine programs
// itself; playlists are gone). Layout mirrors internal/request:
// Store interface, MemStore, PGStore, one contract suite for both.
package station

import (
	"context"
	"time"
)

// DJSettings are the DJ delivery knobs (spec 2026-07-23-dj-voice-settings).
// StationIDMin is MINUTES between station IDs; 0 disables. BreakEvery 0
// disables seam breaks. Persisted on the station row; the director re-reads
// them every tick.
type DJSettings struct {
	VoiceID      string
	Rate         float64
	BreakEvery   int
	StationIDMin int // minutes
	MaxChars     int
}

// DefaultDJSettings mirrors the 00010 migration column defaults (the
// current prod env values).
func DefaultDJSettings() DJSettings {
	return DJSettings{VoiceID: "vi-VN-Neural2-A", Rate: 1.0,
		BreakEvery: 1, StationIDMin: 60, MaxChars: 1024}
}

type Station struct {
	OnAir      bool
	OnAirSince *time.Time
	AIEnabled  bool
	DJ         DJSettings
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
	// UpdateDJSettings persists the full DJ knob set (no partial updates —
	// the API layer always sends every field) and returns the fresh Station.
	UpdateDJSettings(ctx context.Context, s DJSettings) (Station, error)
}
