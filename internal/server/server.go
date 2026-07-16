// Package server implements algovn.radiolab.v1.LabService over the lab's
// internal packages. Deps grows one field per bench task.
package server

import (
	"context"
	"time"

	radiolabv1 "github.com/the-algovn/protos/gen/go/algovn/radiolab/v1"
	"github.com/the-algovn/radio-service/internal/spend"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Deps struct {
	Ledger *spend.Ledger
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
