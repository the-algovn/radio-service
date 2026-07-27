package audit

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/the-algovn/radio-service/internal/db"
)

// PGStore stores audit rows in Postgres via sqlc, with prune-on-write
// retention (the ledger's unbounded-growth mistake is not repeated here).
type PGStore struct {
	pool      *pgxpool.Pool
	retention time.Duration
}

func NewPGStore(pool *pgxpool.Pool, retention time.Duration) *PGStore {
	return &PGStore{pool: pool, retention: retention}
}

func (s *PGStore) Record(ctx context.Context, r Rec) error {
	q := db.New(s.pool)
	if err := q.InsertLLMCall(ctx, db.InsertLLMCallParams{
		Ts: r.TS, Label: r.Label, Model: r.Model, Provider: r.Provider,
		SystemPrompt: r.System, UserPrompt: r.User, Output: r.Output,
		InTokens: int32(r.InTokens), OutTokens: int32(r.OutTokens), CostUsd: r.CostUSD,
		LatencyMs: int32(r.LatencyMS), Error: r.Error, Fake: r.Fake,
	}); err != nil {
		return err
	}
	// Best-effort prune; a failure never fails the record (listeners pattern).
	if s.retention > 0 {
		_ = q.PruneLLMCalls(ctx, time.Now().Add(-s.retention))
	}
	return nil
}

func (s *PGStore) List(ctx context.Context, f Filter, limit, offset int) ([]Rec, error) {
	rows, err := db.New(s.pool).ListLLMCalls(ctx, db.ListLLMCallsParams{
		LabelFilter: f.Label, ErrorsOnly: f.ErrorsOnly, Lim: int32(limit), Off: int32(offset),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Rec, 0, len(rows))
	for _, r := range rows {
		out = append(out, Rec{
			ID: r.ID, TS: r.Ts, Label: r.Label, Model: r.Model, Provider: r.Provider,
			System: r.SystemPrompt, User: r.UserPrompt, Output: r.Output,
			InTokens: int(r.InTokens), OutTokens: int(r.OutTokens), CostUSD: r.CostUsd,
			LatencyMS: int(r.LatencyMs), Error: r.Error, Fake: r.Fake,
		})
	}
	return out, nil
}

func (s *PGStore) Count(ctx context.Context, f Filter) (int64, error) {
	return db.New(s.pool).CountLLMCalls(ctx, db.CountLLMCallsParams{
		LabelFilter: f.Label, ErrorsOnly: f.ErrorsOnly,
	})
}

func (s *PGStore) Stats(ctx context.Context, since time.Time) ([]Stat, error) {
	rows, err := db.New(s.pool).StatsLLMCalls(ctx, since)
	if err != nil {
		return nil, err
	}
	out := make([]Stat, 0, len(rows))
	for _, r := range rows {
		out = append(out, Stat{
			Label: r.Label, Model: r.Model, Count: int(r.N),
			InTokens: int(r.InTokens), OutTokens: int(r.OutTokens), CostUSD: r.CostUsd,
		})
	}
	return out, nil
}
