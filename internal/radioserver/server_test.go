package radioserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	radiov1 "github.com/the-algovn/protos/gen/go/algovn/radio/v1"
	ttsv1 "github.com/the-algovn/protos/gen/go/algovn/tts/v1"
	"github.com/the-algovn/radio-service/internal/broadcast"
	"github.com/the-algovn/radio-service/internal/director"
	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/schedule"
	"github.com/the-algovn/radio-service/internal/showlog"
	"github.com/the-algovn/radio-service/internal/station"
)

type fakeLedger struct{ spent float64 }

func (f *fakeLedger) SpentSince(context.Context, time.Time) (float64, error) { return f.spent, nil }

// fakeTTS is a minimal ttsv1.TTSServiceClient double standing in for the
// shared tts-service's catalog in voiceKnown's tests -- the real catalog
// lives in a different service, out of reach in a unit test.
type fakeTTS struct{}

func (fakeTTS) Synthesize(context.Context, *ttsv1.SynthesizeRequest, ...grpc.CallOption) (*ttsv1.SynthesizeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not used by these tests")
}

func (fakeTTS) ListVoices(context.Context, *ttsv1.ListVoicesRequest, ...grpc.CallOption) (*ttsv1.ListVoicesResponse, error) {
	return &ttsv1.ListVoicesResponse{Voices: []*ttsv1.Voice{
		{Id: "google:vi-VN-Neural2-A", Label: "Neural2 A", Tier: "neural2"},
		{Id: "google:vi-VN-Chirp3-HD-Aoede", Label: "Chirp3 HD Aoede", Tier: "chirp3-hd"},
	}}, nil
}

// erroringTTS simulates the shared tts-service being unreachable, for tests
// proving UpdateDJSettings doesn't hard-depend on a healthy catalog check
// when voice_id isn't actually changing.
type erroringTTS struct{}

func (erroringTTS) Synthesize(context.Context, *ttsv1.SynthesizeRequest, ...grpc.CallOption) (*ttsv1.SynthesizeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not used by these tests")
}

func (erroringTTS) ListVoices(context.Context, *ttsv1.ListVoicesRequest, ...grpc.CallOption) (*ttsv1.ListVoicesResponse, error) {
	return nil, status.Error(codes.Unavailable, "tts-service down")
}

func newTestServerWithTTS(t *testing.T, tts ttsv1.TTSServiceClient) *Server {
	t.Helper()
	return New(Deps{
		Store: station.NewMemStore(), Log: live.NewMemAirLog(), Search: &fakeSearch{},
		Requests: request.NewMemStore(), Library: library.NewMemLibrary(), Location: time.FixedZone("ICT", 7*3600),
		Listeners: live.NewMemListeners(time.Now),
		Now:       time.Now, Skipper: &fakeSkipper{}, Ledger: &fakeLedger{spent: 0.25}, BudgetUSD: 1.0,
		TTS: tts,
	})
}

func newTestServer(t *testing.T, ytIDs ...string) *Server {
	t.Helper()
	lib := library.NewMemLibrary()
	for _, id := range ytIDs {
		require.NoError(t, lib.Add(context.Background(), library.Track{
			YTID: id, Title: "t-" + id, Channel: "c-" + id, DurationS: 60, ArtifactID: "a-" + id,
		}))
	}
	return New(Deps{
		Store: station.NewMemStore(), Log: live.NewMemAirLog(), Search: &fakeSearch{},
		Requests: request.NewMemStore(), Library: lib, Location: time.FixedZone("ICT", 7*3600),
		Listeners: live.NewMemListeners(time.Now),
		Now:       time.Now, Skipper: &fakeSkipper{}, Ledger: &fakeLedger{spent: 0.25}, BudgetUSD: 1.0,
		TTS: fakeTTS{},
	})
}

// TestPlaylistRPCsAreGone: the 9 deleted playlist methods now fall through to
// the embedded UnimplementedRadioServiceServer and answer Unimplemented.
func TestPlaylistRPCsAreGone(t *testing.T) {
	s := newTestServer(t)
	_, err := s.CreatePlaylist(context.Background(), &radiov1.CreatePlaylistRequest{Name: "x"})
	require.Equal(t, codes.Unimplemented, status.Code(err))
}

// TestStationGuards: the station on-air lifecycle (playlists gone in v1.2).
// The empty-library → FailedPrecondition / with-tracks → success guard lives
// in TestGoOnAirNeedsNonEmptyLibrary; this pins the OnAir/OnAirSince state.
func TestStationGuards(t *testing.T) {
	s := newTestServer(t, "a", "b")
	ctx := context.Background()

	st, err := s.GetStation(ctx, &radiov1.GetStationRequest{})
	require.NoError(t, err)
	require.False(t, st.GetStation().GetOnAir())
	require.Empty(t, st.GetStation().GetOnAirSince())

	on, err := s.GoOnAir(ctx, &radiov1.GoOnAirRequest{})
	require.NoError(t, err)
	require.True(t, on.GetStation().GetOnAir())
	require.NotEmpty(t, on.GetStation().GetOnAirSince())

	off, err := s.GoOffAir(ctx, &radiov1.GoOffAirRequest{})
	require.NoError(t, err)
	require.False(t, off.GetStation().GetOnAir())
	require.Empty(t, off.GetStation().GetOnAirSince())
}

func newLiveTestServer(t *testing.T, ytIDs ...string) (*Server, station.Store, *live.MemAirLog, *live.MemListeners) {
	t.Helper()
	lib := library.NewMemLibrary()
	for _, id := range ytIDs {
		require.NoError(t, lib.Add(context.Background(), library.Track{
			YTID: id, Title: "t-" + id, Channel: "c-" + id, DurationS: 60, ArtifactID: "a-" + id,
		}))
	}
	st := station.NewMemStore()
	log := live.NewMemAirLog()
	ls := live.NewMemListeners(time.Now)
	return New(Deps{Store: st, Log: log, Listeners: ls, Library: lib}), st, log, ls
}

func TestGetNowPlayingOffAirIsEmpty(t *testing.T) {
	s, _, _, _ := newLiveTestServer(t)
	resp, err := s.GetNowPlaying(context.Background(), &radiov1.GetNowPlayingRequest{})
	require.NoError(t, err)
	require.Nil(t, resp.GetNowPlaying()) // ABSENT ⇔ off-air
}

func TestGetNowPlayingOnAir(t *testing.T) {
	s, st, log, ls := newLiveTestServer(t, "a")
	ctx := context.Background()
	_, err := s.GoOnAir(ctx, &radiov1.GoOnAirRequest{})
	require.NoError(t, err)
	started := time.Now().Add(-10 * time.Second).Truncate(time.Second)
	require.NoError(t, log.Append(ctx, live.Entry{YTID: "a", Title: "t-a", Artist: "c-a", StartedAt: started, DurationS: 60}))
	require.NoError(t, ls.Beat(ctx, "tab-1"))

	resp, err := s.GetNowPlaying(ctx, &radiov1.GetNowPlayingRequest{})
	require.NoError(t, err)
	np := resp.GetNowPlaying()
	require.NotNil(t, np)
	require.Equal(t, "track", np.GetKind())
	require.Equal(t, "t-a", np.GetTitle())
	require.Equal(t, started.UTC().Format(time.RFC3339Nano), np.GetStartedAt())
	require.Equal(t, int32(60), np.GetDurationSeconds())
	require.Equal(t, int32(1), np.GetListeners())
	_ = st
}

func TestGetHistoryAndHeartbeat(t *testing.T) {
	s, _, log, ls := newLiveTestServer(t)
	ctx := context.Background()
	old := time.Now().Add(-10 * time.Minute)
	require.NoError(t, log.Append(ctx, live.Entry{YTID: "x", Title: "t-x", Artist: "c-x", StartedAt: old, DurationS: 60}))

	h, err := s.GetHistory(ctx, &radiov1.GetHistoryRequest{})
	require.NoError(t, err)
	require.Len(t, h.GetItems(), 1)
	require.Equal(t, "t-x", h.GetItems()[0].GetTitle())
	require.Equal(t, old.UTC().Format(time.RFC3339Nano), h.GetItems()[0].GetAiredAt())

	_, err = s.Heartbeat(ctx, &radiov1.HeartbeatRequest{SessionId: "tab-9"})
	require.NoError(t, err)
	n, err := ls.Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	_, err = s.Heartbeat(ctx, &radiov1.HeartbeatRequest{})
	require.Equal(t, codes.InvalidArgument, status.Code(err)) // blank session_id
	_, err = s.Heartbeat(ctx, &radiov1.HeartbeatRequest{SessionId: strings.Repeat("x", 101)})
	require.Equal(t, codes.InvalidArgument, status.Code(err)) // oversized
}

func TestNowPlayingAndHistoryCarryProvenance(t *testing.T) {
	s, _, _, _ := newLiveTestServer(t, "a") // Listeners must be set: on-air GetNowPlaying calls it
	ctx := context.Background()
	_, err := s.GoOnAir(ctx, &radiov1.GoOnAirRequest{})
	require.NoError(t, err)

	// currently airing: a listener request
	require.NoError(t, s.deps.Log.Append(ctx, live.Entry{
		YTID: "a", Title: "t-a", Artist: "c-a",
		StartedAt: time.Now().Add(-10 * time.Second), DurationS: 240,
		Source: "listener", RequestedByName: "Ngọc",
	}))
	np, err := s.GetNowPlaying(ctx, &radiov1.GetNowPlayingRequest{})
	require.NoError(t, err)
	require.Equal(t, "listener", np.GetNowPlaying().GetSource())
	require.Equal(t, "Ngọc", np.GetNowPlaying().GetRequestedByName())
	require.Empty(t, np.GetNowPlaying().GetReason())

	// finished earlier: an AI pick → history carries source + reason
	require.NoError(t, s.deps.Log.Append(ctx, live.Entry{
		YTID: "b", Title: "t-b", Artist: "c-b",
		StartedAt: time.Now().Add(-2 * time.Hour), DurationS: 60,
		Source: "ai", Reason: "hợp đêm mưa",
	}))
	h, err := s.GetHistory(ctx, &radiov1.GetHistoryRequest{})
	require.NoError(t, err)
	var hit *radiov1.HistoryItem
	for _, it := range h.GetItems() {
		if it.GetTitle() == "t-b" {
			hit = it
		}
	}
	require.NotNil(t, hit)
	require.Equal(t, "ai", hit.GetSource())
	require.Equal(t, "hợp đêm mưa", hit.GetReason())
	require.Empty(t, hit.GetRequestedByName())
}

func TestGoOnAirPokesNotifier(t *testing.T) {
	lib := library.NewMemLibrary()
	require.NoError(t, lib.Add(context.Background(), library.Track{YTID: "a", Title: "t", Channel: "c", DurationS: 60, ArtifactID: "x"}))
	pokes := 0
	s := New(Deps{Store: station.NewMemStore(), Library: lib, Notifier: notifierFunc(func() { pokes++ })})
	ctx := context.Background()
	_, err := s.GoOnAir(ctx, &radiov1.GoOnAirRequest{})
	require.NoError(t, err)
	_, err = s.GoOffAir(ctx, &radiov1.GoOffAirRequest{})
	require.NoError(t, err)
	require.Equal(t, 2, pokes)
}

type notifierFunc func()

func (f notifierFunc) Poke() { f() }

// -- GetShowTimeline test doubles --

type fakeShowLog struct {
	segs  []showlog.Segment
	count int64
}

func (f *fakeShowLog) Recent(_ context.Context, limit, offset int) ([]showlog.Segment, error) {
	// Mirror the store's clamping: limit<=0 → 50, capped at 200.
	if limit <= 0 {
		limit = 50
	} else if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(f.segs) {
		return nil, nil
	}
	end := offset + limit
	if end > len(f.segs) {
		end = len(f.segs)
	}
	return f.segs[offset:end], nil
}

func (f *fakeShowLog) Count(_ context.Context) (int64, error) {
	if f.count > 0 {
		return f.count, nil
	}
	return int64(len(f.segs)), nil
}

type fakeSessions struct {
	sessions []broadcast.Session
}

func (f *fakeSessions) Overlapping(_ context.Context, _, _ time.Time, _ int) ([]broadcast.Session, error) {
	return f.sessions, nil
}

// fakeBreaks implements Breaks with a canned Snapshot, for director-present
// tests that don't need the full director.
type fakeBreaks struct {
	snap director.Snapshot
}

func (f *fakeBreaks) Snapshot() director.Snapshot { return f.snap }

func newTimelineTestServer(t *testing.T, deps Deps) *Server {
	t.Helper()
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Location == nil {
		deps.Location = time.UTC
	}
	return New(deps)
}

// -- GetShowTimeline tests --

func TestGetShowTimelineOffAir(t *testing.T) {
	s := newTimelineTestServer(t, Deps{
		Store:     station.NewMemStore(),
		ShowLog:   &fakeShowLog{},
		Requests:  request.NewMemStore(),
		Sched:     schedule.NewMemStore(),
		Library:   library.NewMemLibrary(),
		Listeners: live.NewMemListeners(time.Now),
		Ledger:    &fakeLedger{},
	})
	ctx := context.Background()

	resp, err := s.GetShowTimeline(ctx, &radiov1.GetShowTimelineRequest{})
	require.NoError(t, err)
	require.Empty(t, resp.GetUpcoming())
	require.Equal(t, "off_air", resp.GetBreakGate())
}

func TestGetShowTimelineNilBreaksYieldsDjDisabled(t *testing.T) {
	st := station.NewMemStore()
	_, err := st.GoOnAir(context.Background())
	require.NoError(t, err)

	s := newTimelineTestServer(t, Deps{
		Store:     st,
		ShowLog:   &fakeShowLog{},
		Requests:  request.NewMemStore(),
		Sched:     schedule.NewMemStore(),
		Library:   library.NewMemLibrary(),
		Listeners: live.NewMemListeners(time.Now),
		Ledger:    &fakeLedger{},
	})
	// Breaks is deliberately nil — RADIO_DJ_ENABLED unset.
	ctx := context.Background()
	now := time.Now()

	resp, err := s.GetShowTimeline(ctx, &radiov1.GetShowTimelineRequest{})
	require.NoError(t, err)
	require.Equal(t, "dj_disabled", resp.GetBreakGate())
	// Verify no break segments in upcoming — the DJ can't produce any.
	for _, seg := range resp.GetUpcoming() {
		require.NotEqual(t, "dj", seg.GetKind())
		require.NotEqual(t, "station_id", seg.GetKind())
	}
	require.NotEmpty(t, resp.GetServerNow())
	_ = now
}

func TestGetShowTimelinePastExcludesAiringAndTotalPast(t *testing.T) {
	// An hour past the store's on-air anchor, so every row below belongs to
	// the current session — a row predating OnAirSince is not airing.
	now := time.Now().Add(time.Hour)
	// One airing track (started 30s ago, 180s long — still playing).
	// Three finished tracks.
	airingSeg := showlog.Segment{
		ID: 1, Origin: showlog.OriginAir, Kind: "track",
		YTID: "airing", Title: "Airing", Artist: "Artist A",
		StartedAt: now.Add(-30 * time.Second), DurationS: 180,
		Source: "listener", RequestedByName: "Name",
	}
	segs := []showlog.Segment{
		airingSeg,
		{ID: 2, Origin: showlog.OriginAir, Kind: "track", YTID: "y2", Title: "Past 1", StartedAt: now.Add(-5 * time.Minute), DurationS: 200, Source: "ai", Reason: "reason 1"},
		{ID: 3, Origin: showlog.OriginAir, Kind: "track", YTID: "y3", Title: "Past 2", StartedAt: now.Add(-10 * time.Minute), DurationS: 180},
		{ID: 4, Origin: showlog.OriginAir, Kind: "track", YTID: "y4", Title: "Past 3", StartedAt: now.Add(-15 * time.Minute), DurationS: 240},
	}
	lib := library.NewMemLibrary()
	for _, sg := range segs {
		require.NoError(t, lib.Add(context.Background(), library.Track{
			YTID: sg.YTID, Title: sg.Title, Channel: sg.Artist,
			DurationS: float64(sg.DurationS), ArtifactID: "a-" + sg.YTID,
		}))
	}

	st := station.NewMemStore()
	_, err := st.GoOnAir(context.Background())
	require.NoError(t, err)

	s := newTimelineTestServer(t, Deps{
		Store:     st,
		ShowLog:   &fakeShowLog{segs: segs, count: int64(len(segs))},
		Requests:  request.NewMemStore(),
		Sched:     schedule.NewMemStore(),
		Library:   lib,
		Listeners: live.NewMemListeners(time.Now),
		Ledger:    &fakeLedger{},
		Now:       func() time.Time { return now },
	})
	ctx := context.Background()

	resp, err := s.GetShowTimeline(ctx, &radiov1.GetShowTimelineRequest{})
	require.NoError(t, err)

	// Airing is present and is the airing track.
	require.NotNil(t, resp.GetAiring())
	require.Equal(t, "Airing", resp.GetAiring().GetTitle())
	require.Equal(t, "airing", resp.GetAiring().GetCertainty())

	// Past does NOT include the airing row.
	require.Len(t, resp.GetPast(), 3)
	for _, p := range resp.GetPast() {
		require.NotEqual(t, "Airing", p.GetTitle())
	}
	// total_past excludes the airing row.
	require.Equal(t, int64(3), resp.GetTotalPast())
}

func TestGetShowTimelinePagingBoundsAreStores(t *testing.T) {
	// Build more than the store default (50). The handler must NOT re-clamp.
	now := time.Now()
	segs := make([]showlog.Segment, 100)
	for i := range segs {
		segs[i] = showlog.Segment{
			ID: int64(i + 1), Origin: showlog.OriginAir, Kind: "track",
			YTID: "y", Title: "Past", StartedAt: now.Add(-time.Duration(i+5) * time.Minute),
			DurationS: 180,
		}
	}
	lib := library.NewMemLibrary()
	_ = lib.Add(context.Background(), library.Track{YTID: "y", Title: "Past", Channel: "c", DurationS: 180, ArtifactID: "a"})

	st := station.NewMemStore()
	_, err := st.GoOnAir(context.Background())
	require.NoError(t, err)

	s := newTimelineTestServer(t, Deps{
		Store:     st,
		ShowLog:   &fakeShowLog{segs: segs, count: int64(len(segs))},
		Requests:  request.NewMemStore(),
		Sched:     schedule.NewMemStore(),
		Library:   lib,
		Listeners: live.NewMemListeners(time.Now),
		Ledger:    &fakeLedger{},
	})
	ctx := context.Background()

	// limit=0, offset=0 → the store default (50), NOT 20.
	resp, err := s.GetShowTimeline(ctx, &radiov1.GetShowTimelineRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetPast(), 50)
	require.Equal(t, int64(100), resp.GetTotalPast())

	// limit=5, offset=0 → use caller's limit (store clamps at its layer).
	resp2, err := s.GetShowTimeline(ctx, &radiov1.GetShowTimelineRequest{Limit: 5})
	require.NoError(t, err)
	require.Len(t, resp2.GetPast(), 5)
}

func TestGetShowTimelineTalkRowMapsScriptProvenance(t *testing.T) {
	now := time.Now()
	// A music row (finished) and a talk row (finished).
	talkSeg := showlog.Segment{
		ID: 1, Origin: showlog.OriginTalk, Kind: showlog.KindSeam,
		Title: "Talk break", StartedAt: now.Add(-2 * time.Minute), DurationS: 40,
		Script: "Chào các bạn", BacksellTitle: "Track A", PromiseTitle: "Track B",
		CorrelationID: "corr-1", Model: "claude-haiku",
		InTokens: 500, OutTokens: 80, CostUSD: 0.03, LatencyMS: 1200,
	}
	musicSeg := showlog.Segment{
		ID: 2, Origin: showlog.OriginAir, Kind: "track",
		YTID: "m1", Title: "Music Track", Artist: "Artist M",
		StartedAt: now.Add(-5 * time.Minute), DurationS: 200,
		Source: "ai", Reason: "vibe match",
	}
	segs := []showlog.Segment{talkSeg, musicSeg}
	lib := library.NewMemLibrary()
	require.NoError(t, lib.Add(context.Background(), library.Track{
		YTID: "m1", Title: "Music Track", Channel: "Artist M",
		DurationS: 200, ArtifactID: "a-m1",
	}))

	st := station.NewMemStore()
	_, err := st.GoOnAir(context.Background())
	require.NoError(t, err)

	s := newTimelineTestServer(t, Deps{
		Store:     st,
		ShowLog:   &fakeShowLog{segs: segs},
		Requests:  request.NewMemStore(),
		Sched:     schedule.NewMemStore(),
		Library:   lib,
		Listeners: live.NewMemListeners(time.Now),
		Ledger:    &fakeLedger{},
	})
	ctx := context.Background()

	resp, err := s.GetShowTimeline(ctx, &radiov1.GetShowTimelineRequest{})
	require.NoError(t, err)

	// Two past items: talk first (newest), then music.
	require.Len(t, resp.GetPast(), 2)

	// Talk row (newest, first in past): carries script and LLM provenance.
	talk := resp.GetPast()[0]
	require.Equal(t, "dj", talk.GetKind()) // stored "seam" → wire "dj"
	require.Equal(t, "aired", talk.GetCertainty())
	require.Equal(t, "Chào các bạn", talk.GetScript())
	require.Equal(t, "Track A", talk.GetBacksellTitle())
	require.Equal(t, "Track B", talk.GetPromiseTitle())
	require.Equal(t, "corr-1", talk.GetCorrelationId())
	require.Equal(t, "claude-haiku", talk.GetModel())
	require.Equal(t, int32(500), talk.GetInTokens())
	require.Equal(t, int32(80), talk.GetOutTokens())
	require.Equal(t, 0.03, talk.GetCostUsd())
	require.Equal(t, int32(1200), talk.GetLatencyMs())

	// Music row: no script, no LLM provenance.
	music := resp.GetPast()[1]
	require.Equal(t, "track", music.GetKind())
	require.Equal(t, "Music Track", music.GetTitle())
	require.Equal(t, "Artist M", music.GetArtist())
	require.Equal(t, "ai", music.GetSource())
	require.Equal(t, "vibe match", music.GetReason())
	require.Empty(t, music.GetScript())
	require.Empty(t, music.GetModel())
	require.Empty(t, music.GetCorrelationId())
}

func TestGetShowTimelineTotalPastStableAcrossPages(t *testing.T) {
	// An hour past the store's on-air anchor — see the note above.
	now := time.Now().Add(time.Hour)
	// 5 segments: [airing (newest), s1, s2, s3, s4].
	// limit=2, so page 0 gets [airing, s1] and page 1 gets [s2, s3].
	// total_past must be 4 on both pages, not 4 then 5.
	airingSeg := showlog.Segment{
		ID: 1, Origin: showlog.OriginAir, Kind: "track",
		YTID: "airing", Title: "Airing", Artist: "A",
		StartedAt: now.Add(-30 * time.Second), DurationS: 180,
	}
	segs := []showlog.Segment{
		airingSeg,
		{ID: 2, Origin: showlog.OriginAir, Kind: "track", YTID: "y2", Title: "Past 1", StartedAt: now.Add(-5 * time.Minute), DurationS: 200},
		{ID: 3, Origin: showlog.OriginAir, Kind: "track", YTID: "y3", Title: "Past 2", StartedAt: now.Add(-10 * time.Minute), DurationS: 180},
		{ID: 4, Origin: showlog.OriginAir, Kind: "track", YTID: "y4", Title: "Past 3", StartedAt: now.Add(-15 * time.Minute), DurationS: 240},
		{ID: 5, Origin: showlog.OriginAir, Kind: "track", YTID: "y5", Title: "Past 4", StartedAt: now.Add(-20 * time.Minute), DurationS: 180},
	}
	lib := library.NewMemLibrary()
	for _, sg := range segs {
		require.NoError(t, lib.Add(context.Background(), library.Track{
			YTID: sg.YTID, Title: sg.Title, Channel: sg.Artist,
			DurationS: float64(sg.DurationS), ArtifactID: "a-" + sg.YTID,
		}))
	}

	st := station.NewMemStore()
	_, err := st.GoOnAir(context.Background())
	require.NoError(t, err)

	s := newTimelineTestServer(t, Deps{
		Store:     st,
		ShowLog:   &fakeShowLog{segs: segs, count: int64(len(segs))},
		Requests:  request.NewMemStore(),
		Sched:     schedule.NewMemStore(),
		Library:   lib,
		Listeners: live.NewMemListeners(time.Now),
		Ledger:    &fakeLedger{},
		Now:       func() time.Time { return now },
	})
	ctx := context.Background()

	// page 0: offset=0, limit=2 → returns [airing, Past 1].
	// airing excluded from past, so past = [Past 1], total_past = 4.
	resp0, err := s.GetShowTimeline(ctx, &radiov1.GetShowTimelineRequest{Limit: 2, Offset: 0})
	require.NoError(t, err)
	require.NotNil(t, resp0.GetAiring())
	require.Equal(t, "Airing", resp0.GetAiring().GetTitle())
	require.Len(t, resp0.GetPast(), 1)
	require.Equal(t, "Past 1", resp0.GetPast()[0].GetTitle())
	require.Equal(t, int64(4), resp0.GetTotalPast())

	// page 1: offset=2, limit=2 → returns [Past 2, Past 3].
	// airing not in page, but still detected; total_past still 4.
	resp1, err := s.GetShowTimeline(ctx, &radiov1.GetShowTimelineRequest{Limit: 2, Offset: 2})
	require.NoError(t, err)
	require.NotNil(t, resp1.GetAiring()) // present because detected independently
	require.Len(t, resp1.GetPast(), 2)
	require.Equal(t, "Past 2", resp1.GetPast()[0].GetTitle())
	require.Equal(t, "Past 3", resp1.GetPast()[1].GetTitle())
	require.Equal(t, int64(4), resp1.GetTotalPast()) // stable across pages
}

// A track cut short keeps its NOMINAL duration in air_log forever — nothing
// ever amends the row — so arithmetic alone reports it airing long after the
// audio stopped. Airing detection must consult the current session anchor.
func TestGetShowTimelineStaleRowIsNotAiring(t *testing.T) {
	ctx := context.Background()
	// build serves exactly one show-log row, with the handler clock pinned.
	build := func(t *testing.T, seg showlog.Segment, now time.Time, onAir bool) *Server {
		t.Helper()
		lib := library.NewMemLibrary()
		require.NoError(t, lib.Add(ctx, library.Track{
			YTID: seg.YTID, Title: seg.Title, DurationS: float64(seg.DurationS), ArtifactID: "a",
		}))
		st := station.NewMemStore()
		if onAir {
			_, err := st.GoOnAir(ctx)
			require.NoError(t, err)
		}
		return newTimelineTestServer(t, Deps{
			Store:     st,
			ShowLog:   &fakeShowLog{segs: []showlog.Segment{seg}, count: 1},
			Requests:  request.NewMemStore(),
			Sched:     schedule.NewMemStore(),
			Library:   lib,
			Listeners: live.NewMemListeners(time.Now),
			Ledger:    &fakeLedger{},
			Now:       func() time.Time { return now },
		})
	}
	// A row still nominally running an hour from now. GoOnAir, when the case
	// asks for it, anchors the session AFTER this row started.
	cutShort := func(t0 time.Time) showlog.Segment {
		return showlog.Segment{ID: 1, Origin: showlog.OriginAir, Kind: "track",
			YTID: "y1", Title: "Cut short", StartedAt: t0.Add(-5 * time.Minute), DurationS: 3600}
	}

	t.Run("off air", func(t *testing.T) {
		t0 := time.Now()
		s := build(t, cutShort(t0), t0, false)
		resp, err := s.GetShowTimeline(ctx, &radiov1.GetShowTimelineRequest{})
		require.NoError(t, err)
		require.Nil(t, resp.GetAiring(), "an off-air station is airing nothing")
		require.Len(t, resp.GetPast(), 1, "the cut track belongs in past")
		require.Equal(t, "Cut short", resp.GetPast()[0].GetTitle())
		require.Equal(t, int64(1), resp.GetTotalPast())
	})

	t.Run("on air after a toggle", func(t *testing.T) {
		t0 := time.Now()
		now := t0.Add(time.Minute)
		s := build(t, cutShort(t0), now, true) // GoOnAir anchors at ~t0
		resp, err := s.GetShowTimeline(ctx, &radiov1.GetShowTimelineRequest{})
		require.NoError(t, err)
		require.Nil(t, resp.GetAiring(), "the stale row is from the previous session")

		// The phantom end would push the whole walk ~55 minutes out.
		require.NotEmpty(t, resp.GetUpcoming())
		startedAt, err := time.Parse(time.RFC3339, resp.GetUpcoming()[0].GetStartedAt())
		require.NoError(t, err)
		require.WithinDuration(t, now, startedAt, 2*time.Second,
			"the next item starts now, not at the cut track's nominal end")
	})

	t.Run("genuinely airing", func(t *testing.T) {
		// Guard against over-correction: a row started inside the current
		// session and still running is airing.
		t0 := time.Now()
		seg := showlog.Segment{ID: 1, Origin: showlog.OriginAir, Kind: "track",
			YTID: "y1", Title: "Now playing", StartedAt: t0.Add(30 * time.Minute), DurationS: 3600}
		s := build(t, seg, t0.Add(time.Hour), true) // GoOnAir anchors at ~t0
		resp, err := s.GetShowTimeline(ctx, &radiov1.GetShowTimelineRequest{})
		require.NoError(t, err)
		require.NotNil(t, resp.GetAiring())
		require.Equal(t, "Now playing", resp.GetAiring().GetTitle())
		require.Empty(t, resp.GetPast())
		require.Equal(t, int64(0), resp.GetTotalPast())
	})
}

func TestGetShowTimelineBetweenItemsStillOnAir(t *testing.T) {
	now := time.Now()
	// On air but between items: newest row's end is already in the past.
	// airing must be absent and total_past == total on all offsets.
	segs := []showlog.Segment{
		{ID: 1, Origin: showlog.OriginAir, Kind: "track", YTID: "y1", Title: "Past 0", StartedAt: now.Add(-4 * time.Minute), DurationS: 180},
		{ID: 2, Origin: showlog.OriginAir, Kind: "track", YTID: "y2", Title: "Past 1", StartedAt: now.Add(-8 * time.Minute), DurationS: 200},
		{ID: 3, Origin: showlog.OriginAir, Kind: "track", YTID: "y3", Title: "Past 2", StartedAt: now.Add(-12 * time.Minute), DurationS: 180},
	}
	lib := library.NewMemLibrary()
	for _, sg := range segs {
		require.NoError(t, lib.Add(context.Background(), library.Track{
			YTID: sg.YTID, Title: sg.Title, Channel: sg.Artist,
			DurationS: float64(sg.DurationS), ArtifactID: "a-" + sg.YTID,
		}))
	}

	st := station.NewMemStore()
	_, err := st.GoOnAir(context.Background())
	require.NoError(t, err)

	s := newTimelineTestServer(t, Deps{
		Store:     st,
		ShowLog:   &fakeShowLog{segs: segs, count: int64(len(segs))},
		Requests:  request.NewMemStore(),
		Sched:     schedule.NewMemStore(),
		Library:   lib,
		Listeners: live.NewMemListeners(time.Now),
		Ledger:    &fakeLedger{},
	})
	ctx := context.Background()

	// offset=0: airing absent, total_past = total = 3.
	resp0, err := s.GetShowTimeline(ctx, &radiov1.GetShowTimelineRequest{Limit: 2, Offset: 0})
	require.NoError(t, err)
	require.Nil(t, resp0.GetAiring())
	require.Len(t, resp0.GetPast(), 2)
	require.Equal(t, int64(3), resp0.GetTotalPast())

	// offset=2: still no airing, still total_past = 3.
	resp1, err := s.GetShowTimeline(ctx, &radiov1.GetShowTimelineRequest{Limit: 2, Offset: 2})
	require.NoError(t, err)
	require.Nil(t, resp1.GetAiring())
	require.Len(t, resp1.GetPast(), 1)
	require.Equal(t, "Past 2", resp1.GetPast()[0].GetTitle())
	require.Equal(t, int64(3), resp1.GetTotalPast()) // stable across pages
}
