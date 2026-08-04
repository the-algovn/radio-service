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

func medianOr(s State) int {
	if s.MedianTrackS > 0 {
		return s.MedianTrackS
	}
	return FallbackMedianS
}

// anchorFresh runs the SAME test live's Take runs: identity plus a one-second
// tolerance. A clip failing it is discarded at Take, so reporting it as
// `prepared` would promise a break that cannot air.
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

	if lastWasBreak || !sessionHasMusic {
		return Segment{}, false
	}

	// prepared: the director has a rendered clip ready to hand over at the
	// next seam. Only valid on the first iteration (the slot can only be
	// consumed once) and only when its anchor is still fresh.
	if first && s.Dir.HasClip && anchorFresh(s) {
		return Segment{
			SegmentID:      fmt.Sprintf("proj:prep:%d", idx),
			Kind:           s.Dir.ClipKind,
			Certainty:      CertaintyPrepared,
			DurationS:      int(math.Round(s.Dir.ClipDurationS)),
			BacksellTitle:  s.Dir.ClipBacksellTitle,
			PromiseTitle:   s.Dir.ClipPromiseTitle,
			CorrelationID:  s.Dir.ClipCorrelationID,
		}, true
	}

	// due breaks are suppressed by the gate (budget, no listeners, etc.).
	// Prepared clips above are not — they are already paid for.
	if gate != GateOK {
		return Segment{}, false
	}

	// station ID wins when both it and a seam are due.
	if s.Station.DJ.StationIDMin > 0 {
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

	// seam due: the +1 counts the currently-airing track, which the
	// engine's TrackFinished call will increment before the next boundary
	// (so finishedSinceSeam was N, will become N+1 before the boundary).
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
	// committed next-up — the feeder's arm 1. Consumes the pin exactly once.
	if s.NextUp != nil && !*pinConsumed {
		*pinConsumed = true
		dur := medianOr(s)
		if d, ok := s.Durations[s.NextUp.YTID]; ok {
			dur = d
		}
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
	}

	// ready queue head — the feeder's arm 2.
	if len(*ready) > 0 {
		it := (*ready)[0]
		*ready = (*ready)[1:]
		dur := medianOr(s)
		if d, ok := s.Durations[it.YTID]; ok {
			dur = d
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
