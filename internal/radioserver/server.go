// Package radioserver implements algovn.radio.v1.RadioService — the public
// radio product's operator surface (Slice 1: playlists + station control).
// Input validation and sentinel→gRPC-code mapping live here; state guards
// live in playlist.Store.
package radioserver

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	radiov1 "github.com/the-algovn/protos/gen/go/algovn/radio/v1"
	"github.com/the-algovn/radio-service/internal/playlist"
)

const maxNameRunes = 200

type Deps struct {
	Store  playlist.Store
	Logger *slog.Logger
}

type Server struct {
	radiov1.UnimplementedRadioServiceServer
	deps   Deps
	logger *slog.Logger
}

func New(deps Deps) *Server {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{deps: deps, logger: logger}
}

// mapErr converts playlist sentinels to gRPC statuses; anything else is
// Internal.
func mapErr(op string, err error) error {
	switch {
	case errors.Is(err, playlist.ErrNotFound):
		return status.Errorf(codes.NotFound, "%s: %v", op, err)
	case errors.Is(err, playlist.ErrEmptyPlaylist),
		errors.Is(err, playlist.ErrNoActivePlaylist),
		errors.Is(err, playlist.ErrActiveOnAir):
		return status.Errorf(codes.FailedPrecondition, "%s: %v", op, err)
	case errors.Is(err, playlist.ErrStale):
		return status.Errorf(codes.InvalidArgument, "%s: %v", op, err)
	default:
		return status.Errorf(codes.Internal, "%s: %v", op, err)
	}
}

// cleanName trims and validates a playlist name.
func cleanName(name string) (string, error) {
	n := strings.TrimSpace(name)
	if n == "" {
		return "", status.Error(codes.InvalidArgument, "name is required")
	}
	if utf8.RuneCountInString(n) > maxNameRunes {
		return "", status.Errorf(codes.InvalidArgument, "name exceeds %d characters", maxNameRunes)
	}
	return n, nil
}

func summaryProto(s playlist.Summary) *radiov1.PlaylistSummary {
	return &radiov1.PlaylistSummary{
		Id: s.ID, Name: s.Name, TrackCount: int32(s.TrackCount),
		TotalDurationS: s.TotalDurationS, IsActive: s.IsActive,
		CreatedAt: s.CreatedAt.Format(time.RFC3339), UpdatedAt: s.UpdatedAt.Format(time.RFC3339),
	}
}

func playlistProto(s playlist.Summary, items []playlist.Item) *radiov1.Playlist {
	p := &radiov1.Playlist{Summary: summaryProto(s)}
	for _, it := range items {
		p.Tracks = append(p.Tracks, &radiov1.PlaylistTrack{
			Position: int32(it.Position), YtId: it.YTID, Title: it.Title,
			Channel: it.Channel, DurationS: it.DurationS,
		})
	}
	return p
}

func stationProto(st playlist.Station) *radiov1.Station {
	out := &radiov1.Station{
		ActivePlaylistId: st.ActivePlaylistID, ActivePlaylistName: st.ActivePlaylistName,
		ActiveTrackCount: int32(st.ActiveTrackCount), OnAir: st.OnAir,
	}
	if st.OnAirSince != nil {
		out.OnAirSince = st.OnAirSince.Format(time.RFC3339)
	}
	return out
}

func (s *Server) CreatePlaylist(ctx context.Context, req *radiov1.CreatePlaylistRequest) (*radiov1.CreatePlaylistResponse, error) {
	name, err := cleanName(req.GetName())
	if err != nil {
		return nil, err
	}
	sum, err := s.deps.Store.Create(ctx, name)
	if err != nil {
		return nil, mapErr("create playlist", err)
	}
	return &radiov1.CreatePlaylistResponse{Summary: summaryProto(sum)}, nil
}

func (s *Server) ListPlaylists(ctx context.Context, _ *radiov1.ListPlaylistsRequest) (*radiov1.ListPlaylistsResponse, error) {
	sums, err := s.deps.Store.List(ctx)
	if err != nil {
		return nil, mapErr("list playlists", err)
	}
	resp := &radiov1.ListPlaylistsResponse{}
	for _, sum := range sums {
		resp.Playlists = append(resp.Playlists, summaryProto(sum))
	}
	return resp, nil
}

func (s *Server) GetPlaylist(ctx context.Context, req *radiov1.GetPlaylistRequest) (*radiov1.GetPlaylistResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	sum, items, err := s.deps.Store.Get(ctx, req.GetId())
	if err != nil {
		return nil, mapErr("get playlist", err)
	}
	return &radiov1.GetPlaylistResponse{Playlist: playlistProto(sum, items)}, nil
}

func (s *Server) RenamePlaylist(ctx context.Context, req *radiov1.RenamePlaylistRequest) (*radiov1.RenamePlaylistResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	name, err := cleanName(req.GetName())
	if err != nil {
		return nil, err
	}
	sum, err := s.deps.Store.Rename(ctx, req.GetId(), name)
	if err != nil {
		return nil, mapErr("rename playlist", err)
	}
	return &radiov1.RenamePlaylistResponse{Summary: summaryProto(sum)}, nil
}

func (s *Server) DeletePlaylist(ctx context.Context, req *radiov1.DeletePlaylistRequest) (*radiov1.DeletePlaylistResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	if err := s.deps.Store.Delete(ctx, req.GetId()); err != nil {
		return nil, mapErr("delete playlist", err)
	}
	return &radiov1.DeletePlaylistResponse{}, nil
}

func (s *Server) AddTrack(ctx context.Context, req *radiov1.AddTrackRequest) (*radiov1.AddTrackResponse, error) {
	if req.GetPlaylistId() == "" || req.GetYtId() == "" {
		return nil, status.Error(codes.InvalidArgument, "playlist_id and yt_id are required")
	}
	sum, items, err := s.deps.Store.AddTrack(ctx, req.GetPlaylistId(), req.GetYtId())
	if err != nil {
		return nil, mapErr("add track", err)
	}
	return &radiov1.AddTrackResponse{Playlist: playlistProto(sum, items)}, nil
}

func (s *Server) RemoveTrack(ctx context.Context, req *radiov1.RemoveTrackRequest) (*radiov1.RemoveTrackResponse, error) {
	if req.GetPlaylistId() == "" || req.GetYtId() == "" {
		return nil, status.Error(codes.InvalidArgument, "playlist_id and yt_id are required")
	}
	sum, items, err := s.deps.Store.RemoveTrack(ctx, req.GetPlaylistId(), req.GetYtId())
	if err != nil {
		return nil, mapErr("remove track", err)
	}
	return &radiov1.RemoveTrackResponse{Playlist: playlistProto(sum, items)}, nil
}

func (s *Server) ReorderTracks(ctx context.Context, req *radiov1.ReorderTracksRequest) (*radiov1.ReorderTracksResponse, error) {
	if req.GetPlaylistId() == "" {
		return nil, status.Error(codes.InvalidArgument, "playlist_id is required")
	}
	sum, items, err := s.deps.Store.Reorder(ctx, req.GetPlaylistId(), req.GetYtIds())
	if err != nil {
		return nil, mapErr("reorder tracks", err)
	}
	return &radiov1.ReorderTracksResponse{Playlist: playlistProto(sum, items)}, nil
}

func (s *Server) GetStation(ctx context.Context, _ *radiov1.GetStationRequest) (*radiov1.GetStationResponse, error) {
	st, err := s.deps.Store.GetStation(ctx)
	if err != nil {
		return nil, mapErr("get station", err)
	}
	return &radiov1.GetStationResponse{Station: stationProto(st)}, nil
}

func (s *Server) SetActivePlaylist(ctx context.Context, req *radiov1.SetActivePlaylistRequest) (*radiov1.SetActivePlaylistResponse, error) {
	if req.GetPlaylistId() == "" {
		return nil, status.Error(codes.InvalidArgument, "playlist_id is required")
	}
	st, err := s.deps.Store.SetActive(ctx, req.GetPlaylistId())
	if err != nil {
		return nil, mapErr("set active playlist", err)
	}
	return &radiov1.SetActivePlaylistResponse{Station: stationProto(st)}, nil
}

func (s *Server) GoOnAir(ctx context.Context, _ *radiov1.GoOnAirRequest) (*radiov1.GoOnAirResponse, error) {
	st, err := s.deps.Store.GoOnAir(ctx)
	if err != nil {
		return nil, mapErr("go on air", err)
	}
	s.logger.Info("station on air", "playlist_id", st.ActivePlaylistID)
	return &radiov1.GoOnAirResponse{Station: stationProto(st)}, nil
}

func (s *Server) GoOffAir(ctx context.Context, _ *radiov1.GoOffAirRequest) (*radiov1.GoOffAirResponse, error) {
	st, err := s.deps.Store.GoOffAir(ctx)
	if err != nil {
		return nil, mapErr("go off air", err)
	}
	s.logger.Info("station off air")
	return &radiov1.GoOffAirResponse{Station: stationProto(st)}, nil
}
