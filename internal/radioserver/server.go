// Package radioserver implements algovn.radio.v1.RadioService — the public
// radio product's station console surface (station control + moderation)
// and its listener request surface. Input validation and sentinel→gRPC-code
// mapping live here; state guards live in station.Store and request.Store.
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
	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/station"
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

	msgOperatorRemoved = "đài đã gỡ yêu cầu này"
	recentTerminalCap  = 20
)

// Notifier lets the server nudge the broadcast engine (same process) when
// on-air state changes; the engine's 5s poll is the fallback.
type Notifier interface{ Poke() }

// Searcher is the yt-dlp search surface (satisfied by *ingest.Runner).
type Searcher interface {
	Search(ctx context.Context, query string, n int) ([]ingest.Candidate, error)
}

// Skipper asks the feeder to end the airing track (v1.2 moderation).
type Skipper interface{ RequestSkip() }

// Ledger is the spend reader for the operator dashboard (PGLedger and
// MemLedger satisfy it — same shape the programmer uses).
type Ledger interface {
	SpentSince(ctx context.Context, since time.Time) (float64, error)
}

type Deps struct {
	Store     station.Store
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
	Skipper   Skipper
	Ledger    Ledger
	BudgetUSD float64
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

func stationProto(st station.Station) *radiov1.Station {
	out := &radiov1.Station{OnAir: st.OnAir, AiEnabled: st.AIEnabled}
	if st.OnAirSince != nil {
		out.OnAirSince = st.OnAirSince.Format(time.RFC3339)
	}
	return out
}

func (s *Server) GetStation(ctx context.Context, _ *radiov1.GetStationRequest) (*radiov1.GetStationResponse, error) {
	st, err := s.deps.Store.GetStation(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get station: %v", err)
	}
	listeners, err := s.deps.Listeners.Count(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listeners: %v", err)
	}
	libCount, err := s.deps.Library.Count(ctx, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "library count: %v", err)
	}
	spent, err := s.deps.Ledger.SpentSince(ctx, request.DayStart(s.deps.Now(), s.deps.Location))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "spend: %v", err)
	}
	return &radiov1.GetStationResponse{Station: stationProto(st), Stats: &radiov1.StationStats{
		Listeners: int32(listeners), LibraryCount: int32(libCount),
		SpendTodayUsd: spent, BudgetUsd: s.deps.BudgetUSD,
	}}, nil
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
		return nil, status.Errorf(codes.Internal, "go on air: %v", err)
	}
	s.logger.Info("station on air")
	if s.deps.Notifier != nil {
		s.deps.Notifier.Poke()
	}
	return &radiov1.GoOnAirResponse{Station: stationProto(st)}, nil
}

func (s *Server) GoOffAir(ctx context.Context, _ *radiov1.GoOffAirRequest) (*radiov1.GoOffAirResponse, error) {
	st, err := s.deps.Store.GoOffAir(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "go off air: %v", err)
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
		return nil, status.Errorf(codes.Internal, "get station: %v", err)
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
			Source:  e.Source, RequestedByName: e.RequestedByName, Reason: e.Reason,
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
		Reason:    it.Reason,
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

func (s *Server) stationRequests(ctx context.Context) (*radiov1.ListStationRequestsResponse, error) {
	pending, err := s.deps.Requests.Pending(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "pending read: %v", err)
	}
	recent, err := s.deps.Requests.RecentTerminal(ctx, recentTerminalCap)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "recent read: %v", err)
	}
	resp := &radiov1.ListStationRequestsResponse{}
	for _, it := range pending {
		resp.Pending = append(resp.Pending, requestProto(it))
	}
	for _, it := range recent {
		resp.Recent = append(resp.Recent, requestProto(it))
	}
	return resp, nil
}

func (s *Server) ListStationRequests(ctx context.Context, _ *radiov1.ListStationRequestsRequest) (*radiov1.ListStationRequestsResponse, error) {
	return s.stationRequests(ctx)
}

func (s *Server) ReorderRequests(ctx context.Context, req *radiov1.ReorderRequestsRequest) (*radiov1.ListStationRequestsResponse, error) {
	if len(req.GetIds()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ids are required")
	}
	if err := s.deps.Requests.Reorder(ctx, req.GetIds()); err != nil {
		if errors.Is(err, request.ErrStale) {
			return nil, status.Error(codes.InvalidArgument, "stale queue — refresh and retry")
		}
		return nil, status.Errorf(codes.Internal, "reorder: %v", err)
	}
	live.PublishQueueSnapshot(ctx, s.deps.Producer, s.deps.Requests, s.logger)
	return s.stationRequests(ctx)
}

func (s *Server) RemoveRequest(ctx context.Context, req *radiov1.RemoveRequestRequest) (*radiov1.ListStationRequestsResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	if err := s.deps.Requests.FailPending(ctx, req.GetId(), msgOperatorRemoved); err != nil {
		if errors.Is(err, request.ErrNotFound) {
			return nil, status.Error(codes.FailedPrecondition, "request is not pending")
		}
		return nil, status.Errorf(codes.Internal, "remove: %v", err)
	}
	s.logger.Info("operator removed request", "id", req.GetId())
	live.PublishQueueSnapshot(ctx, s.deps.Producer, s.deps.Requests, s.logger)
	return s.stationRequests(ctx)
}

func (s *Server) SkipTrack(ctx context.Context, _ *radiov1.SkipTrackRequest) (*radiov1.SkipTrackResponse, error) {
	st, err := s.deps.Store.GetStation(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get station: %v", err)
	}
	if !st.OnAir {
		return nil, status.Error(codes.FailedPrecondition, "station is off air")
	}
	s.deps.Skipper.RequestSkip()
	s.logger.Info("operator skipped the airing track")
	return &radiov1.SkipTrackResponse{}, nil
}

func (s *Server) SetAIEnabled(ctx context.Context, req *radiov1.SetAIEnabledRequest) (*radiov1.SetAIEnabledResponse, error) {
	st, err := s.deps.Store.SetAIEnabled(ctx, req.GetEnabled())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "set ai enabled: %v", err)
	}
	s.logger.Info("ai pause toggled", "enabled", req.GetEnabled())
	return &radiov1.SetAIEnabledResponse{Station: stationProto(st)}, nil
}
