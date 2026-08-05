package timeline_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/schedule"
	"github.com/the-algovn/radio-service/internal/station"
	"github.com/the-algovn/radio-service/internal/timeline"
)

var base = time.Date(2026, 8, 4, 21, 0, 0, 0, time.UTC)

func airing(kind string, dur int) *timeline.Segment {
	return &timeline.Segment{
		SegmentID: "air:1", Kind: kind, Certainty: timeline.CertaintyAiring,
		Title: "Ánh Nắng Của Anh", YTID: "", StartedAt: base, DurationS: dur,
	}
}

func liveState() timeline.State {
	return timeline.State{
		Now:       base.Add(90 * time.Second),
		Station:   station.Station{OnAir: true, AIEnabled: true, DJ: station.DJSettings{BreakEvery: 2, StationIDMin: 20}},
		Airing:    airing(timeline.KindTrack, 240),
		Dir:       timeline.DirectorSnapshot{Present: true, LastStationID: base, StationIDsAvailable: true},
		Listeners: 2, BudgetUSD: 5, MedianTrackS: 200,
	}
}

func TestOffAirProjectsNothing(t *testing.T) {
	s := liveState()
	s.Station.OnAir = false
	up, _, gate := timeline.Project(s)
	require.Empty(t, up)
	require.Equal(t, timeline.GateOffAir, gate)
}

func TestCommittedPinIsCertain(t *testing.T) {
	s := liveState()
	s.NextUp = &schedule.NextUp{YTID: "y2", Title: "Chạy Ngay Đi", Channel: "Sơn Tùng M-TP"}
	s.Durations = map[string]int{"y2": 268}
	up, _, _ := timeline.Project(s)
	first := firstOfKind(t, up, timeline.KindTrack)
	require.Equal(t, timeline.CertaintyCommitted, first.Certainty)
	require.Equal(t, "Chạy Ngay Đi", first.Title)
}

func TestPinOutranksTheReadyQueue(t *testing.T) {
	s := liveState()
	s.NextUp = &schedule.NextUp{YTID: "pin", Title: "Pinned"}
	s.Pending = []request.Item{{ID: "r1", YTID: "y9", Title: "Queued", Status: request.StatusReady}}
	s.Durations = map[string]int{"pin": 200, "y9": 200}
	up, _, _ := timeline.Project(s)
	require.Equal(t, "Pinned", firstOfKind(t, up, timeline.KindTrack).Title)
}

func TestApprovedIsNotAirable(t *testing.T) {
	s := liveState()
	s.Pending = []request.Item{
		{ID: "r1", YTID: "y1", Title: "Downloading", Status: request.StatusApproved},
		{ID: "r2", YTID: "y2", Title: "Ready", Status: request.StatusReady},
	}
	s.Durations = map[string]int{"y1": 200, "y2": 200}
	up, staging, _ := timeline.Project(s)
	require.Equal(t, "Ready", firstOfKind(t, up, timeline.KindTrack).Title)
	require.Len(t, staging, 1)
	require.Equal(t, "Downloading", staging[0].Title)
	require.Equal(t, timeline.CertaintyStaging, staging[0].Certainty)
}

func TestEmptyQueueFallsToUnknown(t *testing.T) {
	s := liveState()
	up, _, _ := timeline.Project(s)
	first := firstOfKind(t, up, timeline.KindTrack, timeline.KindUnknown)
	require.Equal(t, timeline.KindUnknown, first.Kind)
	require.Equal(t, timeline.CertaintyUnknown, first.Certainty)
	require.Empty(t, first.Title, "an unknown block must never carry a title")
	require.Equal(t, 200, first.DurationS, "sized from MedianTrackS")
}

func TestUnknownUsesFallbackMedianWhenUnset(t *testing.T) {
	s := liveState()
	s.MedianTrackS = 0
	up, _, _ := timeline.Project(s)
	require.Equal(t, timeline.FallbackMedianS, firstOfKind(t, up, timeline.KindUnknown).DurationS)
}

func TestPreparedClipIsEmittedWithExactDuration(t *testing.T) {
	s := liveState()
	s.Dir.HasClip = true
	s.Dir.ClipKind = live.ClipSeam // the ENGINE kind, which is what production supplies
	s.Dir.ClipDurationS = 38
	s.Dir.ClipAnchorYTID = "y1"
	s.Dir.ClipAnchorStartedAt = base
	s.Airing.YTID = "y1"
	up, _, _ := timeline.Project(s)
	require.Equal(t, timeline.CertaintyPrepared, up[0].Certainty)
	require.Equal(t, 38, up[0].DurationS)
	require.Equal(t, timeline.KindDJ, up[0].Kind,
		"the engine kind %q must be translated to the wire vocabulary", live.ClipSeam)
}

func TestOnlyOnePreparedClipAcrossTheHorizon(t *testing.T) {
	// The director holds exactly ONE slotted clip. Without the `first` guard
	// in seamArm it is emitted at every seam across the horizon, each copy
	// carrying the same CorrelationID — one paid-for break rendered eight
	// times at `prepared`, which is the fabrication the certainty ladder
	// exists to prevent. This is the same composition hazard the walk
	// documents for anchorFresh: the arm is only sound in the right state.
	s := liveState()
	s.Dir.HasClip = true
	s.Dir.ClipKind = live.ClipSeam
	s.Dir.ClipDurationS = 38
	s.Dir.ClipAnchorYTID = "y1"
	s.Dir.ClipAnchorStartedAt = base
	s.Dir.ClipCorrelationID = "corr-1"
	s.Airing.YTID = "y1"
	up, _, _ := timeline.Project(s)

	require.Greater(t, len(up), 3, "the walk must reach several seams or this asserts nothing")
	prepared := 0
	for _, seg := range up {
		if seg.Certainty == timeline.CertaintyPrepared {
			prepared++
		}
	}
	require.Equal(t, 1, prepared, "the director's single clip must be projected once")
}

func TestStaleAnchorEmitsNoPrepared(t *testing.T) {
	s := liveState()
	s.Dir.HasClip = true
	s.Dir.ClipKind = live.ClipSeam // the ENGINE kind, which is what production supplies
	s.Dir.ClipDurationS = 38
	s.Dir.ClipAnchorYTID = "y1"
	s.Dir.ClipAnchorStartedAt = base.Add(5 * time.Second) // > AnchorTolerance
	s.Airing.YTID = "y1"
	up, _, _ := timeline.Project(s)
	require.NotEqual(t, timeline.CertaintyPrepared, up[0].Certainty)
}

func TestAiringBreakBlocksEvenADueStationID(t *testing.T) {
	// The airing item is a talk break, so lastWasBreak is true. That guard
	// must block EVERY break — even a station ID that is genuinely due.
	// sessionHasMusic is false (DJ is not music) and the station ID is
	// wildly overdue, so the only thing standing between this state and
	// a due station ID is the `lastWasBreak` early-return in seamArm.
	s := liveState()
	s.Airing = airing(timeline.KindDJ, 38)
	s.Station.DJ.StationIDMin = 1
	s.Dir.LastStationID = base.Add(-99 * time.Hour)
	up, _, _ := timeline.Project(s)
	require.NotEqual(t, timeline.KindDJ, up[0].Kind)
	require.NotEqual(t, timeline.KindStationID, up[0].Kind)
	// Confirm it actually emitted music, not just "not a break".
	firstOfKind(t, up, timeline.KindTrack, timeline.KindUnknown)
}

func TestSeamDueFromCadence(t *testing.T) {
	s := liveState()
	s.Dir.FinishedSinceSeam = 1 // +1 for the airing track == BreakEvery 2
	up, _, _ := timeline.Project(s)
	require.Equal(t, timeline.KindDJ, up[0].Kind)
	require.Equal(t, timeline.CertaintyDue, up[0].Certainty)
}

func TestStationIDWinsWhenBothDue(t *testing.T) {
	s := liveState()
	s.Dir.FinishedSinceSeam = 5
	s.Dir.LastStationID = base.Add(-60 * time.Minute)
	up, _, _ := timeline.Project(s)
	require.Equal(t, timeline.KindStationID, up[0].Kind)
}

func TestNoStationIDWhenTheDirectorHasNoLines(t *testing.T) {
	// ids.available() is a term of the engine's dueKindLocked. With no usable
	// station-ID lines the engine falls through to BreakEvery and airs a seam,
	// so promising a station_id here would be the wrong kind, the wrong
	// duration, and would shift every following StartedAt forever.
	s := liveState()
	s.Dir.FinishedSinceSeam = 5
	s.Dir.LastStationID = base.Add(-60 * time.Minute) // wildly overdue
	s.Dir.StationIDsAvailable = false
	up, _, _ := timeline.Project(s)
	for _, seg := range up {
		require.NotEqual(t, timeline.KindStationID, seg.Kind,
			"the director cannot produce a station ID it has no lines for")
	}
}

func TestZeroLastStationIDIsNotOverdue(t *testing.T) {
	// For up to 20s after go-on-air the director has not yet reset
	// lastStationID (the reset rides its own ticker; GoOnAir pokes the feeder).
	// time.Time.Sub saturates, so an unguarded comparison announces a station
	// ID the engine has explicitly deferred by StationIDMin — which then
	// vanishes from the console on the next poll.
	s := liveState()
	s.Dir.LastStationID = time.Time{}
	up, _, _ := timeline.Project(s)
	require.NotEmpty(t, up)
	require.NotEqual(t, timeline.KindStationID, up[0].Kind,
		"a station ID is not due before the director's clock has started")
}

func TestStationIDIsTestedAgainstTheWalkClockNotNow(t *testing.T) {
	s := liveState()
	s.Dir.FinishedSinceSeam = 0
	s.Station.DJ.StationIDMin = 3
	s.Dir.LastStationID = base.Add(-1 * time.Minute) // not due at Now, due at the seam
	up, _, _ := timeline.Project(s)
	require.Equal(t, timeline.KindStationID, up[0].Kind)
}

func TestGateSuppressesDueButNotPrepared(t *testing.T) {
	s := liveState()
	s.Listeners = 0
	s.Dir.FinishedSinceSeam = 1
	up, _, gate := timeline.Project(s)
	require.Equal(t, timeline.GateNoListeners, gate)
	for _, seg := range up {
		require.NotEqual(t, timeline.CertaintyDue, seg.Certainty, "due breaks are suppressed when gated")
	}

	s.Dir.HasClip = true
	s.Dir.ClipKind = live.ClipSeam // the ENGINE kind, which is what production supplies
	s.Dir.ClipDurationS = 30
	s.Dir.ClipAnchorYTID = "y1"
	s.Dir.ClipAnchorStartedAt = base
	s.Airing.YTID = "y1"
	up, _, _ = timeline.Project(s)
	require.Equal(t, timeline.CertaintyPrepared, up[0].Certainty,
		"a prepared clip is paid for and will air regardless of the gate")
}

func TestNoTwoBreaksInARow(t *testing.T) {
	s := liveState()
	s.Dir.FinishedSinceSeam = 99
	s.Station.DJ.StationIDMin = 1
	s.Dir.LastStationID = base.Add(-99 * time.Hour)
	up, _, _ := timeline.Project(s)
	for i := 1; i < len(up); i++ {
		if isBreak(up[i-1]) {
			require.False(t, isBreak(up[i]), "awaitMusic forbids back-to-back breaks at %d", i)
		}
	}
}

func TestTimesAccumulateAndAreWholeSeconds(t *testing.T) {
	s := liveState()
	up, _, _ := timeline.Project(s)
	prevEnd := base.Add(240 * time.Second) // airing start + duration
	for _, seg := range up {
		require.True(t, seg.StartedAt.Equal(prevEnd), "each segment starts where the previous ended")
		require.Zero(t, seg.StartedAt.Nanosecond(), "projected times are quantised to whole seconds")
		prevEnd = seg.StartedAt.Add(time.Duration(seg.DurationS) * time.Second)
	}
}

func TestPreparedClipIsOnTheTimeAxis(t *testing.T) {
	// A prepared clip is the item whose air time is best known, so it must
	// carry StartedAt like every other emission. An empty StartedAt is the
	// wire signal for the off-axis staging strip.
	s := liveState()
	s.Dir.HasClip = true
	s.Dir.ClipKind = live.ClipSeam // the ENGINE kind, which is what production supplies
	s.Dir.ClipDurationS = 38
	s.Dir.ClipAnchorYTID = "y1"
	s.Dir.ClipAnchorStartedAt = base
	s.Airing.YTID = "y1"
	up, _, _ := timeline.Project(s)

	require.Equal(t, timeline.CertaintyPrepared, up[0].Certainty)
	prevEnd := base.Add(240 * time.Second) // airing start + duration
	for _, seg := range up {
		require.False(t, seg.StartedAt.IsZero(), "every projected segment sits on the time axis")
		require.True(t, seg.StartedAt.Equal(prevEnd), "each segment starts where the previous ended")
		prevEnd = seg.StartedAt.Add(time.Duration(seg.DurationS) * time.Second)
	}
}

func TestHorizonAndSegmentCap(t *testing.T) {
	s := liveState()
	up, _, _ := timeline.Project(s)
	require.LessOrEqual(t, len(up), timeline.MaxSegments)
	last := up[len(up)-1]
	require.True(t, last.StartedAt.Before(base.Add(240*time.Second+timeline.HorizonS*time.Second)))

	// The cap is not redundant with the horizon. medianOr can return as little
	// as 1s and BreakEvery has no lower bound, so on a short-track station the
	// cap is the only thing that stops the walk — the case the assertion above
	// never reaches, because liveState's ~15 segments are horizon-bound.
	s.MedianTrackS = 1
	up, _, _ = timeline.Project(s)
	require.Len(t, up, timeline.MaxSegments, "the segment cap stops a short-track walk")
	last = up[len(up)-1]
	require.Less(t, last.StartedAt.Sub(base.Add(240*time.Second)), timeline.HorizonS*time.Second/2,
		"the horizon must be nowhere near binding, or the cap is not what was tested")
}

func TestNothingAiringStartsAtNow(t *testing.T) {
	s := liveState()
	s.Airing = nil
	up, _, _ := timeline.Project(s)
	require.True(t, up[0].StartedAt.Equal(s.Now.Truncate(time.Second)))
}

func TestSegmentIDsAreUniqueAndStable(t *testing.T) {
	s := liveState()
	s.Pending = []request.Item{{ID: "r1", YTID: "y1", Title: "A", Status: request.StatusReady}}
	s.Durations = map[string]int{"y1": 200}
	a, _, _ := timeline.Project(s)
	b, _, _ := timeline.Project(s)
	seen := map[string]bool{}
	for i, seg := range a {
		require.NotEmpty(t, seg.SegmentID)
		require.False(t, seen[seg.SegmentID], "duplicate segment id %q", seg.SegmentID)
		seen[seg.SegmentID] = true
		require.Equal(t, seg.SegmentID, b[i].SegmentID, "ids must be stable across identical calls")
	}
}

// Fix 1: unusable pinned request //

func TestUnusablePinWithMissingRequestFallsToReadyQueue(t *testing.T) {
	s := liveState()
	s.NextUp = &schedule.NextUp{YTID: "pin", Title: "Phantom", RequestID: "ghost"}
	s.Pending = []request.Item{{ID: "r1", YTID: "y1", Title: "Queued", Status: request.StatusReady}}
	s.Durations = map[string]int{"pin": 200, "y1": 200}
	up, _, _ := timeline.Project(s)
	first := firstOfKind(t, up, timeline.KindTrack)
	require.Equal(t, "Queued", first.Title)
	require.Equal(t, timeline.CertaintyProjected, first.Certainty)
}

func TestUnusablePinWithApprovedRequestFallsToReadyQueue(t *testing.T) {
	s := liveState()
	s.NextUp = &schedule.NextUp{YTID: "pin", Title: "Pending", RequestID: "r9"}
	s.Pending = []request.Item{
		{ID: "r9", YTID: "pin", Title: "Pending", Status: request.StatusApproved},
		{ID: "r1", YTID: "y1", Title: "Ready", Status: request.StatusReady},
	}
	s.Durations = map[string]int{"pin": 200, "y1": 200}
	up, _, _ := timeline.Project(s)
	require.Equal(t, "Ready", firstOfKind(t, up, timeline.KindTrack).Title)
}

func TestUsablePinWithReadyRequestCarriesProvenance(t *testing.T) {
	s := liveState()
	s.NextUp = &schedule.NextUp{YTID: "pin", Title: "Promised", RequestID: "r7"}
	s.Pending = []request.Item{
		{ID: "r7", YTID: "pin", Title: "Promised",
			Source: request.SourceAI, DisplayName: "DJ", Reason: "it slaps",
			Status: request.StatusReady},
	}
	s.Durations = map[string]int{"pin": 200}
	up, _, _ := timeline.Project(s)
	first := firstOfKind(t, up, timeline.KindTrack)
	require.Equal(t, timeline.CertaintyCommitted, first.Certainty)
	require.Equal(t, "Promised", first.Title)
	require.Equal(t, request.SourceAI, first.Source)
	require.Equal(t, "DJ", first.RequestedByName)
	require.Equal(t, "it slaps", first.Reason)
	require.Equal(t, "r7", first.RequestID)

	// The pin's request is also a ready-queue row. The engine airs it once —
	// MarkAired removes it before NextReady runs — so the walk must not
	// project it again a few slots later.
	seen := 0
	for _, seg := range up {
		if seg.RequestID == "r7" {
			seen++
		}
	}
	require.Equal(t, 1, seen, "a pinned ready request must be projected exactly once")
}

// Fix 3: ready queue skips unresolvable heads //

func TestReadyQueueSkipsUnresolvableHead(t *testing.T) {
	// Two ready requests; the head's ytID is absent from Durations (missing
	// library track). The projector must pop past it and project the second
	// one — just as planNext arm 2 returns skip for unresolvable tracks.
	s := liveState()
	s.Pending = []request.Item{
		{ID: "r1", YTID: "gone", Title: "Gone", Status: request.StatusReady},
		{ID: "r2", YTID: "y2", Title: "Survivor", Status: request.StatusReady},
	}
	s.Durations = map[string]int{"y2": 250} // only the survivor resolved
	up, _, _ := timeline.Project(s)
	first := firstOfKind(t, up, timeline.KindTrack)
	require.Equal(t, "Survivor", first.Title)
	require.Equal(t, timeline.CertaintyProjected, first.Certainty)
}

func TestUnusablePinWithEmptyQueueFallsToUnknown(t *testing.T) {
	// Pin whose track is missing from the library (absent from Durations),
	// and no ready queue behind it. The pin is consumed but unusable; the
	// projector must fall to KindUnknown rather than naming a phantom.
	s := liveState()
	s.NextUp = &schedule.NextUp{YTID: "gone", Title: "Phantom"}
	// No Durations entry for "gone" → pin is unusable.
	// No Pending → ready queue is empty.
	up, _, _ := timeline.Project(s)
	first := firstOfKind(t, up, timeline.KindTrack, timeline.KindUnknown)
	require.Equal(t, timeline.KindUnknown, first.Kind)
	require.Empty(t, first.Title)
	require.Equal(t, timeline.CertaintyUnknown, first.Certainty)
}

// Fix 2: station ID may open a session //

func TestStationIDOpensSessionWhenNoMusicHasAired(t *testing.T) {
	s := liveState()
	s.Airing = nil
	s.Station.DJ.StationIDMin = 3
	s.Dir.LastStationID = base.Add(-5 * time.Minute)
	up, _, _ := timeline.Project(s)
	require.Equal(t, timeline.KindStationID, up[0].Kind)
}

func TestSeamDueIsSkippedWhenNoMusicHasAired(t *testing.T) {
	s := liveState()
	s.Airing = nil
	s.Dir.FinishedSinceSeam = 1
	s.Station.DJ.StationIDMin = 20
	s.Dir.LastStationID = base
	up, _, _ := timeline.Project(s)
	require.NotEqual(t, timeline.KindDJ, up[0].Kind)
	require.NotEqual(t, timeline.KindStationID, up[0].Kind)
}

// helpers

func isBreak(s timeline.Segment) bool {
	return s.Kind == timeline.KindDJ || s.Kind == timeline.KindStationID
}

func firstOfKind(t *testing.T, segs []timeline.Segment, kinds ...string) timeline.Segment {
	t.Helper()
	for _, s := range segs {
		for _, k := range kinds {
			if s.Kind == k {
				return s
			}
		}
	}
	t.Fatalf("no segment of kinds %v in %d segments", kinds, len(segs))
	return timeline.Segment{}
}
