package radioserver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	radiov1 "github.com/the-algovn/protos/gen/go/algovn/radio/v1"
	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/playlist"
)

func newTestServer(t *testing.T, ytIDs ...string) *Server {
	t.Helper()
	lib := library.NewMemLibrary()
	for _, id := range ytIDs {
		require.NoError(t, lib.Add(context.Background(), library.Track{
			YTID: id, Title: "t-" + id, Channel: "c-" + id, DurationS: 60, ArtifactID: "a-" + id,
		}))
	}
	return New(Deps{Store: playlist.NewMemStore(lib)})
}

// mkPlaylist creates a playlist with the given tracks and returns its id.
func mkPlaylist(t *testing.T, s *Server, name string, ytIDs ...string) string {
	t.Helper()
	ctx := context.Background()
	cr, err := s.CreatePlaylist(ctx, &radiov1.CreatePlaylistRequest{Name: name})
	require.NoError(t, err)
	for _, id := range ytIDs {
		_, err = s.AddTrack(ctx, &radiov1.AddTrackRequest{PlaylistId: cr.GetSummary().GetId(), YtId: id})
		require.NoError(t, err)
	}
	return cr.GetSummary().GetId()
}

func TestCreatePlaylistValidation(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	for _, name := range []string{"", "   ", strings.Repeat("x", 201)} {
		_, err := s.CreatePlaylist(ctx, &radiov1.CreatePlaylistRequest{Name: name})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "name %q", name)
	}
	resp, err := s.CreatePlaylist(ctx, &radiov1.CreatePlaylistRequest{Name: "  đêm khuya  "})
	require.NoError(t, err)
	require.Equal(t, "đêm khuya", resp.GetSummary().GetName()) // trimmed
}

func TestPlaylistLifecycleAndProjection(t *testing.T) {
	s := newTestServer(t, "a", "b")
	ctx := context.Background()
	id := mkPlaylist(t, s, "mix", "a", "b")

	get, err := s.GetPlaylist(ctx, &radiov1.GetPlaylistRequest{Id: id})
	require.NoError(t, err)
	require.Equal(t, int32(2), get.GetPlaylist().GetSummary().GetTrackCount())
	require.Equal(t, int64(120), get.GetPlaylist().GetSummary().GetTotalDurationS())
	tracks := get.GetPlaylist().GetTracks()
	require.Equal(t, "a", tracks[0].GetYtId())
	require.Equal(t, "t-a", tracks[0].GetTitle())
	require.Equal(t, int64(60), tracks[0].GetDurationS())
	require.NotEmpty(t, get.GetPlaylist().GetSummary().GetCreatedAt()) // RFC3339

	list, err := s.ListPlaylists(ctx, &radiov1.ListPlaylistsRequest{})
	require.NoError(t, err)
	require.Len(t, list.GetPlaylists(), 1)
	require.Equal(t, "mix", list.GetPlaylists()[0].GetName())
	require.Equal(t, int32(2), list.GetPlaylists()[0].GetTrackCount())

	re, err := s.ReorderTracks(ctx, &radiov1.ReorderTracksRequest{PlaylistId: id, YtIds: []string{"b", "a"}})
	require.NoError(t, err)
	require.Equal(t, "b", re.GetPlaylist().GetTracks()[0].GetYtId())

	_, err = s.ReorderTracks(ctx, &radiov1.ReorderTracksRequest{PlaylistId: id, YtIds: []string{"b"}})
	require.Equal(t, codes.InvalidArgument, status.Code(err)) // ErrStale

	_, err = s.GetPlaylist(ctx, &radiov1.GetPlaylistRequest{Id: "missing"})
	require.Equal(t, codes.NotFound, status.Code(err))
	_, err = s.AddTrack(ctx, &radiov1.AddTrackRequest{PlaylistId: id, YtId: "nope"})
	require.Equal(t, codes.NotFound, status.Code(err))
	_, err = s.AddTrack(ctx, &radiov1.AddTrackRequest{PlaylistId: id})
	require.Equal(t, codes.InvalidArgument, status.Code(err)) // blank yt_id
}

func TestStationGuards(t *testing.T) {
	s := newTestServer(t, "a", "b")
	ctx := context.Background()

	st, err := s.GetStation(ctx, &radiov1.GetStationRequest{})
	require.NoError(t, err)
	require.False(t, st.GetStation().GetOnAir())
	require.Empty(t, st.GetStation().GetOnAirSince())

	// v1: on-air needs only a non-empty library (Task 11 adds the
	// empty-library FailedPrecondition; this fixture's library has tracks).
	on0, err := s.GoOnAir(ctx, &radiov1.GoOnAirRequest{})
	require.NoError(t, err)
	require.True(t, on0.GetStation().GetOnAir())
	_, err = s.GoOffAir(ctx, &radiov1.GoOffAirRequest{})
	require.NoError(t, err)

	id := mkPlaylist(t, s, "mix", "a")
	act, err := s.SetActivePlaylist(ctx, &radiov1.SetActivePlaylistRequest{PlaylistId: id})
	require.NoError(t, err)
	require.Equal(t, id, act.GetStation().GetActivePlaylistId())
	require.Equal(t, "mix", act.GetStation().GetActivePlaylistName())

	on, err := s.GoOnAir(ctx, &radiov1.GoOnAirRequest{})
	require.NoError(t, err)
	require.True(t, on.GetStation().GetOnAir())
	require.NotEmpty(t, on.GetStation().GetOnAirSince())

	// on-air guards → FailedPrecondition
	_, err = s.DeletePlaylist(ctx, &radiov1.DeletePlaylistRequest{Id: id})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	_, err = s.RemoveTrack(ctx, &radiov1.RemoveTrackRequest{PlaylistId: id, YtId: "a"})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	off, err := s.GoOffAir(ctx, &radiov1.GoOffAirRequest{})
	require.NoError(t, err)
	require.False(t, off.GetStation().GetOnAir())
	require.Empty(t, off.GetStation().GetOnAirSince())

	// empty playlist can't be activated
	empty := mkPlaylist(t, s, "empty")
	_, err = s.SetActivePlaylist(ctx, &radiov1.SetActivePlaylistRequest{PlaylistId: empty})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestRenamePlaylistGuards(t *testing.T) {
	s := newTestServer(t, "a")
	ctx := context.Background()
	id := mkPlaylist(t, s, "mix", "a")

	_, err := s.RenamePlaylist(ctx, &radiov1.RenamePlaylistRequest{Id: "", Name: "x"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = s.RenamePlaylist(ctx, &radiov1.RenamePlaylistRequest{Id: id, Name: "  "})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	resp, err := s.RenamePlaylist(ctx, &radiov1.RenamePlaylistRequest{Id: id, Name: "  new name  "})
	require.NoError(t, err)
	require.Equal(t, "new name", resp.GetSummary().GetName())
}

func newLiveTestServer(t *testing.T, ytIDs ...string) (*Server, playlist.Store, *live.MemAirLog, *live.MemListeners) {
	t.Helper()
	lib := library.NewMemLibrary()
	for _, id := range ytIDs {
		require.NoError(t, lib.Add(context.Background(), library.Track{
			YTID: id, Title: "t-" + id, Channel: "c-" + id, DurationS: 60, ArtifactID: "a-" + id,
		}))
	}
	st := playlist.NewMemStore(lib)
	log := live.NewMemAirLog()
	ls := live.NewMemListeners(time.Now)
	return New(Deps{Store: st, Log: log, Listeners: ls}), st, log, ls
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
	id := mkPlaylist(t, s, "mix", "a")
	_, err := s.SetActivePlaylist(ctx, &radiov1.SetActivePlaylistRequest{PlaylistId: id})
	require.NoError(t, err)
	_, err = s.GoOnAir(ctx, &radiov1.GoOnAirRequest{})
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

func TestGetQueueRotation(t *testing.T) {
	s, _, log, _ := newLiveTestServer(t, "a", "b", "c")
	ctx := context.Background()
	id := mkPlaylist(t, s, "mix", "a", "b", "c")
	_, err := s.SetActivePlaylist(ctx, &radiov1.SetActivePlaylistRequest{PlaylistId: id})
	require.NoError(t, err)
	_, err = s.GoOnAir(ctx, &radiov1.GoOnAirRequest{})
	require.NoError(t, err)
	require.NoError(t, log.Append(ctx, live.Entry{YTID: "b", Title: "t-b", Artist: "c-b", StartedAt: time.Now(), DurationS: 60}))

	resp, err := s.GetQueue(ctx, &radiov1.GetQueueRequest{})
	require.NoError(t, err)
	items := resp.GetItems()
	require.Len(t, items, 2)
	require.Equal(t, "t-c", items[0].GetTitle()) // after current b, wrapping
	require.Equal(t, "t-a", items[1].GetTitle())
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

func TestGoOnAirPokesNotifier(t *testing.T) {
	lib := library.NewMemLibrary()
	require.NoError(t, lib.Add(context.Background(), library.Track{YTID: "a", Title: "t", Channel: "c", DurationS: 60, ArtifactID: "x"}))
	pokes := 0
	s := New(Deps{Store: playlist.NewMemStore(lib), Notifier: notifierFunc(func() { pokes++ })})
	id := mkPlaylist(t, s, "mix", "a")
	ctx := context.Background()
	_, err := s.SetActivePlaylist(ctx, &radiov1.SetActivePlaylistRequest{PlaylistId: id})
	require.NoError(t, err)
	_, err = s.GoOnAir(ctx, &radiov1.GoOnAirRequest{})
	require.NoError(t, err)
	_, err = s.GoOffAir(ctx, &radiov1.GoOffAirRequest{})
	require.NoError(t, err)
	require.Equal(t, 2, pokes)
}

type notifierFunc func()

func (f notifierFunc) Poke() { f() }

// failingAirLog wraps an AirLog and makes Latest return an error.
type failingAirLog struct {
	log live.AirLog
}

func (f *failingAirLog) Append(ctx context.Context, e live.Entry) error {
	return f.log.Append(ctx, e)
}

func (f *failingAirLog) Latest(ctx context.Context) (live.Entry, bool, error) {
	return live.Entry{}, false, errors.New("test error")
}

func (f *failingAirLog) History(ctx context.Context, limit int) ([]live.Entry, error) {
	return f.log.History(ctx, limit)
}

func (f *failingAirLog) AiredSince(ctx context.Context, ytID string, since time.Time) (bool, error) {
	return f.log.AiredSince(ctx, ytID, since)
}

func (f *failingAirLog) RecentYTIDs(ctx context.Context, n int) ([]string, error) {
	return f.log.RecentYTIDs(ctx, n)
}

func TestGetQueueAirLogError(t *testing.T) {
	s, st, log, _ := newLiveTestServer(t, "a")
	ctx := context.Background()
	id := mkPlaylist(t, s, "mix", "a")
	_, err := s.SetActivePlaylist(ctx, &radiov1.SetActivePlaylistRequest{PlaylistId: id})
	require.NoError(t, err)
	_, err = s.GoOnAir(ctx, &radiov1.GoOnAirRequest{})
	require.NoError(t, err)

	// Replace with a failing AirLog
	s.deps.Log = &failingAirLog{log: log}
	_ = st // unused in this test

	_, err = s.GetQueue(ctx, &radiov1.GetQueueRequest{})
	require.Equal(t, codes.Internal, status.Code(err))
}
