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

func newCoreDirector(t *testing.T, breakEvery, stationIDMin int) (*Director, *dirClock) {
	t.Helper()
	clk := newDirClock()
	dr := New(Deps{
		BreakEvery: breakEvery, StationIDMin: stationIDMin, MaxChars: 450,
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
	dr, _ := newCoreDirector(t, 2, 0)
	_, ok := dr.Take(live.Entry{YTID: "a"})
	require.False(t, ok)
}

func TestTakeFreshBacksellResetsCounter(t *testing.T) {
	dr, _ := newCoreDirector(t, 2, 0)
	anchor := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	dr.TrackFinished(live.Entry{YTID: "x"})
	p := slotClip(t, dr, live.Clip{Kind: live.ClipBacksell, AnchorYTID: "a", AnchorStartedAt: anchor})
	c, ok := dr.Take(live.Entry{YTID: "a", StartedAt: anchor})
	require.True(t, ok)
	require.Equal(t, p, c.Path)
	dr.mu.Lock()
	defer dr.mu.Unlock()
	require.Nil(t, dr.slot)
	require.Equal(t, 0, dr.finishedSinceBacksell, "hand-off resets the backsell counter")
}

func TestTakeStaleBacksellDeletesAndClears(t *testing.T) {
	dr, _ := newCoreDirector(t, 2, 0)
	anchor := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	dr.TrackFinished(live.Entry{YTID: "x"})
	p := slotClip(t, dr, live.Clip{Kind: live.ClipBacksell, AnchorYTID: "a", AnchorStartedAt: anchor})
	_, ok := dr.Take(live.Entry{YTID: "b", StartedAt: anchor.Add(time.Minute)})
	require.False(t, ok)
	_, err := os.Stat(p)
	require.True(t, os.IsNotExist(err), "stale clip file must be deleted inside Take")
	dr.mu.Lock()
	defer dr.mu.Unlock()
	require.Nil(t, dr.slot, "slot cleared — no livelock on the slot-empty gate")
	require.Equal(t, 1, dr.finishedSinceBacksell, "stale discard does NOT reset the counter")
}

func TestTakeStationIDAlwaysFreshAndStampsTimer(t *testing.T) {
	dr, clk := newCoreDirector(t, 0, 60)
	slotClip(t, dr, live.Clip{Kind: live.ClipStationID})
	_, ok := dr.Take(live.Entry{YTID: "whatever", StartedAt: clk.Now()})
	require.True(t, ok)
	dr.mu.Lock()
	defer dr.mu.Unlock()
	require.Equal(t, clk.Now(), dr.lastStationID, "hand-off stamps the station-id timer")
}

func TestDueKindArithmetic(t *testing.T) {
	dr, clk := newCoreDirector(t, 2, 0)
	dr.mu.Lock()
	require.Equal(t, "", dr.dueKindLocked(clk.Now()), "0 finished + current = 1 < 2")
	dr.mu.Unlock()
	dr.TrackFinished(live.Entry{YTID: "a"})
	dr.mu.Lock()
	require.Equal(t, live.ClipBacksell, dr.dueKindLocked(clk.Now()), "1 finished + current = 2 >= 2")
	dr.mu.Unlock()
}

func TestDueKindStationIDWinsAndBreakEveryZeroDisables(t *testing.T) {
	dr, clk := newCoreDirector(t, 2, 60)
	dr.mu.Lock()
	dr.lastStationID = clk.Now()
	dr.mu.Unlock()
	dr.TrackFinished(live.Entry{YTID: "a"}) // backsell due
	clk.advance(61 * time.Minute)           // station id also due
	dr.mu.Lock()
	require.Equal(t, live.ClipStationID, dr.dueKindLocked(clk.Now()), "station_id wins; backsell carries over")
	dr.mu.Unlock()

	off, _ := newCoreDirector(t, 0, 0)
	off.TrackFinished(live.Entry{YTID: "a"})
	off.mu.Lock()
	require.Equal(t, "", off.dueKindLocked(clk.Now()), "both knobs 0 = nothing ever due")
	off.mu.Unlock()
}

func TestCancelPendingDeletesClip(t *testing.T) {
	dr, _ := newCoreDirector(t, 2, 0)
	p := slotClip(t, dr, live.Clip{Kind: live.ClipBacksell, AnchorYTID: "a"})
	dr.mu.Lock()
	dr.cancelPendingLocked("test")
	dr.mu.Unlock()
	_, err := os.Stat(p)
	require.True(t, os.IsNotExist(err))
	dr.mu.Lock()
	defer dr.mu.Unlock()
	require.Nil(t, dr.slot)
}

func TestRingCapsAtFive(t *testing.T) {
	dr, _ := newCoreDirector(t, 2, 0)
	for i := 0; i < 7; i++ {
		dr.pushRing("tóm tắt", []string{"câu"})
	}
	dr.mu.Lock()
	defer dr.mu.Unlock()
	require.Len(t, dr.ring, 5)
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
	f.dr.TrackFinished(live.Entry{YTID: "a"}) // backsell due
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
	p := slotClip(t, f.dr, live.Clip{Kind: live.ClipBacksell, AnchorYTID: "a"})
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
