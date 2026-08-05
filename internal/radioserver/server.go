// Package radioserver implements algovn.radio.v1.RadioService — the public
// radio product's station console surface (station control + moderation)
// and its listener request surface. Input validation and sentinel→gRPC-code
// mapping live here; state guards live in station.Store and request.Store.
package radioserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	radiov1 "github.com/the-algovn/protos/gen/go/algovn/radio/v1"
	ttsv1 "github.com/the-algovn/protos/gen/go/algovn/tts/v1"
	"github.com/the-algovn/radio-service/internal/broadcast"
	"github.com/the-algovn/radio-service/internal/director"
	"github.com/the-algovn/radio-service/internal/ingest"
	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/schedule"
	"github.com/the-algovn/radio-service/internal/showlog"
	"github.com/the-algovn/radio-service/internal/station"
	"github.com/the-algovn/radio-service/internal/timeline"
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

	// DJ settings bounds (spec §6). Rate matches the audition slider range;
	// the max_chars floor prevents an unsatisfiable script cap (which would
	// burn one LLM retry loop per due tick), the ceiling bounds per-break cost.
	djMinRate     = 0.7
	djMaxRate     = 1.3
	djMinMaxChars = 50
	djMaxMaxChars = 2000
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

// Breaks exposes the director's cadence state read-only. Narrow and local,
// matching Notifier/Skipper/Ledger — radioserver has no business calling the
// director's mutating methods.
type Breaks interface{ Snapshot() director.Snapshot }

// ShowLog is the read side of the show log: what aired, merged music+talk.
// Append lives on the full Store; radioserver has no business writing it.
type ShowLog interface {
	Recent(ctx context.Context, limit, offset int) ([]showlog.Segment, error)
	Count(ctx context.Context) (int64, error)
}

// Sessions is the read side of the broadcast-session log.
type Sessions interface {
	Overlapping(ctx context.Context, from, to time.Time, limit int) ([]broadcast.Session, error)
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
	Sched     schedule.Store
	Library   library.Library
	Producer  live.Producer
	Location  *time.Location // station-local civil day for daily quotas; nil → UTC
	Skipper   Skipper
	Ledger    Ledger
	Breaks    Breaks
	ShowLog   ShowLog
	Sessions  Sessions
	BudgetUSD float64
	TTS       ttsv1.TTSServiceClient // voice catalog membership for UpdateDJSettings
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

func djSettingsProto(dj station.DJSettings) *radiov1.DJSettings {
	return &radiov1.DJSettings{VoiceId: dj.VoiceID, SpeakingRate: dj.Rate,
		BreakEvery: int32(dj.BreakEvery), StationIdMin: int32(dj.StationIDMin),
		MaxChars: int32(dj.MaxChars)}
}

// canonicalVoiceID mirrors catalog.Resolve's namespacing rule in tts-service:
// an id with no "provider:" prefix is a bare id persisted before tts-service
// existed (e.g. the 00010 migration's dj_voice_id default) and resolves to
// google, tts-service's original sole provider.
func canonicalVoiceID(id string) string {
	if !strings.Contains(id, ":") {
		return "google:" + id
	}
	return id
}

// voiceKnown reports catalog membership by asking the shared tts-service.
// Both sides are canonicalized before comparing, since a bare persisted id
// (e.g. "vi-VN-Neural2-A") must match the catalog's namespaced id
// ("google:vi-VN-Neural2-A") for the same voice.
// "fake" is deliberately NOT accepted: the shared catalog never lists it,
// and persisting it would poison the row for the day a real backend key is
// missing (spec §6).
func (s *Server) voiceKnown(ctx context.Context, id string) (bool, error) {
	resp, err := s.deps.TTS.ListVoices(ctx, &ttsv1.ListVoicesRequest{})
	if err != nil {
		return false, err
	}
	want := canonicalVoiceID(id)
	for _, v := range resp.GetVoices() {
		if canonicalVoiceID(v.GetId()) == want {
			return true, nil
		}
	}
	return false, nil
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
	}, Dj: djSettingsProto(st.DJ)}, nil
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
	s.logger.InfoContext(ctx, "station on air")
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
	s.logger.InfoContext(ctx, "station off air")
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
	s.logger.InfoContext(ctx, "listener request queued", "yt_id", it.YTID, "sub", sub, "status", it.Status)
	live.PublishQueueSnapshot(ctx, s.deps.Producer, s.deps.Requests, s.deps.Sched, s.logger)
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
	live.PublishQueueSnapshot(ctx, s.deps.Producer, s.deps.Requests, s.deps.Sched, s.logger)
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
	s.logger.InfoContext(ctx, "operator removed request", "id", req.GetId())
	live.PublishQueueSnapshot(ctx, s.deps.Producer, s.deps.Requests, s.deps.Sched, s.logger)
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
	s.logger.InfoContext(ctx, "operator skipped the airing track")
	return &radiov1.SkipTrackResponse{}, nil
}

func (s *Server) SetAIEnabled(ctx context.Context, req *radiov1.SetAIEnabledRequest) (*radiov1.SetAIEnabledResponse, error) {
	st, err := s.deps.Store.SetAIEnabled(ctx, req.GetEnabled())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "set ai enabled: %v", err)
	}
	s.logger.InfoContext(ctx, "ai pause toggled", "enabled", req.GetEnabled())
	return &radiov1.SetAIEnabledResponse{Station: stationProto(st)}, nil
}

func (s *Server) UpdateDJSettings(ctx context.Context, req *radiov1.UpdateDJSettingsRequest) (*radiov1.UpdateDJSettingsResponse, error) {
	in := req.GetSettings()
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "settings are required")
	}
	cur, err := s.deps.Store.GetStation(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get station: %v", err)
	}
	// Only consult the shared tts-service catalog when voice_id is actually
	// changing. voiceKnown's only job is validating a NEW voice choice; a
	// codes.Unavailable tts-service must not block edits to the other DJ
	// fields, including the one an operator most wants when TTS itself is
	// broken (break_every: 0, to stop the DJ attempting breaks).
	if canonicalVoiceID(in.GetVoiceId()) != canonicalVoiceID(cur.DJ.VoiceID) {
		known, err := s.voiceKnown(ctx, in.GetVoiceId())
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "voice catalog: %v", err)
		}
		if !known {
			return nil, status.Error(codes.InvalidArgument, "unknown voice_id")
		}
	}
	if r := in.GetSpeakingRate(); r < djMinRate || r > djMaxRate {
		return nil, status.Errorf(codes.InvalidArgument, "speaking_rate must be between %.1f and %.1f", djMinRate, djMaxRate)
	}
	if in.GetBreakEvery() < 0 {
		return nil, status.Error(codes.InvalidArgument, "break_every must be >= 0 (0 disables)")
	}
	if in.GetStationIdMin() < 0 {
		return nil, status.Error(codes.InvalidArgument, "station_id_min must be >= 0 (0 disables)")
	}
	if n := in.GetMaxChars(); n < djMinMaxChars || n > djMaxMaxChars {
		return nil, status.Errorf(codes.InvalidArgument, "max_chars must be between %d and %d", djMinMaxChars, djMaxMaxChars)
	}
	st, err := s.deps.Store.UpdateDJSettings(ctx, station.DJSettings{
		VoiceID: in.GetVoiceId(), Rate: in.GetSpeakingRate(),
		BreakEvery: int(in.GetBreakEvery()), StationIDMin: int(in.GetStationIdMin()),
		MaxChars: int(in.GetMaxChars()),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "update dj settings: %v", err)
	}
	s.logger.InfoContext(ctx, "dj settings updated", "voice", st.DJ.VoiceID, "rate", st.DJ.Rate,
		"break_every", st.DJ.BreakEvery, "station_id_min", st.DJ.StationIDMin, "max_chars", st.DJ.MaxChars)
	return &radiov1.UpdateDJSettingsResponse{Settings: djSettingsProto(st.DJ)}, nil
}

// GetShowTimeline returns the full show context: what aired, what is airing,
// what comes next, and the broadcast-session dividers. One read pass assembles
// timeline.State, then timeline.Project projects the forward running order.
func (s *Server) GetShowTimeline(ctx context.Context, req *radiov1.GetShowTimelineRequest) (*radiov1.GetShowTimelineResponse, error) {
	now := s.deps.Now()
	limit := int(req.GetLimit())
	offset := int(req.GetOffset())

	// 1. Station — the on-air flag, AI switch and DJ cadence settings.
	st, err := s.deps.Store.GetStation(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "station: %v", err)
	}

	// 2. Show log page and total count.
	segs, err := s.deps.ShowLog.Recent(ctx, limit, offset)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "show log: %v", err)
	}
	total, err := s.deps.ShowLog.Count(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "show log count: %v", err)
	}

	// 3. Detect airing independently of the page, so total_past is the same
	// answer for every offset. One extra indexed, single-row read.
	newest, err := s.deps.ShowLog.Recent(ctx, 1, 0)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "show log recent: %v", err)
	}
	// The engine writes the NOMINAL duration at track start and never amends
	// it, so a track cut short keeps a future end time forever. Arithmetic
	// alone would report that row `airing` and — worse, once back on air —
	// anchor the forward walk to a phantom end minutes from now. OnAirSince
	// holds only the CURRENT session (see internal/broadcast), so a row that
	// predates it belongs to a session that has already ended.
	var airing *showlog.Segment
	if len(newest) > 0 && st.OnAir && st.OnAirSince != nil {
		n := newest[0]
		if !n.StartedAt.Before(*st.OnAirSince) &&
			n.StartedAt.Add(time.Duration(n.DurationS)*time.Second).After(now) {
			airing = &n
		}
	}
	totalPast := total
	if airing != nil {
		totalPast--
	}

	// Build past, excluding the airing row when it is in the page (offset == 0).
	past := make([]showlog.Segment, 0, len(segs))
	for i := range segs {
		if airing != nil && segs[i].ID == airing.ID && segs[i].Origin == airing.Origin {
			continue
		}
		past = append(past, segs[i])
	}

	// 4. Committed next-up. Empty YTID means NOT committed (ClearNextUp writes
	// empty strings and the singleton row always exists).
	var nextUp *schedule.NextUp
	nu, found, err := s.deps.Sched.GetNextUp(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "next up: %v", err)
	}
	if found && nu.YTID != "" {
		nextUp = &nu
	}

	// 5. Pending set + Durations map. Durations membership IS the
	// library-existence signal: absent means NOT AIRABLE. Populate for every
	// track successfully resolved — the airing track, every ready request, and
	// the pinned next-up. A miss degrades the item (no entry in Durations, so
	// the projector skips or marks it unknown) but never fails the RPC.
	pending, err := s.deps.Requests.Pending(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "pending: %v", err)
	}
	durations := make(map[string]int)

	if airing != nil && airing.Origin == showlog.OriginAir && airing.YTID != "" {
		if tr, ok, _ := s.deps.Library.Get(ctx, airing.YTID); ok {
			durations[airing.YTID] = int(tr.DurationS)
		}
	}
	for _, it := range pending {
		if it.Status != request.StatusReady {
			continue
		}
		if _, exists := durations[it.YTID]; exists {
			continue
		}
		if tr, ok, _ := s.deps.Library.Get(ctx, it.YTID); ok {
			durations[it.YTID] = int(tr.DurationS)
		}
	}
	if nextUp != nil {
		if _, exists := durations[nextUp.YTID]; !exists {
			if tr, ok, _ := s.deps.Library.Get(ctx, nextUp.YTID); ok {
				durations[nextUp.YTID] = int(tr.DurationS)
			}
		}
	}

	// 6. Median track seconds from the air_log rows in the page already
	// fetched (no extra query). 0 when under 5 rows, so Project falls back.
	medianS := medianTrackS(segs)

	// 7. Listeners and daily spend.
	listeners, err := s.deps.Listeners.Count(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listeners: %v", err)
	}
	spent, err := s.deps.Ledger.SpentSince(ctx, request.DayStart(now, s.deps.Location))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "spend: %v", err)
	}

	// 8. Director snapshot — nil Breaks means RADIO_DJ_ENABLED is unset.
	// Gate will report dj_disabled and no break segments will be emitted.
	var dirSnap timeline.DirectorSnapshot
	if s.deps.Breaks != nil {
		snap := s.deps.Breaks.Snapshot()
		dirSnap = timeline.DirectorSnapshot{
			Present:             true,
			HasClip:             snap.HasClip,
			ClipKind:            snap.ClipKind,
			ClipDurationS:       snap.ClipDurationS,
			ClipAnchorYTID:      snap.ClipAnchorYTID,
			ClipAnchorStartedAt: snap.ClipAnchorStartedAt,
			ClipBacksellTitle:   snap.ClipBacksellTitle,
			ClipPromiseTitle:    snap.ClipPromiseTitle,
			ClipCorrelationID:   snap.ClipCorrelationID,
			FinishedSinceSeam:   snap.FinishedSinceSeam,
			LastStationID:       snap.LastStationID,
		}
	}

	// 9. Broadcast session markers spanning the returned page.
	// A read failure degrades to no markers; never fail the RPC.
	var sessionMarkers []*radiov1.SessionMarker
	if s.deps.Sessions != nil {
		sessionMarkers = s.sessionMarkers(ctx, segs, past, airing, now, offset)
	}

	// 10. Project the forward running order.
	var airingSeg *timeline.Segment
	if airing != nil {
		as := showlogToTimeline(airing, timeline.CertaintyAiring)
		airingSeg = &as
	}
	state := timeline.State{
		Now:          now,
		Station:      st,
		Airing:       airingSeg,
		NextUp:       nextUp,
		Pending:      pending,
		Durations:    durations,
		MedianTrackS: medianS,
		Dir:          dirSnap,
		Listeners:    listeners,
		SpentUSD:     spent,
		BudgetUSD:    s.deps.BudgetUSD,
	}
	upcoming, staging, gate := timeline.Project(state)

	// 11. Map to proto.
	resp := &radiov1.GetShowTimelineResponse{
		BreakGate: gate,
		TotalPast: totalPast,
		ServerNow: now.Format(time.RFC3339),
		Sessions:  sessionMarkers,
	}
	for i := range past {
		resp.Past = append(resp.Past, showlogToProto(&past[i]))
	}
	if airingSeg != nil {
		resp.Airing = timelineToProto(airingSeg)
	}
	for i := range upcoming {
		resp.Upcoming = append(resp.Upcoming, timelineToProto(&upcoming[i]))
	}
	for i := range staging {
		resp.Staging = append(resp.Staging, timelineToProto(&staging[i]))
	}
	return resp, nil
}

// sessionMarkers queries broadcast sessions spanning the returned page.
// A read error logs and returns nil — sessions are a degradation, not a failure.
func (s *Server) sessionMarkers(ctx context.Context, segs, past []showlog.Segment, airing *showlog.Segment, now time.Time, offset int) []*radiov1.SessionMarker {
	from := oldestStart(segs, past, airing, now)
	to := now
	sessions, err := s.deps.Sessions.Overlapping(ctx, from, to, 0)
	if err != nil {
		s.logger.ErrorContext(ctx, "broadcast sessions read failed", "err", err)
		return nil
	}
	out := make([]*radiov1.SessionMarker, 0, len(sessions))
	for _, ses := range sessions {
		m := &radiov1.SessionMarker{StartedAt: ses.StartedAt.Format(time.RFC3339)}
		if ses.EndedAt != nil {
			m.EndedAt = ses.EndedAt.Format(time.RFC3339)
		}
		out = append(out, m)
	}
	return out
}

// oldestStart returns the earliest StartedAt in the page, for the sessions
// query. Falls back to 12h ago when the page is empty.
func oldestStart(segs, past []showlog.Segment, airing *showlog.Segment, now time.Time) time.Time {
	// The page might segs or past; use the larger set.
	candidates := segs
	if len(past) > len(candidates) {
		candidates = past
	}
	if len(candidates) == 0 && airing == nil {
		return now.Add(-12 * time.Hour)
	}
	oldest := now
	for _, seg := range candidates {
		if seg.StartedAt.Before(oldest) {
			oldest = seg.StartedAt
		}
	}
	if airing != nil && airing.StartedAt.Before(oldest) {
		oldest = airing.StartedAt
	}
	return oldest
}

// medianTrackS computes the median duration of music rows (OriginAir) in segs.
// Returns 0 when under 5 rows so the projector falls back to FallbackMedianS.
func medianTrackS(segs []showlog.Segment) int {
	var ds []int
	for _, seg := range segs {
		if seg.Origin == showlog.OriginAir && seg.DurationS > 0 {
			ds = append(ds, seg.DurationS)
		}
	}
	if len(ds) < 5 {
		return 0
	}
	sort.Ints(ds)
	return ds[len(ds)/2]
}

// showSegmentID mints the stable identity a console keys its rows on.
//
// It must be ORIGIN-QUALIFIED: air_log.id and talk_segment.id are independent
// BIGSERIALs that RecentShowSegments UNION ALLs, so a bare id collides across
// origins on any fresh database. And it must be ROLE-INDEPENDENT: the same row
// is `airing` on one poll and `past` on the next, and a changing id remounts
// the row in React.
func showSegmentID(seg *showlog.Segment) string {
	return fmt.Sprintf("log:%s:%d", seg.Origin, seg.ID)
}

// showlogToTimeline converts one showlog.Segment to a timeline.Segment,
// branching on Origin so the projector sees the right Kind and fields.
func showlogToTimeline(seg *showlog.Segment, cert string) timeline.Segment {
	out := timeline.Segment{
		SegmentID: showSegmentID(seg),
		Kind:      showlogKindToTimeline(seg),
		Certainty: cert,
		Title:     seg.Title,
		StartedAt: seg.StartedAt,
		DurationS: seg.DurationS,
	}
	if seg.Origin == showlog.OriginAir {
		out.YTID = seg.YTID
		out.Artist = seg.Artist
		out.Source = seg.Source
		out.RequestedByName = seg.RequestedByName
		out.Reason = seg.Reason
	} else {
		out.Script = seg.Script
		out.BacksellTitle = seg.BacksellTitle
		out.PromiseTitle = seg.PromiseTitle
		out.CorrelationID = seg.CorrelationID
		out.Model = seg.Model
		out.InTokens = seg.InTokens
		out.OutTokens = seg.OutTokens
		out.CostUSD = seg.CostUSD
		out.LatencyMS = seg.LatencyMS
	}
	return out
}

// showlogToProto maps a past showlog.Segment to a proto ShowSegment. Past rows
// are always "aired". The mapping branches on Origin: music rows carry YTID
// and air-log provenance; talk rows carry script and LLM provenance.
func showlogToProto(seg *showlog.Segment) *radiov1.ShowSegment {
	out := &radiov1.ShowSegment{
		SegmentId: showSegmentID(seg),
		Kind:      showlogKindToTimeline(seg),
		Certainty: timeline.CertaintyAired,
		Title:     seg.Title,
		StartedAt: seg.StartedAt.Format(time.RFC3339),
		DurationS: int32(seg.DurationS),
	}
	if seg.Origin == showlog.OriginAir {
		out.Artist = seg.Artist
		out.Source = seg.Source
		out.RequestedByName = seg.RequestedByName
		out.Reason = seg.Reason
	} else {
		out.Script = seg.Script
		out.BacksellTitle = seg.BacksellTitle
		out.PromiseTitle = seg.PromiseTitle
		out.CorrelationId = seg.CorrelationID
		out.Model = seg.Model
		out.InTokens = int32(seg.InTokens)
		out.OutTokens = int32(seg.OutTokens)
		out.CostUsd = seg.CostUSD
		out.LatencyMs = int32(seg.LatencyMS)
	}
	return out
}

// timelineToProto converts a timeline.Segment (past, airing, upcoming, or
// staging) to a proto ShowSegment. Fields are set unconditionally; the
// consumer branches on Kind at the other end.
func timelineToProto(seg *timeline.Segment) *radiov1.ShowSegment {
	out := &radiov1.ShowSegment{
		SegmentId:       seg.SegmentID,
		Kind:            seg.Kind,
		Certainty:       seg.Certainty,
		Title:           seg.Title,
		Artist:          seg.Artist,
		ThumbnailUrl:    seg.ThumbnailURL,
		DurationS:       int32(seg.DurationS),
		Source:          seg.Source,
		RequestedByName: seg.RequestedByName,
		Reason:          seg.Reason,
		RequestId:       seg.RequestID,
		Status:          seg.Status,
		Script:          seg.Script,
		BacksellTitle:   seg.BacksellTitle,
		PromiseTitle:    seg.PromiseTitle,
		CorrelationId:   seg.CorrelationID,
		Model:           seg.Model,
		InTokens:        int32(seg.InTokens),
		OutTokens:       int32(seg.OutTokens),
		CostUsd:         seg.CostUSD,
		LatencyMs:       int32(seg.LatencyMS),
	}
	if !seg.StartedAt.IsZero() {
		out.StartedAt = seg.StartedAt.Format(time.RFC3339)
	}
	return out
}

// showlogKindToTimeline translates the stored kind (Origin + Kind) to the wire
// vocabulary. Stored kind "seam" maps to wire "dj"; "station_id" stays the same;
// air-log rows are always "track".
func showlogKindToTimeline(seg *showlog.Segment) string {
	if seg.Origin == showlog.OriginAir {
		return timeline.KindTrack
	}
	return timeline.KindFromEngine(seg.Kind)
}
