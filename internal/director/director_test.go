package director

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/live"
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
