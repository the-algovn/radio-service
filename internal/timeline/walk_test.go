package timeline_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/cadence"
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

// TestDirSessionHasMusicArmsASeamWithNothingAiring pins the left operand of
// walk.go's `s.Dir.SessionHasMusic || (s.Airing != nil && ...)`. Nothing is
// airing here, so the right operand is false throughout - only the
// director's own SessionHasMusic can make the seam due. Without reading
// s.Dir.SessionHasMusic at all, the walk would derive session-has-music
// purely from Airing and wrongly suppress this seam.
func TestDirSessionHasMusicArmsASeamWithNothingAiring(t *testing.T) {
	s := liveState()
	s.Airing = nil
	s.Dir.SessionHasMusic = true
	s.Dir.FinishedSinceSeam = 1 // +1 == BreakEvery(2): owed

	up, _, _ := timeline.Project(s)

	require.Equal(t, timeline.KindDJ, up[0].Kind)
	require.Equal(t, timeline.CertaintyDue, up[0].Certainty)
}

// helpers

func isBreak(s timeline.Segment) bool {
	return s.Kind == timeline.KindDJ || s.Kind == timeline.KindStationID
}

func TestForcedBreakProjectsAsDue(t *testing.T) {
	s := liveState()
	s.Station.DJ.BreakEvery = 8 // cadence nowhere near owed
	s.Dir.FinishedSinceSeam = 0
	s.Dir.Forced = true

	up, _, _ := timeline.Project(s)

	var dj *timeline.Segment
	for i := range up {
		if up[i].Kind == timeline.KindDJ {
			dj = &up[i]
			break
		}
	}
	require.NotNil(t, dj, "a forced break must project as a row")
	require.Equal(t, timeline.CertaintyDue, dj.Certainty)
	require.True(t, dj.Forced, "the row must say it was armed, not owed")
}

func TestCadenceDueIsNotMarkedForced(t *testing.T) {
	s := liveState()
	s.Station.DJ.BreakEvery = 2
	s.Dir.FinishedSinceSeam = 1 // +1 counts the airing track: owed
	s.Dir.Forced = false

	up, _, _ := timeline.Project(s)

	for _, seg := range up {
		if seg.Kind == timeline.KindDJ && seg.Certainty == timeline.CertaintyDue {
			require.False(t, seg.Forced, "cadence-owed is not operator-armed")
			return
		}
	}
	t.Fatal("expected a cadence-due break")
}

func TestPreparedClipCarriesForcedWhenArmed(t *testing.T) {
	s := liveState()
	s.Dir.HasClip = true
	s.Dir.ClipKind = live.ClipSeam
	s.Dir.ClipDurationS = 8
	s.Dir.ClipAnchorYTID = "y1"
	s.Dir.ClipAnchorStartedAt = base
	s.Airing.YTID = "y1"
	s.Dir.Forced = true

	up, _, _ := timeline.Project(s)

	require.NotEmpty(t, up)
	require.Equal(t, timeline.CertaintyPrepared, up[0].Certainty)
	require.True(t, up[0].Forced, "Cancel must reach a prepared forced break")
}

// TestPreparedClipAdvancesCadenceWithTheEngineKind pins seamArm's middle
// return - the ENGINE kind fed to cadence.Advance - for the prepared-clip
// arm specifically. If that return is wrong (empty, or the WIRE kind by
// mistake), Advance falls to its default branch: Forced is never cleared and
// FinishedSinceSeam is never reset, so the next cadence-due seam wrongly
// inherits the operator's single arming.
func TestPreparedClipAdvancesCadenceWithTheEngineKind(t *testing.T) {
	s := liveState()
	s.Dir.HasClip = true
	s.Dir.ClipKind = live.ClipSeam
	s.Dir.ClipDurationS = 8
	s.Dir.ClipAnchorYTID = "y1"
	s.Dir.ClipAnchorStartedAt = base
	s.Airing.YTID = "y1"
	s.Dir.Forced = true

	up, _, _ := timeline.Project(s)

	require.Equal(t, timeline.CertaintyPrepared, up[0].Certainty)
	require.True(t, up[0].Forced, "the prepared clip itself carries the arming")

	for _, seg := range up[1:] {
		if seg.Kind == timeline.KindDJ {
			require.False(t, seg.Forced,
				"Forced must be cleared once the prepared clip airs in the projection - "+
					"a later seam still carrying it means Advance never ran")
		}
	}
}

// Take anchor-checks seam clips against the just-finished entry, so a seam
// prepared before any music aired is discarded. The projector must not
// promise a break that cannot air; the flag survives and fires at the first
// real seam instead.
func TestForcedBreakIsSuppressedBeforeAnyMusicAired(t *testing.T) {
	s := liveState()
	s.Dir.Forced = true
	s.Airing = nil // no music aired this session

	up, _, _ := timeline.Project(s)

	require.NotEqual(t, timeline.KindDJ, up[0].Kind,
		"a forced break cannot open the session before any music has aired")

	found := false
	for _, seg := range up {
		if seg.Kind == timeline.KindDJ {
			found = true
			break
		}
	}
	require.True(t, found, "the forced flag must survive to fire at the first real seam")
}

func TestGateSuppressesAForcedDueBreak(t *testing.T) {
	s := liveState()
	s.Dir.Forced = true
	s.Listeners = 0

	up, _, gate := timeline.Project(s)
	require.Equal(t, timeline.GateNoListeners, gate)
	for _, seg := range up {
		require.NotEqual(t, timeline.CertaintyDue, seg.Certainty,
			"ForceBreak bypasses cadence only, never the listener gate")
	}
}

// A cadence break can legitimately recur later in the 30-minute horizon once
// finishedSinceSeam resets - that is correct behaviour, matching the engine.
// What must not recur is the operator's SINGLE arming, so this counts rows
// carrying Forced, not every KindDJ row.
func TestForcedBreakFiresExactlyOnceAcrossTheHorizon(t *testing.T) {
	s := liveState()
	s.Station.DJ.BreakEvery = 8
	s.Station.DJ.StationIDMin = 0
	s.Dir.Forced = true

	up, _, _ := timeline.Project(s)

	count := 0
	for _, seg := range up {
		if seg.Kind == timeline.KindDJ && seg.Forced {
			count++
		}
	}
	require.Equal(t, 1, count, "an armed forced break must fire once, not at every seam")
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

func TestKindFromEngineMapsTheNewKindsThrough(t *testing.T) {
	require.Equal(t, timeline.KindDJ, timeline.KindFromEngine("seam"), "the one deliberate rename")
	require.Equal(t, timeline.KindStationID, timeline.KindFromEngine("station_id"))
	require.Equal(t, timeline.KindMusing, timeline.KindFromEngine(cadence.KindMusing))
	require.Equal(t, timeline.KindDaypartTransition, timeline.KindFromEngine(cadence.KindDaypartTransition))
	require.Equal(t, timeline.KindWakeGreeting, timeline.KindFromEngine(cadence.KindWakeGreeting))
	require.Equal(t, timeline.KindUnknown, timeline.KindFromEngine("dedication_read"))
}

// All three are break kinds on the wire, so an airing one must block a break
// projected on top of it. isBreakKind is an exclusion rather than an
// enumeration precisely so this keeps holding as the vocabulary grows.
func TestAnAiringNewKindBlocksABreakOnTopOfIt(t *testing.T) {
	for _, k := range []string{timeline.KindMusing, timeline.KindDaypartTransition,
		timeline.KindWakeGreeting} {
		t.Run(k, func(t *testing.T) {
			s := liveState()
			s.Airing = airing(k, 30)
			s.Dir.SessionHasMusic = true
			s.Dir.FinishedSinceSeam = 5
			s.Dir.LastStationID = base.Add(-60 * time.Minute)
			up, _, _ := timeline.Project(s)
			require.False(t, isBreak(up[0]), "a break is never followed immediately by another")
		})
	}
}

func TestWakeGreetingProjectsAheadOfAnOwedSeam(t *testing.T) {
	s := liveState()
	s.Dir.FinishedSinceSeam = 5
	s.Dir.PendingWake = true
	up, _, _ := timeline.Project(s)
	require.Equal(t, timeline.KindWakeGreeting, up[0].Kind)
	require.Equal(t, timeline.CertaintyDue, up[0].Certainty)
	require.Equal(t, timeline.EstWakeS, up[0].DurationS)
	require.False(t, up[0].Forced, "only a seam can be operator-armed")
}

func TestArmedDaypartTransitionProjectsAndExpires(t *testing.T) {
	s := liveState()
	s.Dir.FinishedSinceSeam = 5
	s.Dir.PendingDaypart = base
	up, _, _ := timeline.Project(s)
	require.Equal(t, timeline.KindDaypartTransition, up[0].Kind)
	require.Equal(t, timeline.EstDaypartS, up[0].DurationS)

	// Past the window it stops being due and the owed seam takes the slot back.
	s.Dir.PendingDaypart = base.Add(-cadence.DaypartWindow - time.Minute)
	up, _, _ = timeline.Project(s)
	require.Equal(t, timeline.KindDJ, up[0].Kind)
}

func TestMusingProjectsFromItsOwnTimer(t *testing.T) {
	s := liveState()
	s.Station.DJ.MusingEveryMin = 10
	s.Dir.LastMusing = base.Add(-11 * time.Minute)
	up, _, _ := timeline.Project(s)
	require.Equal(t, timeline.KindMusing, up[0].Kind)
	require.Equal(t, timeline.EstMusingS, up[0].DurationS)
}

// A zero LastMusing means the director has not started its clock yet, not
// "overdue since the epoch" - the same guard the station ID already has.
func TestZeroLastMusingIsNotOverdue(t *testing.T) {
	s := liveState()
	s.Station.DJ.MusingEveryMin = 10
	s.Dir.LastMusing = time.Time{}
	up, _, _ := timeline.Project(s)
	for _, seg := range up {
		require.NotEqual(t, timeline.KindMusing, seg.Kind)
	}
}
