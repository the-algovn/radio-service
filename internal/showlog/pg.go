package showlog

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/the-algovn/radio-service/internal/db"
)

// PGStore persists the show log with prune-on-write retention, the pattern
// internal/audit and internal/talkmem both use.
type PGStore struct {
	pool      *pgxpool.Pool
	retention time.Duration
}

// NewPGStore builds the store. retention <= 0 disables pruning.
//
// Callers should pass the SAME retention the audit store uses
// (LLM_AUDIT_RETENTION_DAYS), not a hardcoded constant: provenance is read
// back as a join against llm_call, so a segment outliving its call would
// silently lose its model, tokens and cost.
func NewPGStore(pool *pgxpool.Pool, retention time.Duration) *PGStore {
	return &PGStore{pool: pool, retention: retention}
}

func (s *PGStore) Append(ctx context.Context, t Talk) error {
	q := db.New(s.pool)
	if err := q.InsertTalkSegment(ctx, db.InsertTalkSegmentParams{
		Kind: t.Kind, StartedAt: t.StartedAt, DurationS: int32(t.DurationS),
		Script: t.Script, BacksellTitle: t.BacksellTitle,
		PromiseTitle: t.PromiseTitle, CorrelationID: t.CorrelationID,
	}); err != nil {
		return err
	}
	// Best-effort prune; a failure never fails the append (audit's pattern).
	if s.retention > 0 {
		_ = q.PruneTalkSegments(ctx, time.Now().Add(-s.retention))
	}
	return nil
}

func (s *PGStore) Recent(ctx context.Context, limit, offset int) ([]Segment, error) {
	if offset < 0 {
		offset = 0
	}
	rows, err := db.New(s.pool).RecentShowSegments(ctx, db.RecentShowSegmentsParams{
		Lim: int32(clampLimit(limit)), Off: int32(offset),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Segment, 0, len(rows))
	for _, r := range rows {
		out = append(out, Segment{
			ID: r.ID, Origin: r.Origin, Kind: r.Kind, YTID: r.YtID,
			Title: r.Title, Artist: r.Artist, StartedAt: r.StartedAt,
			DurationS: int(r.DurationS), Source: r.Source,
			RequestedByName: r.RequestedByName, Reason: r.Reason,
			Script: r.Script, BacksellTitle: r.BacksellTitle,
			PromiseTitle: r.PromiseTitle, CorrelationID: r.CorrelationID,
			Model: r.Model, InTokens: int(r.InTokens), OutTokens: int(r.OutTokens),
			CostUSD: r.CostUsd, LatencyMS: int(r.LatencyMs),
		})
	}
	return out, nil
}

func (s *PGStore) Count(ctx context.Context) (int64, error) {
	return db.New(s.pool).CountShowSegments(ctx)
}
