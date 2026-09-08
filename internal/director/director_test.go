package director

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/spend"
	"github.com/the-algovn/radio-service/internal/station"
)

// dirClock is a settable clock for director tests (Tick unused here).
type dirClock struct {
	mu sync.Mutex
	t  time.Time
	c  chan time.Time
}

func newDirClock() *dirClock {
	return &dirClock{t: time.Date(2026, 7, 22, 22, 0, 0, 0, time.UTC), c: make(chan time.Time, 1)}
}
func (c *dirClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *dirClock) Tick(time.Duration) <-chan time.Time { return c.c }
func (c *dirClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newCoreDirector(t *testing.T) (*Director, *dirClock) {
	t.Helper()
	clk := newDirClock()
	dr := New(Deps{
		StationIDsPath: writeIDs(t, "đài thân mến\n"),
		DataDir:        t.TempDir(), Clock: clk,
	})
	return dr, clk
}

func slotClip(t *testing.T, dr *Director, c live.Clip) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.pcm")
	require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
	c.Path = p
	dr.mu.Lock()
	dr.slot = &c
	dr.mu.Unlock()
	return p
}

var _ live.TalkSource = (*Director)(nil)

func TestTakeEmptySlot(t *testing.T) {
	dr, _ := newCoreDirector(t)
	_, ok := dr.Take(live.Entry{YTID: "a"})
	require.False(t, ok)
}

func TestTakeFreshSeamResetsCounter(t *testing.T) {
	dr, _ := newCoreDirector(t)
	anchor := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	dr.TrackFinished(live.Entry{YTID: "x"})
	p := slotClip(t, dr, live.Clip{Kind: live.ClipSeam, AnchorYTID: "a", AnchorStartedAt: anchor})
	c, ok := dr.Take(live.Entry{YTID: "a", StartedAt: anchor})
	require.True(t, ok)
	require.Equal(t, p, c.Path)
	dr.mu.Lock()
	defer dr.mu.Unlock()
	require.Nil(t, dr.slot)
	require.Equal(t, 0, dr.cad.FinishedSinceSeam, "hand-off resets the seam counter")
}

// TestTakeFreshAcrossPrecisionLoss covers the CRITICAL fix: the anchor
// round-trips through Postgres TIMESTAMPTZ (microsecond precision) while the
// feeder's entry carries the sample clock's nanoseconds — a clip anchored at
// the microsecond-truncated timestamp must still match the nanosecond-precise
// justFinished entry within the 1s tolerance.
func TestTakeFreshAcrossPrecisionLoss(t *testing.T) {
	dr, _ := newCoreDirector(t)
	ts := time.Date(2026, 7, 22, 21, 0, 0, 123456789, time.UTC)
	dr.TrackFinished(live.Entry{YTID: "x"})
	p := slotClip(t, dr, live.Clip{Kind: live.ClipSeam, AnchorYTID: "a", AnchorStartedAt: ts.Truncate(time.Microsecond)})
	c, ok := dr.Take(live.Entry{YTID: "a", StartedAt: ts})
	require.True(t, ok)
	require.Equal(t, p, c.Path)
	dr.mu.Lock()
	defer dr.mu.Unlock()
	require.Nil(t, dr.slot)
	require.Equal(t, 0, dr.cad.FinishedSinceSeam, "hand-off resets the seam counter")
}

// TestTakeStaleSameYTIDDifferentAiring covers the other half of the OR: same
// YTID as the anchor, but a StartedAt two minutes later — a different airing
// of the same track, well outside the 1s tolerance — must still be discarded
// as stale.
func TestTakeStaleSameYTIDDifferentAiring(t *testing.T) {
	dr, _ := newCoreDirector(t)
	anchor := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	dr.TrackFinished(live.Entry{YTID: "x"})
	p := slotClip(t, dr, live.Clip{Kind: live.ClipSeam, AnchorYTID: "a", AnchorStartedAt: anchor})
	_, ok := dr.Take(live.Entry{YTID: "a", StartedAt: anchor.Add(2 * time.Minute)})
	require.False(t, ok)
	_, err := os.Stat(p)
	require.True(t, os.IsNotExist(err), "stale clip file must be deleted inside Take")
	dr.mu.Lock()
	defer dr.mu.Unlock()
	require.Nil(t, dr.slot, "slot cleared — no livelock on the slot-empty gate")
	require.Equal(t, 1, dr.cad.FinishedSinceSeam, "stale discard does NOT reset the counter")
}

func TestTakeStaleSeamDeletesAndClears(t *testing.T) {
	dr, _ := newCoreDirector(t)
	anchor := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	dr.TrackFinished(live.Entry{YTID: "x"})
	p := slotClip(t, dr, live.Clip{Kind: live.ClipSeam, AnchorYTID: "a", AnchorStartedAt: anchor})
	_, ok := dr.Take(live.Entry{YTID: "b", StartedAt: anchor.Add(time.Minute)})
	require.False(t, ok)
	_, err := os.Stat(p)
	require.True(t, os.IsNotExist(err), "stale clip file must be deleted inside Take")
	dr.mu.Lock()
	defer dr.mu.Unlock()
	require.Nil(t, dr.slot, "slot cleared — no livelock on the slot-empty gate")
	require.Equal(t, 1, dr.cad.FinishedSinceSeam, "stale discard does NOT reset the counter")
}

func TestTakeStationIDAlwaysFreshAndStampsTimer(t *testing.T) {
	dr, clk := newCoreDirector(t)
	slotClip(t, dr, live.Clip{Kind: live.ClipStationID})
	_, ok := dr.Take(live.Entry{YTID: "whatever", StartedAt: clk.Now()})
	require.True(t, ok)
	dr.mu.Lock()
	defer dr.mu.Unlock()
	require.Equal(t, clk.Now(), dr.cad.LastStationID, "hand-off stamps the station-id timer")
}

func TestDueKindArithmetic(t *testing.T) {
	dr, clk := newCoreDirector(t)
	dj := station.DJSettings{BreakEvery: 2}
	dr.cad.SessionHasMusic = true
	dr.mu.Lock()
	require.Equal(t, "", dr.dueKindLocked(clk.Now(), dj), "0 finished + current = 1 < 2")
	dr.mu.Unlock()
	dr.TrackFinished(live.Entry{YTID: "a"})
	dr.mu.Lock()
	require.Equal(t, live.ClipSeam, dr.dueKindLocked(clk.Now(), dj), "1 finished + current = 2 >= 2")
	dr.mu.Unlock()
}

func TestDueKindStationIDWinsAndBreakEveryZeroDisables(t *testing.T) {
	dr, clk := newCoreDirector(t)
	dj := station.DJSettings{BreakEvery: 2, StationIDMin: 60}
	dr.mu.Lock()
	dr.cad.LastStationID = clk.Now()
	dr.cad.SessionHasMusic = true
	dr.mu.Unlock()
	dr.TrackFinished(live.Entry{YTID: "a"}) // seam due
	clk.advance(61 * time.Minute)           // station id also due
	dr.mu.Lock()
	require.Equal(t, live.ClipStationID, dr.dueKindLocked(clk.Now(), dj), "station_id wins; seam carries over")
	dr.mu.Unlock()

	off, _ := newCoreDirector(t)
	off.TrackFinished(live.Entry{YTID: "a"})
	off.mu.Lock()
	require.Equal(t, "", off.dueKindLocked(clk.Now(), station.DJSettings{}), "both knobs 0 = nothing ever due")
	off.mu.Unlock()
}

func TestCancelPendingDeletesClip(t *testing.T) {
	dr, _ := newCoreDirector(t)
	p := slotClip(t, dr, live.Clip{Kind: live.ClipSeam, AnchorYTID: "a"})
	dr.mu.Lock()
	dr.cancelPendingLocked("test")
	dr.mu.Unlock()
	_, err := os.Stat(p)
	require.True(t, os.IsNotExist(err))
	dr.mu.Lock()
	defer dr.mu.Unlock()
	require.Nil(t, dr.slot)
}

func onAir(t *testing.T, f *prepFixture) {
	t.Helper()
	_, err := f.dr.d.Station.GoOnAir(context.Background())
	require.NoError(t, err)
}

func withListener(t *testing.T, f *prepFixture) {
	t.Helper()
	require.NoError(t, f.dr.d.Listeners.Beat(context.Background(), "s1"))
}

func seedAirLog(t *testing.T, f *prepFixture) {
	t.Helper()
	require.NoError(t, f.log.Append(context.Background(),
		live.Entry{YTID: "a", Title: "Bài A", StartedAt: time.Now()}))
}

func slotFilled(dr *Director) bool {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	return dr.slot != nil
}

func TestRunOnceOffAirNoPrep(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	withListener(t, f)
	seedAirLog(t, f)
	f.dr.TrackFinished(live.Entry{YTID: "a"}) // seam due
	f.dr.RunOnce(context.Background())
	require.False(t, slotFilled(f.dr))
	require.Zero(t, f.model.calls)
}

func TestRunOncePreparesWhenDue(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	onAir(t, f)
	withListener(t, f)
	seedAirLog(t, f)
	f.dr.RunOnce(context.Background()) // on-air transition observed; nothing due yet
	require.False(t, slotFilled(f.dr))
	f.dr.TrackFinished(live.Entry{YTID: "a"}) // 1 finished + current = 2 >= 2 → due
	f.dr.RunOnce(context.Background())
	require.True(t, slotFilled(f.dr))
	require.Equal(t, 1, f.model.calls)
	// Slot occupied → next tick must not prep again.
	f.dr.RunOnce(context.Background())
	require.Equal(t, 1, f.model.calls)
}

func TestRunOnceNoListenersNoPrep(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	onAir(t, f)
	seedAirLog(t, f)
	f.dr.TrackFinished(live.Entry{YTID: "a"})
	f.dr.RunOnce(context.Background())
	require.False(t, slotFilled(f.dr))
	require.Zero(t, f.model.calls)
}

func TestRunOnceBudgetReachedNoPrep(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	onAir(t, f)
	withListener(t, f)
	seedAirLog(t, f)
	f.dr.TrackFinished(live.Entry{YTID: "a"})
	require.NoError(t, f.ledger.Append(context.Background(), spend.Line{TS: time.Now(), Kind: "llm", CostUSD: 1.0}))
	f.dr.RunOnce(context.Background())
	require.False(t, slotFilled(f.dr))
	require.Zero(t, f.model.calls)
}

func TestRunOncePauseCancelsPendingClip(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	onAir(t, f)
	p := slotClip(t, f.dr, live.Clip{Kind: live.ClipSeam, AnchorYTID: "a"})
	_, err := f.dr.d.Station.SetAIEnabled(context.Background(), false)
	require.NoError(t, err)
	f.dr.RunOnce(context.Background())
	require.False(t, slotFilled(f.dr))
	_, serr := os.Stat(p)
	require.True(t, os.IsNotExist(serr), "pause deletes the pending clip file")
}

func TestRunOnceOnAirTransitionResetsStationIDTimer(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	withListener(t, f)
	// Off-air first tick: lastStationID stays zero but nothing preps.
	f.dr.RunOnce(context.Background())
	onAir(t, f)
	// First on-air tick observes the transition → timer = now → NOT due,
	// even though zero-time would have made it "due" an hour after 1970.
	f.dr.RunOnce(context.Background())
	require.False(t, slotFilled(f.dr))
	f.clk.advance(61 * time.Minute)
	f.dr.RunOnce(context.Background())
	require.True(t, slotFilled(f.dr))
	f.dr.mu.Lock()
	require.Equal(t, live.ClipStationID, f.dr.slot.Kind)
	f.dr.mu.Unlock()
}

func TestSnapshotCopiesTheSlotAndNeverLeaksIt(t *testing.T) {
	dr, _ := newCoreDirector(t)
	dr.slot = &live.Clip{Path: "/tmp/x.pcm", Kind: live.ClipSeam, DurationS: 12, CorrelationID: "c1"}

	snap := dr.Snapshot()
	require.True(t, snap.Present)
	require.True(t, snap.HasClip)
	require.Equal(t, "c1", snap.ClipCorrelationID)

	dr.slot = nil // simulate Take
	require.True(t, snap.HasClip, "the snapshot must be a value copy, not a view")
}

func TestSnapshotDoesNotMutateTheFormatClock(t *testing.T) {
	dr, clk := newCoreDirector(t)
	dr.cad.FinishedSinceSeam = 3
	dr.cad.LastStationID = clk.Now()
	before := dr.cad.FinishedSinceSeam

	_ = dr.Snapshot()
	require.Equal(t, before, dr.cad.FinishedSinceSeam)
}

func TestSnapshotIsRaceFreeWithTake(t *testing.T) {
	dr, _ := newCoreDirector(t)
	done := make(chan struct{})
	go func() {
		for range 200 {
			_ = dr.Snapshot()
		}
		close(done)
	}()
	forceDone := make(chan struct{})
	go func() {
		for range 200 {
			_ = dr.ForceBreak()
		}
		close(forceDone)
	}()
	cancelDone := make(chan struct{})
	go func() {
		for range 200 {
			_ = dr.CancelPrepared()
		}
		close(cancelDone)
	}()
	for range 200 {
		dr.mu.Lock()
		dr.slot = &live.Clip{Path: "/nonexistent", Kind: live.ClipStationID}
		dr.mu.Unlock()
		_, _ = dr.Take(live.Entry{})
	}
	<-done
	<-forceDone
	<-cancelDone
}

func TestRunExitsOnCancel(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.dr.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Run did not exit on cancel")
	}
}

func TestForceBreakArmsTheSeam(t *testing.T) {
	dr, _ := newCoreDirector(t)
	dj := station.DJSettings{BreakEvery: 4, StationIDMin: 0}
	dr.cad.SessionHasMusic = true

	require.Equal(t, "", dr.dueKindLocked(time.Now(), dj), "not due before forcing")
	require.True(t, dr.ForceBreak(), "arming an empty slot returns armed")
	require.Equal(t, live.ClipSeam, dr.dueKindLocked(time.Now(), dj), "forced seam is due")
}

func TestForceBreakNeverPreemptsAnOwedStationID(t *testing.T) {
	dr, _ := newCoreDirector(t)
	// StationIDMin elapsed and IDs loaded: the station ID is owed.
	dj := station.DJSettings{BreakEvery: 4, StationIDMin: 1}
	dr.cad.LastStationID = time.Now().Add(-2 * time.Minute)
	dr.cad.SessionHasMusic = true

	require.True(t, dr.ForceBreak())
	require.Equal(t, live.ClipStationID, dr.dueKindLocked(time.Now(), dj),
		"the forced check sits BELOW the station-ID check")
}

func TestForceBreakIsIdempotent(t *testing.T) {
	dr, _ := newCoreDirector(t)
	require.True(t, dr.ForceBreak())
	require.True(t, dr.ForceBreak(), "already armed is still 'a break is now due'")
}

func TestForceBreakNoOpsAgainstAPreparedClip(t *testing.T) {
	dr, _ := newCoreDirector(t)
	slotClip(t, dr, live.Clip{Kind: live.ClipSeam})

	require.False(t, dr.ForceBreak(), "a clip is already rendered; nothing to arm")
	require.False(t, dr.Snapshot().Forced)
}

func TestSeamTakeClearsTheFlag(t *testing.T) {
	dr, _ := newCoreDirector(t)
	entry := live.Entry{YTID: "abc", StartedAt: time.Now()}
	slotClip(t, dr, live.Clip{
		Kind: live.ClipSeam, AnchorYTID: entry.YTID, AnchorStartedAt: entry.StartedAt,
	})
	dr.cad.Forced = true

	_, ok := dr.Take(entry)
	require.True(t, ok)
	require.False(t, dr.Snapshot().Forced, "a forced break fires exactly once")
}

func TestStationIDTakeDoesNotClearTheFlag(t *testing.T) {
	dr, _ := newCoreDirector(t)
	slotClip(t, dr, live.Clip{Kind: live.ClipStationID})
	dr.cad.Forced = true

	_, ok := dr.Take(live.Entry{YTID: "abc", StartedAt: time.Now()})
	require.True(t, ok)
	require.True(t, dr.Snapshot().Forced, "a forced seam is still owed after an ID airs")
}

func TestStaleDiscardDoesNotClearTheFlag(t *testing.T) {
	dr, _ := newCoreDirector(t)
	slotClip(t, dr, live.Clip{
		Kind: live.ClipSeam, AnchorYTID: "stale", AnchorStartedAt: time.Now().Add(-time.Hour),
	})
	dr.cad.Forced = true

	_, ok := dr.Take(live.Entry{YTID: "fresh", StartedAt: time.Now()})
	require.False(t, ok, "anchor drifted; the clip is discarded")
	require.True(t, dr.Snapshot().Forced, "the break is still owed and re-preps")
}

// The early-return trap. cancelPendingLocked returns immediately on an empty
// slot, so a clear written below that return would leave the station armed
// and the operator's Cancel would do nothing for a full 20s tick.
func TestCancelClearsTheFlagWithAnEmptySlot(t *testing.T) {
	dr, _ := newCoreDirector(t)
	require.True(t, dr.ForceBreak())
	require.Nil(t, dr.slot, "armed, nothing prepared yet")

	require.True(t, dr.CancelPrepared(), "an arming is a thing to cancel")
	require.False(t, dr.Snapshot().Forced)
}

func TestCancelWithNothingArmedOrPrepared(t *testing.T) {
	dr, _ := newCoreDirector(t)
	require.False(t, dr.CancelPrepared(), "nothing to cancel")
}

func TestCancelDiscardsAPreparedClipAndTheFlag(t *testing.T) {
	dr, _ := newCoreDirector(t)
	path := slotClip(t, dr, live.Clip{Kind: live.ClipSeam})
	dr.cad.Forced = true

	require.True(t, dr.CancelPrepared())
	require.Nil(t, dr.slot)
	require.False(t, dr.Snapshot().Forced)
	require.NoFileExists(t, path)
}

func TestSnapshotCarriesForced(t *testing.T) {
	dr, _ := newCoreDirector(t)
	require.False(t, dr.Snapshot().Forced)
	require.True(t, dr.ForceBreak())
	require.True(t, dr.Snapshot().Forced)
}

func TestNoSeamBeforeAnyMusicThisSession(t *testing.T) {
	// A seam prepared before the session's first track is anchored to the
	// PREVIOUS broadcast's last track and is discarded by Take against the
	// zero Entry, so preparing one buys an LLM call and a TTS bill for a clip
	// that can never air.
	dr, clk := newCoreDirector(t)
	dj := station.DJSettings{BreakEvery: 2}

	dr.cad.FinishedSinceSeam = 5
	require.Equal(t, "", dr.dueKindLocked(clk.Now(), dj),
		"nothing is due before the session's first track finishes")

	dr.TrackFinished(live.Entry{YTID: "y1"})
	require.Equal(t, live.ClipSeam, dr.dueKindLocked(clk.Now(), dj),
		"the first finished track opens the seam")
}

func TestGoingOnAirStartsTheSessionSilent(t *testing.T) {
	// The counter carries across sessions. Without the reset, a seam is due
	// the instant a new broadcast opens, is anchored to the PREVIOUS session's
	// last track, and is discarded by Take - one wasted LLM call plus TTS on
	// every session open.
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	withListener(t, f)
	seedAirLog(t, f)

	f.dr.TrackFinished(live.Entry{YTID: "a"})
	f.dr.TrackFinished(live.Entry{YTID: "b"})
	require.True(t, f.dr.cad.SessionHasMusic)
	require.Equal(t, 2, f.dr.cad.FinishedSinceSeam)

	onAir(t, f)
	f.dr.RunOnce(context.Background())

	require.False(t, f.dr.cad.SessionHasMusic, "a new session has heard no music yet")
	require.Equal(t, 0, f.dr.cad.FinishedSinceSeam, "the format clock restarts with the session")
	require.False(t, slotFilled(f.dr), "no clip is prepared before the session's first track")
	require.Zero(t, f.model.calls, "and nothing is paid for one")
}

func TestSnapshotCarriesSessionHasMusic(t *testing.T) {
	dr, _ := newCoreDirector(t)

	require.False(t, dr.Snapshot().SessionHasMusic)
	dr.TrackFinished(live.Entry{YTID: "y1"})
	require.True(t, dr.Snapshot().SessionHasMusic,
		"the projector re-implements the due test and needs this term")
}

func TestGoingOnAirArmsTheGreetingAndSeedsTheDaypart(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	withListener(t, f)
	onAir(t, f)

	f.dr.RunOnce(context.Background())

	require.True(t, f.dr.cad.PendingWake, "a new broadcast owes a greeting")
	require.NotEmpty(t, f.dr.lastDaypart, "seeded, so the first tick cannot fire a bogus rollover")
	require.True(t, f.dr.cad.PendingDaypart.IsZero(), "and owes no rollover it never saw")
	require.Zero(t, f.dr.silentForMin, "she was not silent; the station was off")
}

// The greeting is what the session-open waste turns into. Before the fix a
// carried counter bought an LLM call for a seam Take was guaranteed to
// discard; now the first finished track opens a greeting instead.
func TestTheFirstTrackOfASessionOpensAGreetingNotASeam(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	withListener(t, f)
	onAir(t, f)
	f.dr.RunOnce(context.Background())

	f.dr.TrackFinished(live.Entry{YTID: "a"})
	require.Equal(t, live.ClipWakeGreeting,
		f.dr.dueKindLocked(f.clk.Now(), station.DJSettings{BreakEvery: 2}))
}

// An AIEnabled resume is NOT a new session. The station stayed on air and the
// feeder kept calling TrackFinished, so the counter and SessionHasMusic must
// survive - the greeting outranks the owed seam in the ladder and Advance
// resets the counter when it airs, with no special case anywhere.
func TestAIEnabledResumeKeepsTheSessionButArmsTheGreeting(t *testing.T) {
	ctx := context.Background()
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	withListener(t, f)
	onAir(t, f)
	f.dr.RunOnce(ctx)

	for i := 0; i < 9; i++ {
		f.dr.TrackFinished(live.Entry{YTID: "y"})
	}
	_, err := f.dr.d.Station.SetAIEnabled(ctx, false)
	require.NoError(t, err)
	f.dr.RunOnce(ctx)
	f.clk.advance(42 * time.Minute)
	_, err = f.dr.d.Station.SetAIEnabled(ctx, true)
	require.NoError(t, err)
	f.dr.RunOnce(ctx)

	require.True(t, f.dr.cad.PendingWake)
	require.True(t, f.dr.cad.SessionHasMusic, "music kept playing while she was quiet")
	require.Equal(t, 9, f.dr.cad.FinishedSinceSeam, "a resume is not a new session")
	require.Equal(t, 42, f.dr.silentForMin)
	require.True(t, f.dr.aiDisabledSince.IsZero(), "consumed once it has an answer")
	require.Equal(t, live.ClipWakeGreeting,
		f.dr.dueKindLocked(f.clk.Now(), station.DJSettings{BreakEvery: 2}),
		"the greeting outranks the seam the counter owes")
}

// AI can go quiet, then the station itself go dark, then BOTH come back on
// the same tick. Without clearing aiDisabledSince in the on-air block, the
// resume block below it would still see the stale timestamp and report the
// whole dead-air span as silence - a false statement, since the station
// itself was off for most of it, not merely quiet.
func TestGoingOnAirSameTickAsAIResumeClearsStaleSilence(t *testing.T) {
	ctx := context.Background()
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	withListener(t, f)
	onAir(t, f)
	f.dr.RunOnce(ctx) // 22:00

	_, err := f.dr.d.Station.SetAIEnabled(ctx, false)
	require.NoError(t, err)
	f.dr.RunOnce(ctx) // AI disabled at 22:00; aiDisabledSince stamped

	_, err = f.dr.d.Station.GoOffAir(ctx)
	require.NoError(t, err)
	f.clk.advance(5 * time.Minute)
	f.dr.RunOnce(ctx) // 22:05 - the station itself goes dark too

	f.clk.advance(175 * time.Minute) // 01:00 - three hours since AI went quiet
	_, err = f.dr.d.Station.SetAIEnabled(ctx, true)
	require.NoError(t, err)
	onAir(t, f)
	f.dr.RunOnce(ctx) // on-air and the AI resume land on the SAME tick

	require.Zero(t, f.dr.silentForMin,
		"the station itself was dark; a new session owes no silence report")
}

// A stale silentForMin from a PREVIOUS session must not ride into a new
// session's greeting: a listener would hear "she was quiet for 12 minutes"
// from a pause that happened last session, not this one.
func TestGoingOnAirClearsAStaleSilentForMinFromThePreviousSession(t *testing.T) {
	ctx := context.Background()
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	withListener(t, f)
	onAir(t, f)
	f.dr.RunOnce(ctx) // session 1 opens

	_, err := f.dr.d.Station.SetAIEnabled(ctx, false)
	require.NoError(t, err)
	f.dr.RunOnce(ctx)
	f.clk.advance(12 * time.Minute)
	_, err = f.dr.d.Station.SetAIEnabled(ctx, true)
	require.NoError(t, err)
	f.dr.RunOnce(ctx) // resume; silentForMin becomes 12
	require.Equal(t, 12, f.dr.silentForMin)

	_, err = f.dr.d.Station.GoOffAir(ctx)
	require.NoError(t, err)
	f.dr.RunOnce(ctx) // session 1 ends

	f.clk.advance(time.Hour)
	onAir(t, f)
	f.dr.RunOnce(ctx) // session 2 opens

	require.Zero(t, f.dr.silentForMin,
		"a new session owes no silence report carried from the one before it")
}

func TestDaypartRolloverArmsTheTransitionAndRemembersWhatItLeft(t *testing.T) {
	ctx := context.Background()
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	withListener(t, f)
	onAir(t, f)
	f.dr.RunOnce(ctx) // seeds lastDaypart at 22:00 UTC == "đêm"
	require.Equal(t, "đêm", f.dr.lastDaypart)

	f.clk.advance(7 * time.Hour) // 05:00 == "sáng"
	f.dr.RunOnce(ctx)

	require.Equal(t, f.clk.Now(), f.dr.cad.PendingDaypart)
	require.Equal(t, "đêm", f.dr.daypartFrom)
	require.Equal(t, "sáng", f.dr.lastDaypart)
}

func TestNoRolloverWithinTheSameDaypart(t *testing.T) {
	ctx := context.Background()
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	withListener(t, f)
	onAir(t, f)
	f.dr.RunOnce(ctx)

	f.clk.advance(30 * time.Minute) // 22:30, still "đêm"
	f.dr.RunOnce(ctx)

	require.True(t, f.dr.cad.PendingDaypart.IsZero())
}

// A SECOND session opening in a different daypart from the one the previous
// session ended in must not owe a rollover. The wake greeting already covers
// a session open; a daypart transition on top of it would have her announce
// "it has just turned evening" when evening turned two hours ago while the
// station was dark - a false statement, not a late one.
func TestGoingOnAirDoesNotOweARolloverThatHappenedOffAir(t *testing.T) {
	ctx := context.Background()
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	withListener(t, f)
	onAir(t, f)
	f.dr.RunOnce(ctx) // seeds lastDaypart at 22:00 UTC == "đêm"

	_, err := f.dr.d.Station.GoOffAir(ctx)
	require.NoError(t, err)
	f.dr.RunOnce(ctx) // dead air; lastDaypart is an edge detector, not cleared

	f.clk.advance(7 * time.Hour) // 05:00 == "sáng", crossed while off air
	onAir(t, f)
	f.dr.RunOnce(ctx) // a new broadcast session opens

	require.True(t, f.dr.cad.PendingDaypart.IsZero(),
		"opening a broadcast must not owe a rollover that happened during dead air")
}

// Going off air is precisely what should leave a greeting owed. cancelPending
// disarms the operator's forced break because that is an intent that expires;
// a greeting is a debt.
func TestPauseAndOffAirLeaveTheGreetingAndTheRolloverOwed(t *testing.T) {
	dr, clk := newCoreDirector(t)
	dr.cad.PendingWake = true
	dr.cad.PendingDaypart = clk.Now()
	dr.cad.Forced = true

	dr.mu.Lock()
	dr.cancelPendingLocked("paused or off-air")
	dr.mu.Unlock()

	require.False(t, dr.cad.Forced, "an arming is an intent, and it expires")
	require.True(t, dr.cad.PendingWake, "a greeting is a debt, and it does not")
	require.False(t, dr.cad.PendingDaypart.IsZero())
}

func TestSnapshotCarriesTheProjectableCadenceTerms(t *testing.T) {
	dr, clk := newCoreDirector(t)
	dr.cad.LastMusing = clk.Now()
	dr.cad.PendingDaypart = clk.Now().Add(-time.Minute)
	dr.cad.PendingWake = true

	s := dr.Snapshot()
	require.Equal(t, dr.cad.LastMusing, s.LastMusing)
	require.Equal(t, dr.cad.PendingDaypart, s.PendingDaypart)
	require.True(t, s.PendingWake)
}
