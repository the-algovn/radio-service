package programmer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/ingest"
	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/schedule"
	"github.com/the-algovn/radio-service/internal/spend"
	"github.com/the-algovn/radio-service/internal/station"
)

// fakeClock is a settable live.Clock for deterministic MinutesAgo assertions.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time                      { return c.now }
func (c *fakeClock) Tick(time.Duration) <-chan time.Time { return make(chan time.Time) }

// harness is the shared Programmer setup for the whole package: a scripted
// model, persona dir, an on-air station with room in the budget, and every
// store buildBrief and decide need. RunOnce-level tests close one gate at a
// time from this all-open baseline; decide()-level tests call decide
// directly and never touch the gates at all.
type harness struct {
	ctx       context.Context
	prog      *Programmer
	lib       *library.MemLibrary
	requests  *request.MemStore
	airlog    *live.MemAirLog
	listeners *live.MemListeners
	sched     *schedule.MemStore
	clock     *fakeClock
	search    *fakeSearcher
	model     *scriptedModel
	station   station.Store
	ledger    *spend.MemLedger
	prod      *memProducer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	lib := library.NewMemLibrary()
	requests := request.NewMemStore()
	airlog := live.NewMemAirLog()
	listeners := live.NewMemListeners(time.Now)
	sched := schedule.NewMemStore()
	clock := &fakeClock{now: time.Now()}
	search := &fakeSearcher{byQuery: map[string][]ingest.Candidate{}}
	model := &scriptedModel{}
	personaDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(personaDir, "tieu-duong-duong.md"), []byte("PERSONA"), 0o644))
	st := station.NewMemStore()
	ctx := context.Background()
	_, err := st.GoOnAir(ctx)
	require.NoError(t, err)
	ledger := spend.NewMemLedger()
	prod := &memProducer{}
	prog := New(Deps{
		Requests: requests, Sched: sched, Library: lib, Log: airlog, Listeners: listeners,
		Clock: clock, Location: time.UTC, Search: search,
		Model: model, Fake: false, PersonaDir: personaDir,
		Station: st, Ledger: ledger, BudgetUSD: 1.0, Producer: prod,
		Rand: func(int) int { return 0 },
	})
	// New() defaults a zero Backoff to retryBackoff, so a zero-value Deps
	// field can't request "no backoff" — set it directly on the constructed
	// Programmer instead, to keep the retry tests from sleeping.
	prog.d.Backoff = 0
	return &harness{
		ctx: ctx, prog: prog, lib: lib, requests: requests,
		airlog: airlog, listeners: listeners, sched: sched, clock: clock, search: search, model: model,
		station: st, ledger: ledger, prod: prod,
	}
}

// Pending must include the programmer's OWN queued AI picks. Without this the
// DJ proposes duplicates that HasPendingYTID silently drops.
func TestBriefPendingIncludesBothSources(t *testing.T) {
	h := newHarness(t)
	_, err := h.requests.Create(h.ctx, request.Item{
		Source: request.SourceListener, YTID: "l1", Title: "Listener Song", Status: request.StatusReady,
	})
	require.NoError(t, err)
	_, err = h.requests.Create(h.ctx, request.Item{
		Source: request.SourceAI, YTID: "a1", Title: "AI Song", Status: request.StatusReady, Reason: "vì trời mưa",
	})
	require.NoError(t, err)

	b, err := h.prog.buildBrief(h.ctx)
	require.NoError(t, err)

	titles := map[string]BriefPend{}
	for _, p := range b.Pending {
		titles[p.Title] = p
	}
	require.Contains(t, titles, "Listener Song")
	require.Contains(t, titles, "AI Song")
	require.Equal(t, request.SourceAI, titles["AI Song"].Source)
	require.Equal(t, "vì trời mưa", titles["AI Song"].Reason)
}

// RecentPlays must carry the source, prior reason, and recency that live.Entry
// already records and the old brief threw away.
func TestBriefRecentPlaysCarryDetail(t *testing.T) {
	h := newHarness(t)
	// Fixed, safely-in-the-past instant: MemAirLog.History gates on real
	// wall-clock time (it hasn't "finished airing" until then), independent
	// of the Programmer's injectable clock used for MinutesAgo below.
	h.clock.now = time.Date(2020, 1, 1, 21, 0, 0, 0, time.UTC)
	require.NoError(t, h.airlog.Append(h.ctx, live.Entry{
		YTID: "p1", Title: "Past", Artist: "Someone", Source: request.SourceAI,
		Reason: "đêm khuya", StartedAt: h.clock.now.Add(-9 * time.Minute), DurationS: 200,
	}))

	b, err := h.prog.buildBrief(h.ctx)
	require.NoError(t, err)
	require.Len(t, b.RecentPlays, 1)
	require.Equal(t, "Past", b.RecentPlays[0].Title)
	require.Equal(t, "Someone", b.RecentPlays[0].Artist)
	require.Equal(t, request.SourceAI, b.RecentPlays[0].Source)
	require.Equal(t, "đêm khuya", b.RecentPlays[0].Reason)
	require.Equal(t, 9, b.RecentPlays[0].MinutesAgo)
}

// The library window rotates deterministically instead of sampling randomly, so
// the DJ eventually sees the whole shelf and tests are reproducible.
func TestBriefLibraryWindowRotatesAndResets(t *testing.T) {
	h := newHarness(t)
	total := librarySample + 5
	for i := 0; i < total; i++ {
		// Distinct, ascending AddedAt so the newest-first sort is
		// deterministic (MemLibrary's filtered() offers no tie-break for
		// equal AddedAt, and Go's map iteration order is randomized).
		require.NoError(t, h.lib.Add(h.ctx, library.Track{
			YTID: fmt.Sprintf("t%02d", i), Title: fmt.Sprintf("Track %02d", i), DurationS: 200,
			AddedAt: time.Now().Add(time.Duration(i) * time.Second),
		}))
	}

	first, err := h.prog.buildBrief(h.ctx)
	require.NoError(t, err)
	require.EqualValues(t, total, first.Library.Total)
	require.Len(t, first.Library.Sample, librarySample)

	// Second decision advances past the end: fewer rows, no wrap-around fetch.
	second, err := h.prog.buildBrief(h.ctx)
	require.NoError(t, err)
	require.Len(t, second.Library.Sample, total-librarySample)
	require.NotEqual(t, first.Library.Sample[0].YTID, second.Library.Sample[0].YTID)

	// Third resets to the top of the shelf.
	third, err := h.prog.buildBrief(h.ctx)
	require.NoError(t, err)
	require.Equal(t, first.Library.Sample[0].YTID, third.Library.Sample[0].YTID)
}

// Room state: listeners and the committed next-up track.
func TestBriefCarriesRoomState(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.listeners.Beat(h.ctx, "s1"))
	require.NoError(t, h.sched.SetNextUp(h.ctx, schedule.NextUp{YTID: "n1", Title: "Next", Channel: "Ch"}))

	b, err := h.prog.buildBrief(h.ctx)
	require.NoError(t, err)
	require.Equal(t, 1, b.Listeners)
	require.NotNil(t, b.NextUp)
	require.Equal(t, "n1", b.NextUp.YTID)
}

func TestBriefNextUpNilWhenNoneCommitted(t *testing.T) {
	h := newHarness(t)
	b, err := h.prog.buildBrief(h.ctx)
	require.NoError(t, err)
	require.Nil(t, b.NextUp)
}
