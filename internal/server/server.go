// Package server implements algovn.radiolab.v1.LabService over the lab's
// internal packages. Deps grows one field per bench task.
package server

import (
	"context"
	"time"
	"unicode/utf8"

	radiolabv1 "github.com/the-algovn/protos/gen/go/algovn/radiolab/v1"
	"github.com/the-algovn/radio-service/internal/artifact"
	"github.com/the-algovn/radio-service/internal/spend"
	"github.com/the-algovn/radio-service/internal/voice"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Deps struct {
	Ledger    *spend.Ledger
	Store     *artifact.Store
	Voice     voice.Provider
	VoiceFake bool
}

type Server struct {
	radiolabv1.UnimplementedLabServiceServer
	deps Deps
}

func New(deps Deps) *Server { return &Server{deps: deps} }

func (s *Server) GetLedger(_ context.Context, _ *radiolabv1.GetLedgerRequest) (*radiolabv1.GetLedgerResponse, error) {
	lines, err := s.deps.Ledger.All()
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

func artifactToProto(a artifact.Artifact) *radiolabv1.Artifact {
	return &radiolabv1.Artifact{
		Id: a.ID, Kind: a.Kind, Label: a.Label, Ext: a.Ext, Bytes: a.Bytes,
		CreatedAt: a.CreatedAt.Format(time.RFC3339), Meta: a.Meta,
	}
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
	a, err := s.deps.Store.Save("take", ext, label, data, map[string]string{"voice": req.GetVoiceId(), "text": req.GetText()})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "save take: %v", err)
	}
	chars := utf8.RuneCountInString(req.GetText())
	cost := voice.CostUSD(req.GetVoiceId(), chars)
	if s.deps.VoiceFake {
		cost = 0
	}
	_ = s.deps.Ledger.Append(spend.Line{TS: time.Now(), Kind: "tts", Provider: providerName(s.deps.VoiceFake, "google"), Label: label, Chars: chars, CostUSD: cost})
	return &radiolabv1.SynthesizeVoiceResponse{Artifact: artifactToProto(a), CostUsd: cost, Fake: s.deps.VoiceFake}, nil
}

func (s *Server) ListArtifacts(_ context.Context, req *radiolabv1.ListArtifactsRequest) (*radiolabv1.ListArtifactsResponse, error) {
	all, err := s.deps.Store.List()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list: %v", err)
	}
	resp := &radiolabv1.ListArtifactsResponse{}
	for _, a := range all {
		if req.GetKind() == "" || a.Kind == req.GetKind() {
			resp.Artifacts = append(resp.Artifacts, artifactToProto(a))
		}
	}
	return resp, nil
}

func providerName(fake bool, real string) string {
	if fake {
		return "fake"
	}
	return real
}
