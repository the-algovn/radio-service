// Package cadence is the station's format clock: given what has already
// aired and the operator's DJ settings, it answers which talk segment is due
// and what airing one does to that state.
//
// It exists because the rule has two consumers. internal/director evaluates
// it live against its own state; internal/timeline evaluates it repeatedly
// against synthetic state while projecting the console's forward running
// order. Those were independent hand-written copies pinned together by a
// director export named DueKindForTest, whose own comment called the
// arrangement a drift risk. This package is the single copy.
//
// Pure by construction: no I/O, no logging, and no time.Now() - State.Now is
// the only "now", so the projector can ask about an instant that has not
// arrived.
package cadence

import (
	"time"

	"github.com/the-algovn/radio-service/internal/station"
)

// The talk segment kinds. Mirrored by value in internal/live and pinned by
// live.TestClipKindConstantsMirrorShowlog.
const (
	KindStationID         = "station_id"
	KindSeam              = "seam"
	KindMusing            = "musing"
	KindDaypartTransition = "daypart_transition"
	KindWakeGreeting      = "wake_greeting"
)

// DaypartWindow bounds how late an edge-triggered daypart transition may still
// air. Every other kind carries indefinitely because its trigger is a timer
// that stays elapsed, but a rollover happened at one moment: "da chin gio toi
// roi do" said at ten in the evening is false, not late.
const DaypartWindow = 15 * time.Minute

type State struct {
	Now time.Time

	// SessionHasMusic gates every kind except the station ID. A seam prepared
	// before any music has aired this session is anchored to the PREVIOUS
	// broadcast's last track, so director.Take discards it against the zero
	// Entry - an LLM call and a TTS bill for a clip that can never air.
	SessionHasMusic bool

	FinishedSinceSeam int
	LastStationID     time.Time
	LastMusing        time.Time

	// PendingDaypart is the instant the local daypart rolled over; zero means
	// none is owed. Expiry needs no clearing code: past DaypartWindow the age
	// test stops returning it, and the next rollover overwrites it.
	PendingDaypart time.Time
	PendingWake    bool

	Forced              bool
	StationIDsAvailable bool
}

// DueKind returns the kind that is due, or "" when none is.
//
// Losers carry: a kind that loses a collision stays due and airs at a later
// seam, because the timer terms are still elapsed next time and PendingWake is
// a flag. The daypart transition is the one exception - see DaypartWindow.
func DueKind(s State, dj station.DJSettings) string {
	// A zero LastStationID means the director has not started its clock yet,
	// not "overdue since the epoch". RunOnce stamps it on the off-to-on
	// transition, but the projector can see the zero for up to one 20s tick.
	if dj.StationIDMin > 0 && s.StationIDsAvailable && !s.LastStationID.IsZero() &&
		s.Now.Sub(s.LastStationID) >= time.Duration(dj.StationIDMin)*time.Minute {
		return KindStationID
	}

	// Everything below either describes music that aired or greets a room that
	// has heard some.
	if !s.SessionHasMusic {
		return ""
	}

	if s.PendingWake {
		return KindWakeGreeting
	}
	if !s.PendingDaypart.IsZero() {
		if age := s.Now.Sub(s.PendingDaypart); age >= 0 && age <= DaypartWindow {
			return KindDaypartTransition
		}
	}
	if dj.MusingEveryMin > 0 && !s.LastMusing.IsZero() &&
		s.Now.Sub(s.LastMusing) >= time.Duration(dj.MusingEveryMin)*time.Minute {
		return KindMusing
	}
	if s.Forced || (dj.BreakEvery > 0 && s.FinishedSinceSeam+1 >= dj.BreakEvery) {
		return KindSeam
	}
	return ""
}

// Advance is what airing a talk segment does to the format clock. Every kind
// she authored resets the seam counter; a station ID does not, because it is a
// pre-written line rather than a break.
//
// Forced is cleared by KindSeam ALONE, not by every non-station-id kind.
// Forced only ever makes a seam due, so letting a musing or a greeting consume
// the arming would swallow an operator's forced break silently.
func Advance(s State, kind string, at time.Time) State {
	switch kind {
	case KindStationID:
		s.LastStationID = at
		return s
	case KindSeam:
		s.Forced = false
	case KindMusing:
		s.LastMusing = at
	case KindDaypartTransition:
		s.PendingDaypart = time.Time{}
	case KindWakeGreeting:
		s.PendingWake = false
	default:
		return s
	}
	s.FinishedSinceSeam = 0
	return s
}

// AdvanceMusic is what one aired music item does to the format clock.
func AdvanceMusic(s State) State {
	s.FinishedSinceSeam++
	s.SessionHasMusic = true
	return s
}
