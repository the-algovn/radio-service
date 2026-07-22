package programmer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/brain"
	"github.com/the-algovn/radio-service/internal/ingest"
	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/playlist"
	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/spend"
)

type scriptedModel struct {
	raw   string
	err   error
	calls int
}

func (m *scriptedModel) Name() string { return "gemini-test" }
func (m *scriptedModel) Generate(context.Context, string, string) (string, brain.Usage, error) {
	m.calls++
	return m.raw, brain.Usage{In: 1000, Out: 100}, m.err
}

type scriptedSearch struct {
	byQuery map[string][]ingest.Candidate
	calls   int
}

func (s *scriptedSearch) Search(_ context.Context, q string, _ int) ([]ingest.Candidate, error) {
	s.calls++
	return s.byQuery[q], nil
}

type frame struct{ topic, value string }
type memProducer struct{ frames []frame }

func (p *memProducer) Publish(_ context.Context, topic string, value []byte) error {
	p.frames = append(p.frames, frame{topic, string(value)})
	return nil
}

type fixture struct {
	prog    *Programmer
	model   *scriptedModel
	search  *scriptedSearch
	station playlist.Store
	reqs    *request.MemStore
	lib     *library.MemLibrary
	log     *live.MemAirLog
	lst     *live.MemListeners
	ledger  *spend.MemLedger
	prod    *memProducer
}

// newFixture: on-air station, 3 library tracks, one live listener, empty
// queue, zero spend — every gate OPEN unless a test closes one.
func newFixture(t *testing.T, model *scriptedModel) *fixture {
	t.Helper()
	ctx := context.Background()
	lib := library.NewMemLibrary()
	for _, id := range []string{"lib1", "lib2", "lib3"} {
		require.NoError(t, lib.Add(ctx, library.Track{
			YTID: id, Title: "t-" + id, Channel: "c-" + id, DurationS: 240, ArtifactID: "a-" + id,
		}))
	}
	st := playlist.NewMemStore(lib)
	_, err := st.GoOnAir(ctx)
	require.NoError(t, err)
	lst := live.NewMemListeners(time.Now)
	require.NoError(t, lst.Beat(ctx, "tab-1"))

	personaDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(personaDir, "tieu-duong-duong.md"), []byte("PERSONA"), 0o644))

	f := &fixture{
		model: model, search: &scriptedSearch{byQuery: map[string][]ingest.Candidate{}},
		station: st, reqs: request.NewMemStore(), lib: lib,
		log: live.NewMemAirLog(), lst: lst, ledger: spend.NewMemLedger(), prod: &memProducer{},
	}
	f.prog = New(Deps{
		Model: model, Fake: false, PersonaDir: personaDir,
		Station: st, Requests: f.reqs, Library: lib, Log: f.log, Listeners: lst,
		Search: f.search, Ledger: f.ledger, BudgetUSD: 1.0, Producer: f.prod,
		Clock: live.RealClock(), Rand: func(int) int { return 0 },
		Location: time.FixedZone("ICT", 7*3600),
	})
	return f
}

func TestGatesProduceNoModelCalls(t *testing.T) {
	ctx := context.Background()

	t.Run("off-air", func(t *testing.T) {
		f := newFixture(t, &scriptedModel{})
		_, err := f.station.GoOffAir(ctx)
		require.NoError(t, err)
		f.prog.RunOnce(ctx)
		require.Zero(t, f.model.calls)
	})

	t.Run("zero listeners", func(t *testing.T) {
		f := newFixture(t, &scriptedModel{})
		f.lst = live.NewMemListeners(time.Now) // fresh, nobody beat
		f.prog.d.Listeners = f.lst
		f.prog.RunOnce(ctx)
		require.Zero(t, f.model.calls)
	})

	t.Run("queue depth at target", func(t *testing.T) {
		f := newFixture(t, &scriptedModel{})
		for _, id := range []string{"q1", "q2", "q3"} {
			_, err := f.reqs.Create(ctx, request.Item{Source: request.SourceListener,
				RequestedBy: "u", YTID: id, Title: id, Channel: "c", DurationS: 100,
				Status: request.StatusApproved})
			require.NoError(t, err)
		}
		f.prog.RunOnce(ctx)
		require.Zero(t, f.model.calls)
	})

	t.Run("budget cap hit", func(t *testing.T) {
		f := newFixture(t, &scriptedModel{})
		require.NoError(t, f.ledger.Append(ctx, spend.Line{TS: time.Now(), Kind: "llm", Provider: "gemini", CostUSD: 1.0}))
		f.prog.RunOnce(ctx)
		require.Zero(t, f.model.calls)
	})
}

func TestQueryPickSearchesFiltersEnqueues(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, &scriptedModel{raw: `{"picks":[{"query":"nhạc đêm","reason":"khuya"}]}`})
	f.search.byQuery["nhạc đêm"] = []ingest.Candidate{
		{YTID: "long1", Title: "10h mix", Channel: "Mix - Topic", DurationS: 36000, ViewCount: 5000}, // > 600s → filtered
		{YTID: "new1", Title: "Bài Mới", Channel: "Ca Sĩ - Topic", DurationS: 250, ViewCount: 90000, ThumbnailURL: "https://img/new1"},
	}
	f.prog.RunOnce(ctx)
	require.Equal(t, 1, f.model.calls)
	require.Equal(t, 1, f.search.calls)

	pending, err := f.reqs.Pending(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, request.SourceAI, pending[0].Source)
	require.Equal(t, "new1", pending[0].YTID)
	require.Equal(t, request.StatusApproved, pending[0].Status) // not cached → needs ingest
	require.Equal(t, "https://img/new1", pending[0].ThumbnailURL)

	// spend recorded, queue snapshot published
	lines, err := f.ledger.All(ctx)
	require.NoError(t, err)
	require.Len(t, lines, 1)
	require.Equal(t, "programmer:pick", lines[0].Label)
	require.Positive(t, lines[0].CostUSD)
	require.NotEmpty(t, f.prod.frames)
}

func TestLibraryPickIsReadyImmediately(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, &scriptedModel{raw: `{"picks":[{"yt_id":"lib2","reason":"đổi gió"}]}`})
	f.prog.RunOnce(ctx)
	pending, err := f.reqs.Pending(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "lib2", pending[0].YTID)
	require.Equal(t, request.StatusReady, pending[0].Status)
	require.Equal(t, "t-lib2", pending[0].Title)
}

func TestPickFiltersRecentAndQueuedAndUnknown(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, &scriptedModel{raw: `{"picks":[{"yt_id":"lib1"},{"yt_id":"lib2"}]}`})
	// lib1 recently aired; lib2 already queued → both filtered, nothing enqueued
	require.NoError(t, f.log.Append(ctx, live.Entry{YTID: "lib1", Title: "t", Artist: "a",
		StartedAt: time.Now().Add(-10 * time.Minute), DurationS: 240}))
	_, err := f.reqs.Create(ctx, request.Item{Source: request.SourceListener, RequestedBy: "u",
		YTID: "lib2", Title: "t", Channel: "c", DurationS: 100, Status: request.StatusReady})
	require.NoError(t, err)

	before, err := f.reqs.Pending(ctx)
	require.NoError(t, err)
	f.prog.RunOnce(ctx)
	after, err := f.reqs.Pending(ctx)
	require.NoError(t, err)
	require.Equal(t, len(before), len(after)) // ledger charged, nothing enqueued
	require.Equal(t, 1, f.model.calls)
}

func TestModelErrorAndParseFailureSkipCleanly(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, &scriptedModel{raw: "", err: context.DeadlineExceeded})
	f.prog.RunOnce(ctx)
	pending, _ := f.reqs.Pending(ctx)
	require.Empty(t, pending)
	lines, _ := f.ledger.All(ctx)
	require.Empty(t, lines) // failed call: no usage returned worth pricing

	f2 := newFixture(t, &scriptedModel{raw: "definitely not json"})
	f2.prog.RunOnce(ctx)
	pending, _ = f2.reqs.Pending(ctx)
	require.Empty(t, pending)
	lines, _ = f2.ledger.All(ctx)
	require.Len(t, lines, 1) // tokens were spent — the ledger records them even on parse failure
}

func TestFakeModeReSpinsLibraryWithoutModel(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, &scriptedModel{})
	f.prog.d.Fake = true
	f.prog.RunOnce(ctx)
	require.Zero(t, f.model.calls)
	pending, err := f.reqs.Pending(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, request.SourceAI, pending[0].Source)
	require.Equal(t, request.StatusReady, pending[0].Status)
	require.True(t, strings.HasPrefix(pending[0].YTID, "lib"))
	lines, _ := f.ledger.All(ctx)
	require.Empty(t, lines) // fake mode spends nothing
}

func TestBriefContainsPlaysRequestsAndSample(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, &scriptedModel{raw: `{"picks":[{"yt_id":"lib3"}]}`})
	require.NoError(t, f.log.Append(ctx, live.Entry{YTID: "lib1", Title: "Đã Phát", Artist: "Ca Sĩ",
		StartedAt: time.Now().Add(-30 * time.Minute), DurationS: 240}))
	_, err := f.reqs.Create(ctx, request.Item{Source: request.SourceListener, RequestedBy: "u",
		DisplayName: "Ngọc", YTID: "reqx", Title: "Bài Yêu Cầu", Channel: "c", DurationS: 100,
		Status: request.StatusApproved})
	require.NoError(t, err)

	brief, err := f.prog.buildBrief(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, brief.LocalTime)
	require.Contains(t, brief.RecentPlays, "Đã Phát — Ca Sĩ")
	require.Contains(t, brief.RecentRequests, "Bài Yêu Cầu")
	require.NotEmpty(t, brief.LibrarySample)
	require.LessOrEqual(t, len(brief.LibrarySample), briefSample)
}

func TestPicksStoreCappedReason(t *testing.T) {
	ctx := context.Background()

	t.Run("library pick keeps its reason", func(t *testing.T) {
		f := newFixture(t, &scriptedModel{raw: `{"picks":[{"yt_id":"lib2","reason":"  đổi gió một chút  "}]}`})
		f.prog.RunOnce(ctx)
		pending, err := f.reqs.Pending(ctx)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		require.Equal(t, "đổi gió một chút", pending[0].Reason) // trimmed
	})

	t.Run("query pick keeps its reason", func(t *testing.T) {
		f := newFixture(t, &scriptedModel{raw: `{"picks":[{"query":"nhạc đêm","reason":"khuya rồi"}]}`})
		f.search.byQuery["nhạc đêm"] = []ingest.Candidate{
			{YTID: "new1", Title: "Bài Mới", Channel: "Ca Sĩ - Topic", DurationS: 250, ViewCount: 90000},
		}
		f.prog.RunOnce(ctx)
		pending, err := f.reqs.Pending(ctx)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		require.Equal(t, "khuya rồi", pending[0].Reason)
	})

	t.Run("over-long reason is capped at 200 runes", func(t *testing.T) {
		long := strings.Repeat("đ", 250)
		f := newFixture(t, &scriptedModel{raw: `{"picks":[{"yt_id":"lib2","reason":"` + long + `"}]}`})
		f.prog.RunOnce(ctx)
		pending, err := f.reqs.Pending(ctx)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		require.Equal(t, strings.Repeat("đ", 200), pending[0].Reason)
	})

	t.Run("fake mode stores no reason", func(t *testing.T) {
		f := newFixture(t, &scriptedModel{})
		f.prog.d.Fake = true
		f.prog.RunOnce(ctx)
		pending, err := f.reqs.Pending(ctx)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		require.Empty(t, pending[0].Reason)
	})
}
