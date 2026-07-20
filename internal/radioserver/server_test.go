package radioserver

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	radiov1 "github.com/the-algovn/protos/gen/go/algovn/radio/v1"
	"github.com/the-algovn/radio-service/internal/library"
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

	_, err = s.GoOnAir(ctx, &radiov1.GoOnAirRequest{})
	require.Equal(t, codes.FailedPrecondition, status.Code(err)) // no active playlist

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
