package radioserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	radiov1 "github.com/the-algovn/protos/gen/go/algovn/radio/v1"
	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/station"
)

type fakeLedger struct{ spent float64 }

func (f *fakeLedger) SpentSince(context.Context, time.Time) (float64, error) { return f.spent, nil }

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
