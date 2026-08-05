package timeline

import (
	"fmt"
	"math"
	"time"

	"github.com/the-algovn/radio-service/internal/request"
)

// Duration estimates for breaks that have not been scripted yet. Only ever
// applied to `due` segments — a `prepared` clip carries its exact length.
const (
	EstSeamS      = 40
	EstStationIDS = 12
)

// Project returns the forward running order, the off-axis staging strip, and
// the break gate. Pure: no I/O, no clock read — s.Now is the only "now".
func Project(s State) (upcoming, staging []Segment, gate string) {
	gate = Gate(s)
	staging = buildStaging(s) // step 2

	if !s.Station.OnAir {
		return nil, staging, gate // step 3: no running order for a dead station
	}

	// step 4 — mutable walk locals. EVERY cadence test below compares against
	// `clock`, never s.Now: a station ID that comes due partway through the
	// airing track must fire at the seam where it would actually fire.
	clock := s.Now.Truncate(time.Second)
	if s.Airing != nil {
		clock = s.Airing.StartedAt.Add(time.Duration(s.Airing.DurationS) * time.Second).Truncate(time.Second)
	}
	start := clock
	finishedSinceSeam := s.Dir.FinishedSinceSeam
	lastStationID := s.Dir.LastStationID
	ready := readyOnly(s.Pending)
	pinConsumed := false
	lastWasBreak := s.Airing != nil && isBreakKind(s.Airing.Kind)
	sessionHasMusic := s.Airing != nil && s.Airing.Kind == KindTrack
	first := true

	for len(upcoming) < MaxSegments && clock.Sub(start) < HorizonS*time.Second {
		if seg, ok := seamArm(s, gate, clock, first, lastWasBreak, sessionHasMusic,
			finishedSinceSeam, lastStationID, len(upcoming)); ok {
			upcoming = append(upcoming, seg)
			clock = clock.Add(time.Duration(seg.DurationS) * time.Second)
			lastWasBreak = true
			if seg.Kind == KindStationID {
				lastStationID = clock
			} else {
				finishedSinceSeam = 0
			}
			first = false
			continue // a break is never followed immediately by another
		}
		first = false

		seg := musicArm(s, clock, &pinConsumed, &ready, len(upcoming)) // step 5b
		upcoming = append(upcoming, seg)
		clock = clock.Add(time.Duration(seg.DurationS) * time.Second)
		finishedSinceSeam++
		lastWasBreak = false
		sessionHasMusic = true
	}
	return upcoming, staging, gate
}

func isBreakKind(k string) bool { return k == KindDJ || k == KindStationID }

func readyOnly(items []request.Item) []request.Item {
	out := make([]request.Item, 0, len(items))
	for _, it := range items {
		if it.Status == request.StatusReady {
			out = append(out, it)
		}
	}
	return out
}

// removeRequest drops a request from the walk-local ready queue by ID.
func removeRequest(ready *[]request.Item, id string) {
	for i, it := range *ready {
		if it.ID == id {
			*ready = append((*ready)[:i:i], (*ready)[i+1:]...)
			return
		}
	}
}

func medianOr(s State) int {
	if s.MedianTrackS > 0 {
		return s.MedianTrackS
	}
	return FallbackMedianS
}

// pinRequestReady checks whether a pinned request is still ready to air.
// Returns the matching item when found and its status is ready; absent or
// non-ready rows are unusable — the engine consumes and skips the pin.
func pinRequestReady(pending []request.Item, requestID string) (request.Item, bool) {
	for _, it := range pending {
		if it.ID == requestID {
			return it, it.Status == request.StatusReady
		}
	}
	return request.Item{}, false
}

// anchorFresh runs the SAME test live's Take runs: identity plus a one-second
// tolerance. A clip failing it is discarded at Take, so reporting it as
// `prepared` would promise a break that cannot air.
//
// It compares against s.Airing (which may be a talk break), not the last
// *music* entry like the engine's Take(justFinished). That is only sound
// because seamArm returns early when the airing item is a break — anchorFresh
// is never evaluated in that state. Removing that guard without carrying a
// lastMusicYTID/lastMusicStartedAt through the walk would make this silently
// wrong.
func anchorFresh(s State) bool {
	if s.Airing == nil || s.Dir.ClipAnchorYTID != s.Airing.YTID {
		return false
	}
	d := s.Dir.ClipAnchorStartedAt.Sub(s.Airing.StartedAt)
	return math.Abs(float64(d)) <= float64(AnchorTolerance)
}

// buildStaging collects non-ready pending rows into an off-axis strip. These
// are still downloading (StatusApproved) or otherwise ineligible to air.
// Capped at 20; carries no StartedAt.
func buildStaging(s State) []Segment {
	const cap = 20
	out := make([]Segment, 0)
	for _, it := range s.Pending {
		if it.Status == request.StatusReady {
			continue
		}
		out = append(out, Segment{
			SegmentID:       "req:" + it.ID,
			Certainty:       CertaintyStaging,
			Title:           it.Title,
			YTID:            it.YTID,
			Artist:          it.Channel,
			DurationS:       int(it.DurationS),
			ThumbnailURL:    it.ThumbnailURL,
			Source:          it.Source,
			RequestedByName: it.DisplayName,
			Reason:          it.Reason,
			RequestID:       it.ID,
			Status:          it.Status,
		})
		if len(out) >= cap {
			break
		}
	}
	return out
}

// seamArm decides whether the next item is a talk break. Priority:
// prepared clip (exact, already rendered), then station ID, then seam due.
// `due` breaks are suppressed when the gate is not OK; `prepared` clips are
// NOT — a clip already in the director's slot is paid for and Take will
// still air it regardless of the current gate.
func seamArm(s State, gate string, clock time.Time, first, lastWasBreak, sessionHasMusic bool,
	finishedSinceSeam int, lastStationID time.Time, idx int) (Segment, bool) {

	if lastWasBreak {
		return Segment{}, false
	}

	// Prepared clip: station_id is always fresh (no anchor needed);
	// seam clips must pass the anchor-freshness test Take applies.
	if first && s.Dir.HasClip {
		// ClipKind is the ENGINE kind (live.ClipSeam / live.ClipStationID),
		// which is not the wire vocabulary — translate before it ships.
		kind := KindFromEngine(s.Dir.ClipKind)
		if kind == KindStationID || anchorFresh(s) {
			return Segment{
				SegmentID:     fmt.Sprintf("proj:prep:%d", idx),
				Kind:          kind,
				Certainty:     CertaintyPrepared,
				DurationS:     int(math.Round(s.Dir.ClipDurationS)),
				StartedAt:     clock,
				BacksellTitle: s.Dir.ClipBacksellTitle,
				PromiseTitle:  s.Dir.ClipPromiseTitle,
				CorrelationID: s.Dir.ClipCorrelationID,
			}, true
		}
	}

	// due breaks are suppressed by the gate (budget, no listeners, etc.).
	// Prepared clips above are not — they are already paid for.
	if gate != GateOK {
		return Segment{}, false
	}

	// Station ID wins when both it and a seam are due, and is evaluated
	// even when no music has aired yet — the engine's Take only
	// anchor-checks seam clips, not station IDs.
	// A zero lastStationID is "the director has not reset its clock yet", not
	// "overdue since the epoch". dr.lastStationID is reset on the off→on
	// transition inside RunOnce, driven by a 20s ticker, and GoOnAir pokes the
	// feeder rather than the director — so for up to 20s after go-on-air the
	// projector sees the zero value while the engine never does. Sub saturates
	// rather than overflowing, making the test unconditionally true.
	if s.Station.DJ.StationIDMin > 0 && s.Dir.StationIDsAvailable && !lastStationID.IsZero() {
		if clock.Sub(lastStationID) >= time.Duration(s.Station.DJ.StationIDMin)*time.Minute {
			return Segment{
				SegmentID: fmt.Sprintf("proj:due:%d", idx),
				Kind:      KindStationID,
				Certainty: CertaintyDue,
				DurationS: EstStationIDS,
				StartedAt: clock,
			}, true
		}
	}

	// Seam due — only when sessionHasMusic: a seam clip at session start
	// always fails the anchor check against the zero Entry taken by Take.
	if !sessionHasMusic {
		return Segment{}, false
	}
	if s.Station.DJ.BreakEvery > 0 && finishedSinceSeam+1 >= s.Station.DJ.BreakEvery {
		return Segment{
			SegmentID: fmt.Sprintf("proj:due:%d", idx),
			Kind:      KindDJ,
			Certainty: CertaintyDue,
			DurationS: EstSeamS,
			StartedAt: clock,
		}, true
	}

	return Segment{}, false
}

// musicArm picks the next music track. Priority: committed next-up (the
// feeder's pin), then the ready queue, then an anonymous unknown block
// sized from the median track length.
func musicArm(s State, clock time.Time, pinConsumed *bool, ready *[]request.Item, idx int) Segment {
	// Committed next-up — the feeder's arm 1. The engine ALWAYS consumes
	// the pin (consumedNextUp is set regardless of outcome). The pin is
	// usable only when BOTH hold: (a) the track still exists in the library
	// (present in Durations — absent means NOT AIRABLE), and (b) the
	// request-id check passes (empty, or a Pending row at StatusReady).
	// Otherwise the engine skips the pin and re-plans.
	if s.NextUp != nil && !*pinConsumed {
		*pinConsumed = true

		// Track must still exist. Durations membership IS the library-existence
		// signal — the projector is pure and cannot call the library directly.
		dur, exists := s.Durations[s.NextUp.YTID]
		if !exists {
			// Unusable pin (missing library track): fall through to the ready
			// queue in the same iteration.
		} else if s.NextUp.RequestID == "" {
			// Shuffle pin (no RequestID) with an existing track.
			return Segment{
				SegmentID: "pin:" + s.NextUp.YTID,
				Kind:      KindTrack,
				Certainty: CertaintyCommitted,
				Title:     s.NextUp.Title,
				YTID:      s.NextUp.YTID,
				Artist:    s.NextUp.Channel,
				StartedAt: clock,
				DurationS: dur,
			}
		} else if it, ok := pinRequestReady(s.Pending, s.NextUp.RequestID); ok {
			// Request pin with a still-ready request and an existing track.
			// The pin's row is by definition also in `ready` (both are the
			// StatusReady test), so drop it: the engine's MarkAired removes it
			// before NextReady runs, and leaving it would project it twice.
			removeRequest(ready, it.ID)
			return Segment{
				SegmentID:       "pin:" + s.NextUp.YTID,
				Kind:            KindTrack,
				Certainty:       CertaintyCommitted,
				Title:           s.NextUp.Title,
				YTID:            s.NextUp.YTID,
				Artist:          s.NextUp.Channel,
				StartedAt:       clock,
				DurationS:       dur,
				Source:          it.Source,
				RequestedByName: it.DisplayName,
				Reason:          it.Reason,
				RequestID:       it.ID,
			}
		}
		// Unusable pin (missing track, or request not ready): fall through to
		// the ready queue in the same iteration.
	}

	// ready queue head — the feeder's arm 2. Pop heads until one with a
	// library-resolved track. planNext arm 2 returns skip when a ready
	// request's track is missing, so the projector must skip them too.
	for len(*ready) > 0 {
		it := (*ready)[0]
		*ready = (*ready)[1:]
		dur, ok := s.Durations[it.YTID]
		if !ok {
			continue // unresolvable; skip
		}
		return Segment{
			SegmentID:       "req:" + it.ID,
			Kind:            KindTrack,
			Certainty:       CertaintyProjected,
			Title:           it.Title,
			YTID:            it.YTID,
			Artist:          it.Channel,
			ThumbnailURL:    it.ThumbnailURL,
			StartedAt:       clock,
			DurationS:       dur,
			Source:          it.Source,
			RequestedByName: it.DisplayName,
			Reason:          it.Reason,
			RequestID:       it.ID,
			Status:          it.Status,
		}
	}

	// unknown shuffle — the feeder's arm 3. A fresh random roll at the
	// boundary is unknowable; render geometry, never a title.
	return Segment{
		SegmentID: fmt.Sprintf("proj:unknown:%d", idx),
		Kind:      KindUnknown,
		Certainty: CertaintyUnknown,
		StartedAt: clock,
		DurationS: medianOr(s),
	}
}
