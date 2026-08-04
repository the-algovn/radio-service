// Package broadcast records when the station was on the air.
//
// station.on_air_since holds only the CURRENT session, so it can never place a
// historical divider on the console's timeline. These rows can.
//
// Sessions are written by live.Engine, NOT by GoOnAir/GoOffAir, for three
// independent reasons — see the design doc §"Session lifecycle". The shortest
// one: a boot resume never calls GoOnAir at all (engine.go:10-11, "The poll
// doubles as the boot-resume trigger after a restart with on_air=true"), and
// restarts are how this service deploys.
package broadcast

import (
	"context"
	"time"
)

// defaultLimit bounds Overlapping when the caller passes limit <= 0. BOTH
// stores apply it, so the two backends cannot drift on the edge case.
const defaultLimit = 20

// Session is one broadcast. EndedAt nil means still on air.
type Session struct {
	ID        int64
	StartedAt time.Time
	EndedAt   *time.Time
}

type Store interface {
	// Open records the start of a broadcast.
	Open(ctx context.Context, at time.Time) error
	// Close ends every open session at `at`. After CloseOrphans has run at
	// boot there is at most one, but closing all of them is the idempotent
	// reading and cannot leave a stale row behind.
	Close(ctx context.Context, at time.Time) error
	// CloseOrphans closes sessions left open by a process that died on air,
	// at the end of the last item that aired inside them. Returns how many
	// were closed. Call once at boot, BEFORE any session can be opened.
	CloseOrphans(ctx context.Context, at time.Time) (int64, error)
	// Overlapping returns sessions intersecting [from, to], newest first.
	// limit <= 0 means defaultLimit, never unbounded.
	Overlapping(ctx context.Context, from, to time.Time, limit int) ([]Session, error)
}
