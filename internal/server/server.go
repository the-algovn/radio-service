// Package server implements algovn.radiolab.v1.LabService over the lab's
// internal packages. Deps grows one field per bench task.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	radiolabv1 "github.com/the-algovn/protos/gen/go/algovn/radiolab/v1"
	"github.com/the-algovn/radio-service/internal/artifact"
	"github.com/the-algovn/radio-service/internal/brain"
	"github.com/the-algovn/radio-service/internal/callin"
	"github.com/the-algovn/radio-service/internal/ingest"
	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/persona"
	"github.com/the-algovn/radio-service/internal/render"
	"github.com/the-algovn/radio-service/internal/spend"
	"github.com/the-algovn/radio-service/internal/voice"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

type Deps struct {
	Ledger          spend.Ledger
	Store           artifact.Store
	Voice           voice.Provider
	VoiceFake       bool
	Models          map[string]brain.Model // keys: gemini | anthropic | fake
	DefaultModel    string                 // key into Models
	PersonaDir      string
	PersonaReadonly bool
	FixturesDir     string
	Ingest          *ingest.Runner
	Library         library.Library
	TmpDir          string
	Logger          *slog.Logger
}

type Server struct {
	radiolabv1.UnimplementedLabServiceServer
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

// ledger appends a line to the spend ledger, logging (not discarding) any
// append failure.
func (s *Server) ledger(ctx context.Context, line spend.Line) {
	if err := s.deps.Ledger.Append(ctx, line); err != nil {
		s.logger.Error("ledger append failed", "err", err)
	}
}

// truncateRunes returns s truncated to at most n runes.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func (s *Server) GetLedger(ctx context.Context, _ *radiolabv1.GetLedgerRequest) (*radiolabv1.GetLedgerResponse, error) {
	lines, err := s.deps.Ledger.All(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read ledger: %v", err)
	}
	resp := &radiolabv1.GetLedgerResponse{TotalUsd: spend.Total(lines)}
	for _, ln := range lines {
		resp.Lines = append(resp.Lines, &radiolabv1.LedgerLine{
			Ts: ln.TS.Format(time.RFC3339), Kind: ln.Kind, Provider: ln.Provider, Label: ln.Label,
			Chars: int32(ln.Chars), InTokens: int32(ln.InTokens), OutTokens: int32(ln.OutTokens), CostUsd: ln.CostUSD,
		})
	}
	return resp, nil
}

func (s *Server) artifactToProto(ctx context.Context, a artifact.Artifact) *radiolabv1.Artifact {
	url, err := s.deps.Store.PresignGet(ctx, a.ID)
	if err != nil {
		s.logger.Error("presign failed", "id", a.ID, "err", err)
	}
	return &radiolabv1.Artifact{
		Id: a.ID, Kind: a.Kind, Label: a.Label, Ext: a.Ext, Bytes: a.Bytes,
		CreatedAt: a.CreatedAt.Format(time.RFC3339), Meta: a.Meta, Url: url,
	}
}

// PresignArtifact hands the browser a time-limited MinIO GET URL for one
// artifact id — the console resolves this lazily when a track is played.
func (s *Server) PresignArtifact(ctx context.Context, req *radiolabv1.PresignArtifactRequest) (*radiolabv1.PresignArtifactResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	url, err := s.deps.Store.PresignGet(ctx, req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "presign artifact: %v", err)
	}
	return &radiolabv1.PresignArtifactResponse{Url: url}, nil
}

func (s *Server) ListVoices(context.Context, *radiolabv1.ListVoicesRequest) (*radiolabv1.ListVoicesResponse, error) {
	resp := &radiolabv1.ListVoicesResponse{}
	for _, v := range voice.Voices() {
		resp.Voices = append(resp.Voices, &radiolabv1.Voice{Id: v.ID, Label: v.Label, Tier: v.Tier})
	}
	if s.deps.VoiceFake {
		resp.Voices = append(resp.Voices, &radiolabv1.Voice{Id: "fake", Label: "Fake (no key)", Tier: "fake"})
	}
	return resp, nil
}

func (s *Server) SynthesizeVoice(ctx context.Context, req *radiolabv1.SynthesizeVoiceRequest) (*radiolabv1.SynthesizeVoiceResponse, error) {
	if req.GetText() == "" || req.GetVoiceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "text and voice_id are required")
	}
	data, ext, err := s.deps.Voice.Synthesize(ctx, req.GetText(), req.GetVoiceId(), req.GetSpeakingRate())
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "tts: %v", err)
	}
	label := req.GetLabel()
	if label == "" {
		label = req.GetVoiceId()
	}
	a, err := s.deps.Store.Save(ctx, "take", ext, label, data, map[string]string{"voice": req.GetVoiceId(), "text": req.GetText()})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "save take: %v", err)
	}
	chars := utf8.RuneCountInString(req.GetText())
	cost := voice.CostUSD(req.GetVoiceId(), chars)
	if s.deps.VoiceFake {
		cost = 0
	}
	s.ledger(ctx, spend.Line{TS: time.Now(), Kind: "tts", Provider: providerName(s.deps.VoiceFake, "google"), Label: label, Chars: chars, CostUSD: cost})
	return &radiolabv1.SynthesizeVoiceResponse{Artifact: s.artifactToProto(ctx, a), CostUsd: cost, Fake: s.deps.VoiceFake}, nil
}

func (s *Server) ListArtifacts(ctx context.Context, req *radiolabv1.ListArtifactsRequest) (*radiolabv1.ListArtifactsResponse, error) {
	all, err := s.deps.Store.List(ctx, req.GetKind())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list: %v", err)
	}
	resp := &radiolabv1.ListArtifactsResponse{}
	for _, a := range all {
		resp.Artifacts = append(resp.Artifacts, s.artifactToProto(ctx, a))
	}
	return resp, nil
}

func providerName(fake bool, real string) string {
	if fake {
		return "fake"
	}
	return real
}

func (s *Server) modelFor(name string) (brain.Model, bool) {
	if name == "" {
		name = s.DefaultModelName()
	}
	m, ok := s.deps.Models[name]
	return m, ok
}

func (s *Server) DefaultModelName() string { return s.deps.DefaultModel }

func (s *Server) GenerateScript(ctx context.Context, req *radiolabv1.GenerateScriptRequest) (*radiolabv1.GenerateScriptResponse, error) {
	if req.GetBrief() == nil {
		return nil, status.Error(codes.InvalidArgument, "brief is required")
	}
	m, ok := s.modelFor(req.GetModel())
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unknown model %q", req.GetModel())
	}
	pers := req.GetPersonaOverride()
	if pers == "" {
		var err error
		if pers, err = persona.Load(s.deps.PersonaDir); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "load persona: %v", err)
		}
	}
	briefJSON, err := protojson.Marshal(req.GetBrief())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal brief: %v", err)
	}
	system, user := brain.BuildPrompts(pers, string(briefJSON))
	raw, usage, err := m.Generate(ctx, system, user)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "model: %v", err)
	}
	out, err := brain.ParseOutput(raw)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v (raw: %.200s)", err, raw)
	}
	maxChars := int(req.GetBrief().GetMaxChars())
	if maxChars == 0 {
		maxChars = 800
	}
	cost := brain.CostUSD(m.Name(), usage)
	s.ledger(ctx, spend.Line{TS: time.Now(), Kind: "llm", Provider: m.Name(), Label: "script:" + req.GetBrief().GetType(),
		InTokens: usage.In, OutTokens: usage.Out, CostUSD: cost})
	return &radiolabv1.GenerateScriptResponse{
		Script: out.Script, Summary: out.Summary, UsedPhrases: out.UsedPhrases,
		Violations: brain.Validate(out.Script, maxChars),
		InTokens:   int32(usage.In), OutTokens: int32(usage.Out),
		CostUsd: cost, Fake: m.Name() == "fake", Model: m.Name(),
	}, nil
}

func (s *Server) GetPersona(context.Context, *radiolabv1.GetPersonaRequest) (*radiolabv1.GetPersonaResponse, error) {
	c, err := persona.Load(s.deps.PersonaDir)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "persona: %v", err)
	}
	return &radiolabv1.GetPersonaResponse{Content: c}, nil
}

func (s *Server) SavePersona(_ context.Context, req *radiolabv1.SavePersonaRequest) (*radiolabv1.SavePersonaResponse, error) {
	if s.deps.PersonaReadonly {
		return nil, status.Error(codes.FailedPrecondition, "persona is read-only in this environment; edit persona/*.md and redeploy")
	}
	if strings.TrimSpace(req.GetContent()) == "" {
		return nil, status.Error(codes.InvalidArgument, "content is empty")
	}
	if err := persona.Save(s.deps.PersonaDir, req.GetContent()); err != nil {
		return nil, status.Errorf(codes.Internal, "save persona: %v", err)
	}
	return &radiolabv1.SavePersonaResponse{}, nil
}

func (s *Server) ParseCallIn(ctx context.Context, req *radiolabv1.ParseCallInRequest) (*radiolabv1.ParseCallInResponse, error) {
	if strings.TrimSpace(req.GetText()) == "" {
		return nil, status.Error(codes.InvalidArgument, "text is required")
	}
	m, ok := s.modelFor(req.GetModel())
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unknown model %q", req.GetModel())
	}
	if m.Name() == "fake" {
		// brain.Fake returns script-shaped JSON that fails callin's schema —
		// short-circuit before callin.Parse rather than surface a parse error.
		s.ledger(ctx, spend.Line{TS: time.Now(), Kind: "llm", Provider: "fake", Label: "callin"})
		return &radiolabv1.ParseCallInResponse{
			SongQuery: "", Recipient: "", Message: truncateRunes(req.GetText(), 80),
			Verdict: "allow", RejectReason: "", Digest: "lời nhắn mẫu (fake model)", Weight: "casual",
			CostUsd: 0, Fake: true,
		}, nil
	}
	r, usage, err := callin.Parse(ctx, m, req.GetText())
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "parse: %v", err)
	}
	cost := brain.CostUSD(m.Name(), usage)
	s.ledger(ctx, spend.Line{TS: time.Now(), Kind: "llm", Provider: m.Name(), Label: "callin", InTokens: usage.In, OutTokens: usage.Out, CostUSD: cost})
	return &radiolabv1.ParseCallInResponse{
		SongQuery: r.SongQuery, Recipient: r.Recipient, Message: r.Message,
		Verdict: r.Verdict, RejectReason: r.RejectReason, Digest: r.Digest, Weight: r.Weight,
		CostUsd: cost, Fake: m.Name() == "fake",
	}, nil
}

// fixtureExpected mirrors callin.Result but with the camelCase json tags
// protojson emits, so the console's response JSON (which also carries
// volatile cost_usd/fake fields we must drop) unmarshals cleanly.
type fixtureExpected struct {
	SongQuery    string `json:"songQuery"`
	Recipient    string `json:"recipient"`
	Message      string `json:"message"`
	Verdict      string `json:"verdict"`
	RejectReason string `json:"rejectReason"`
	Digest       string `json:"digest"`
	Weight       string `json:"weight"`
}

func (s *Server) SaveFixture(_ context.Context, req *radiolabv1.SaveFixtureRequest) (*radiolabv1.SaveFixtureResponse, error) {
	var fe fixtureExpected
	if err := json.Unmarshal([]byte(req.GetExpectedJson()), &fe); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "expected_json: %v", err)
	}
	r, err := callin.Normalize(callin.Result{
		SongQuery: fe.SongQuery, Recipient: fe.Recipient, Message: fe.Message,
		Verdict: fe.Verdict, RejectReason: fe.RejectReason, Digest: fe.Digest, Weight: fe.Weight,
	})
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "expected_json: %v", err)
	}
	canonical, err := json.Marshal(r)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal expected: %v", err)
	}
	p, err := callin.SaveFixture(s.deps.FixturesDir, req.GetName(), req.GetRawText(), string(canonical))
	if err != nil {
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			return nil, status.Errorf(codes.Internal, "save fixture: %v", err)
		}
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	return &radiolabv1.SaveFixtureResponse{Path: p}, nil
}

func (s *Server) SearchTracks(ctx context.Context, req *radiolabv1.SearchTracksRequest) (*radiolabv1.SearchTracksResponse, error) {
	if strings.TrimSpace(req.GetQuery()) == "" {
		return nil, status.Error(codes.InvalidArgument, "query is required")
	}
	cs, err := s.deps.Ingest.Search(ctx, req.GetQuery(), int(req.GetLimit()))
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "search: %v", err)
	}
	resp := &radiolabv1.SearchTracksResponse{}
	for _, sc := range ingest.Rank(req.GetQuery(), cs) {
		resp.Candidates = append(resp.Candidates, &radiolabv1.Candidate{
			YtId: sc.YTID, Title: sc.Title, Channel: sc.Channel, DurationS: sc.DurationS,
			ViewCount: sc.ViewCount, ThumbnailUrl: sc.ThumbnailURL, Score: int32(sc.Score), ScoreNotes: sc.Notes,
		})
	}
	return resp, nil
}

func (s *Server) ListTracks(ctx context.Context, req *radiolabv1.ListTracksRequest) (*radiolabv1.ListTracksResponse, error) {
	tracks, err := s.deps.Library.List(ctx, req.GetQuery(), int(req.GetLimit()), int(req.GetOffset()))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list tracks: %v", err)
	}
	total, err := s.deps.Library.Count(ctx, req.GetQuery())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "count tracks: %v", err)
	}
	resp := &radiolabv1.ListTracksResponse{Total: total}
	for _, tr := range tracks {
		resp.Tracks = append(resp.Tracks, &radiolabv1.LibraryTrack{
			YtId: tr.YTID, Title: tr.Title, Channel: tr.Channel, DurationS: int64(tr.DurationS),
			ArtifactId: tr.ArtifactID, InputI: tr.InputI, InputTp: tr.InputTP, InputLra: tr.InputLRA,
			AddedAt: tr.AddedAt.Format(time.RFC3339),
		})
	}
	return resp, nil
}

func (s *Server) DeleteTrack(ctx context.Context, req *radiolabv1.DeleteTrackRequest) (*radiolabv1.DeleteTrackResponse, error) {
	if req.GetYtId() == "" {
		return nil, status.Error(codes.InvalidArgument, "yt_id is required")
	}
	artifactID, found, err := s.deps.Library.Delete(ctx, req.GetYtId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "delete track: %v", err)
	}
	if !found {
		return nil, status.Errorf(codes.NotFound, "track %q not found", req.GetYtId())
	}
	if err := s.deps.Store.Delete(ctx, artifactID); err != nil {
		s.logger.Error("store delete failed", "artifact_id", artifactID, "err", err)
	}
	return &radiolabv1.DeleteTrackResponse{}, nil
}

func (s *Server) DownloadTrack(ctx context.Context, req *radiolabv1.DownloadTrackRequest) (*radiolabv1.DownloadTrackResponse, error) {
	if req.GetYtId() == "" {
		return nil, status.Error(codes.InvalidArgument, "yt_id is required")
	}
	if tr, found, _ := s.deps.Library.Get(ctx, req.GetYtId()); found {
		a, err := s.deps.Store.Get(ctx, tr.ArtifactID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "store get: %v", err)
		}
		return &radiolabv1.DownloadTrackResponse{
			Artifact: s.artifactToProto(ctx, a), DurationS: tr.DurationS,
			InputI: tr.InputI, InputTp: tr.InputTP, InputLra: tr.InputLRA, Cached: true,
		}, nil
	}
	tmp, err := os.MkdirTemp(s.deps.TmpDir, "dl-*")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "tmp: %v", err)
	}
	defer os.RemoveAll(tmp)
	p, err := s.deps.Ingest.Download(ctx, req.GetYtId(), tmp)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "download: %v", err)
	}
	dur, err := ingest.Probe(ctx, p)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "probe: %v", err)
	}
	i, tp, lra, err := ingest.Loudnorm(ctx, p)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "loudnorm: %v", err)
	}
	label := req.GetTitle()
	if label == "" {
		label = req.GetYtId()
	}
	a, err := s.deps.Store.SaveFile(ctx, "track", p, label, map[string]string{
		"yt_id": req.GetYtId(), "duration_s": fmt.Sprintf("%.1f", dur), "input_i": fmt.Sprintf("%.1f", i),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store: %v", err)
	}
	if err := s.deps.Library.Add(ctx, library.Track{
		YTID: req.GetYtId(), Title: label, Channel: req.GetChannel(), DurationS: dur,
		ArtifactID: a.ID, InputI: i, InputTP: tp, InputLRA: lra,
	}); err != nil {
		s.logger.Error("library add failed", "yt_id", req.GetYtId(), "err", err)
	}
	return &radiolabv1.DownloadTrackResponse{Artifact: s.artifactToProto(ctx, a), DurationS: dur, InputI: i, InputTp: tp, InputLra: lra}, nil
}

func (s *Server) RenderPreview(ctx context.Context, req *radiolabv1.RenderPreviewRequest) (*radiolabv1.RenderPreviewResponse, error) {
	tmp, err := os.MkdirTemp(s.deps.TmpDir, "render-*")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "tmp: %v", err)
	}
	defer os.RemoveAll(tmp)
	trackPath, err := s.deps.Store.FetchToFile(ctx, req.GetTrackArtifactId(), tmp)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "track artifact: %v", err)
	}
	voicePath, err := s.deps.Store.FetchToFile(ctx, req.GetVoiceArtifactId(), tmp)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "voice artifact: %v", err)
	}
	out, dur, err := render.Preview(ctx, trackPath, voicePath, tmp, render.Knobs{
		OffsetS: req.GetOffsetS(), DuckDB: req.GetDuckDb(), TailS: req.GetTailS(),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "render: %v", err)
	}
	label := fmt.Sprintf("render off=%.1f duck=%.1f", req.GetOffsetS(), req.GetDuckDb())
	a, err := s.deps.Store.SaveFile(ctx, "render", out, label, map[string]string{
		"track": req.GetTrackArtifactId(), "voice": req.GetVoiceArtifactId(),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store: %v", err)
	}
	return &radiolabv1.RenderPreviewResponse{Artifact: s.artifactToProto(ctx, a), DurationS: dur}, nil
}
