package director

import "github.com/the-algovn/radio-service/internal/live"

// spec is what varies between segment kinds. It is three booleans rather than
// five code paths because everything else about preparing a clip - TTS,
// ledger, render, show memory - is identical across all of them.
type spec struct {
	Generated bool // the brain writes it; false = a pre-written station-ID line
	Anchored  bool // backsells a named track, so Take's staleness check applies
	Promises  bool // peeks and pins the next track
}

// specs is the whole per-kind vocabulary. The seam is alone in the right-hand
// columns and that is the point: it is the only kind that names a specific
// track it just played and a specific track coming next. The other three talk
// about the night, so they need no air log, no peek, and no pin - and they
// cannot go stale.
var specs = map[string]spec{
	live.ClipStationID:         {},
	live.ClipSeam:              {Generated: true, Anchored: true, Promises: true},
	live.ClipMusing:            {Generated: true},
	live.ClipDaypartTransition: {Generated: true},
	live.ClipWakeGreeting:      {Generated: true},
}

func specFor(kind string) (spec, bool) {
	s, ok := specs[kind]
	return s, ok
}
