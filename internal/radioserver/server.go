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
	"github.com/the-algovn/radio-service/internal/ingest"
	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/playlist"
	"github.com/the-algovn/radio-service/internal/request"
)

const (
	maxNameRunes    = 200
	maxSessionIDLen = 100

	maxPendingPerUser = 3
	maxPerDay         = 10
	recentAirWindow   = 2 * time.Hour
	maxRequestSeconds = 600
	myRequestsCap     = 50

	msgPendingQuota = "bạn đang có ba bài chờ phát rồi, đợi chút nha"
	msgDailyQuota   = "hôm nay bạn yêu cầu đủ mười bài rồi, mai lại nhé"
	msgDupQueued    = "bài này đang trong hàng đợi rồi, sắp phát thôi"
	msgRecentAired  = "vừa phát xong, để khuya nhé"
	msgTooLong      = "bài dài quá mười phút, đài không phát được"
)

// Notifier lets the server nudge the broadcast engine (same process) when
// on-air state changes; the engine's 5s poll is the fallback.
type Notifier interface{ Poke() }

// Searcher is the yt-dlp search surface (satisfied by *ingest.Runner).
type Searcher interface {
	Search(ctx context.Context, query string, n int) ([]ingest.Candidate, error)
}

type Deps struct {
	Store     playlist.Store
	Log       live.AirLog
	Listeners live.Listeners
	Notifier  Notifier
	Logger    *slog.Logger
	Search    Searcher         // yt-dlp search (nil only in tests that skip it)
	Now       func() time.Time // injected clock; nil → time.Now
	Requests  request.Store
	Library   library.Library
	Producer  live.Producer
	Location  *time.Location // station-local civil day for daily quotas; nil → UTC
}

type Server struct {
	radiov1.UnimplementedRadioServiceServer
	deps   Deps
	logger *slog.Logger
	search *buckets
}

func New(deps Deps) *Server {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Location == nil {
		deps.Location = time.UTC
	}
	return &Server{deps: deps, logger: logger, search: newBuckets(deps.Now)}
}

// mapErr converts playlist sentinels to gRPC statuses; anything else is
// Internal.
func mapErr(op string, err error) error {
	switch {
	case errors.Is(err, playlist.ErrNotFound):
		return status.Errorf(codes.NotFound, "%s: %v", op, err)
	case errors.Is(err, playlist.ErrEmptyPlaylist),
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
	n, err := s.deps.Library.Count(ctx, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "library count: %v", err)
	}
	if n == 0 {
		return nil, status.Error(codes.FailedPrecondition, "library is empty — nothing to broadcast")
	}
	st, err := s.deps.Store.GoOnAir(ctx)
	if err != nil {
		return nil, mapErr("go on air", err)
	}
	s.logger.Info("station on air", "playlist_id", st.ActivePlaylistID)
	if s.deps.Notifier != nil {
		s.deps.Notifier.Poke()
	}
	return &radiov1.GoOnAirResponse{Station: stationProto(st)}, nil
}

func (s *Server) GoOffAir(ctx context.Context, _ *radiov1.GoOffAirRequest) (*radiov1.GoOffAirResponse, error) {
	st, err := s.deps.Store.GoOffAir(ctx)
	if err != nil {
		return nil, mapErr("go off air", err)
	}
	s.logger.Info("station off air")
	if s.deps.Notifier != nil {
		s.deps.Notifier.Poke()
	}
	return &radiov1.GoOffAirResponse{Station: stationProto(st)}, nil
}

func (s *Server) GetNowPlaying(ctx context.Context, _ *radiov1.GetNowPlayingRequest) (*radiov1.GetNowPlayingResponse, error) {
	st, err := s.deps.Store.GetStation(ctx)
	if err != nil {
		return nil, mapErr("get station", err)
	}
	if !st.OnAir {
		return &radiov1.GetNowPlayingResponse{}, nil // absent ⇔ off-air
	}
	e, found, err := s.deps.Log.Latest(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "air log: %v", err)
	}
	if !found {
		return &radiov1.GetNowPlayingResponse{}, nil // on-air but nothing aired yet
	}
	n, err := s.deps.Listeners.Count(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listeners: %v", err)
	}
	return &radiov1.GetNowPlayingResponse{NowPlaying: &radiov1.NowPlaying{
		Kind: "track", Title: e.Title, Artist: e.Artist,
		StartedAt:       e.StartedAt.UTC().Format(time.RFC3339Nano),
		DurationSeconds: int32(e.DurationS), Listeners: int32(n),
		Source: e.Source, RequestedByName: e.RequestedByName, Reason: e.Reason,
	}}, nil
}

func (s *Server) GetQueue(ctx context.Context, _ *radiov1.GetQueueRequest) (*radiov1.GetQueueResponse, error) {
	items, err := s.deps.Requests.Pending(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "queue read: %v", err)
	}
	resp := &radiov1.GetQueueResponse{}
	for _, it := range items {
		resp.Items = append(resp.Items, &radiov1.QueueItem{
			Title: it.Title, Artist: it.Channel, ThumbnailUrl: it.ThumbnailURL,
			Source: it.Source, RequestedByName: it.DisplayName,
			Reason: it.Reason,
		})
	}
	return resp, nil
}

func (s *Server) GetHistory(ctx context.Context, _ *radiov1.GetHistoryRequest) (*radiov1.GetHistoryResponse, error) {
	entries, err := s.deps.Log.History(ctx, 20)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "air history: %v", err)
	}
	resp := &radiov1.GetHistoryResponse{}
	for _, e := range entries {
		resp.Items = append(resp.Items, &radiov1.HistoryItem{
			Title: e.Title, Artist: e.Artist,
			AiredAt: e.StartedAt.UTC().Format(time.RFC3339Nano),
			Source: e.Source, RequestedByName: e.RequestedByName, Reason: e.Reason,
		})
	}
	return resp, nil
}

func (s *Server) Heartbeat(ctx context.Context, req *radiov1.HeartbeatRequest) (*radiov1.HeartbeatResponse, error) {
	id := req.GetSessionId()
	if id == "" || len(id) > maxSessionIDLen {
		return nil, status.Error(codes.InvalidArgument, "session_id is required (max 100 chars)")
	}
	if err := s.deps.Listeners.Beat(ctx, id); err != nil {
		return nil, status.Errorf(codes.Internal, "heartbeat: %v", err)
	}
	return &radiov1.HeartbeatResponse{}, nil
}

const msgSearchRate = "tìm nhanh quá, chờ chút nha"

func (s *Server) SearchCandidates(ctx context.Context, req *radiov1.SearchCandidatesRequest) (*radiov1.SearchCandidatesResponse, error) {
	sub, _, err := identityFromContext(ctx)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "sign in to search")
	}
	q := strings.TrimSpace(req.GetQuery())
	if q == "" {
		return nil, status.Error(codes.InvalidArgument, "query is required")
	}
	if utf8.RuneCountInString(q) > 200 {
		return nil, status.Error(codes.InvalidArgument, "query exceeds 200 characters")
	}
	if !s.search.allow(sub) {
		return nil, status.Error(codes.ResourceExhausted, msgSearchRate)
	}
	cs, err := s.deps.Search.Search(ctx, q, 10)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "search: %v", err)
	}
	resp := &radiov1.SearchCandidatesResponse{}
	for _, sc := range ingest.Rank(q, cs) {
		if len(resp.Candidates) == 8 {
			break
		}
		resp.Candidates = append(resp.Candidates, &radiov1.Candidate{
			YtId: sc.YTID, Title: sc.Title, Channel: sc.Channel,
			DurationS: int32(sc.DurationS), ThumbnailUrl: sc.ThumbnailURL,
		})
	}
	return resp, nil
}

func requestProto(it request.Item) *radiov1.TrackRequest {
	return &radiov1.TrackRequest{
		Id: it.ID, Source: it.Source, RequestedByName: it.DisplayName,
		YtId: it.YTID, Title: it.Title, Channel: it.Channel,
		DurationS: int32(it.DurationS), ThumbnailUrl: it.ThumbnailURL,
		Status: it.Status, FailReason: it.FailReason,
		CreatedAt: it.CreatedAt.Format(time.RFC3339),
	}
}

func (s *Server) RequestTrack(ctx context.Context, req *radiov1.RequestTrackRequest) (*radiov1.RequestTrackResponse, error) {
	sub, name, err := identityFromContext(ctx)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "sign in to request a song")
	}
	c := req.GetCandidate()
	if c == nil || c.GetYtId() == "" || c.GetTitle() == "" {
		return nil, status.Error(codes.InvalidArgument, "candidate with yt_id and title is required")
	}
	if utf8.RuneCountInString(strings.TrimSpace(c.GetTitle())) > maxNameRunes {
		return nil, status.Error(codes.InvalidArgument, "title exceeds 200 characters")
	}
	if utf8.RuneCountInString(strings.TrimSpace(c.GetChannel())) > maxNameRunes {
		return nil, status.Error(codes.InvalidArgument, "channel exceeds 200 characters")
	}
	if c.GetDurationS() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "candidate duration is required")
	}
	if c.GetDurationS() > maxRequestSeconds {
		return nil, status.Error(codes.InvalidArgument, msgTooLong)
	}
	pending, err := s.deps.Requests.CountPendingByUser(ctx, sub)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "pending count: %v", err)
	}
	if pending >= maxPendingPerUser {
		return nil, status.Error(codes.ResourceExhausted, msgPendingQuota)
	}
	today, err := s.deps.Requests.CountSince(ctx, sub, request.DayStart(s.deps.Now(), s.deps.Location))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "daily count: %v", err)
	}
	if today >= maxPerDay {
		return nil, status.Error(codes.ResourceExhausted, msgDailyQuota)
	}
	dup, err := s.deps.Requests.HasPendingYTID(ctx, c.GetYtId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "dup check: %v", err)
	}
	if dup {
		return nil, status.Error(codes.AlreadyExists, msgDupQueued)
	}
	aired, err := s.deps.Log.AiredSince(ctx, c.GetYtId(), s.deps.Now().Add(-recentAirWindow))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "air check: %v", err)
	}
	if aired {
		return nil, status.Error(codes.FailedPrecondition, msgRecentAired)
	}
	st := request.StatusApproved
	if tr, cached, _ := s.deps.Library.Get(ctx, c.GetYtId()); cached {
		// The library row is ground truth here: for a cached track, the
		// client's claimed duration was already checked above but the real
		// probed duration is what matters — a hostile client could claim
		// 240s for a track the library knows is 30 minutes long.
		if tr.DurationS > float64(maxRequestSeconds) {
			return nil, status.Error(codes.InvalidArgument, msgTooLong)
		}
		st = request.StatusReady
	}
	it, err := s.deps.Requests.Create(ctx, request.Item{
		Source: request.SourceListener, RequestedBy: sub, DisplayName: name,
		YTID: c.GetYtId(), Title: c.GetTitle(), Channel: c.GetChannel(),
		DurationS: int64(c.GetDurationS()), ThumbnailURL: c.GetThumbnailUrl(), Status: st,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create request: %v", err)
	}
	s.logger.Info("listener request queued", "yt_id", it.YTID, "sub", sub, "status", it.Status)
	live.PublishQueueSnapshot(ctx, s.deps.Producer, s.deps.Requests, s.logger)
	return &radiov1.RequestTrackResponse{Request: requestProto(it)}, nil
}

func (s *Server) ListMyRequests(ctx context.Context, _ *radiov1.ListMyRequestsRequest) (*radiov1.ListMyRequestsResponse, error) {
	sub, _, err := identityFromContext(ctx)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "sign in to see your requests")
	}
	items, err := s.deps.Requests.ByUser(ctx, sub, myRequestsCap)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list requests: %v", err)
	}
	resp := &radiov1.ListMyRequestsResponse{}
	for _, it := range items {
		resp.Requests = append(resp.Requests, requestProto(it))
	}
	return resp, nil
}
