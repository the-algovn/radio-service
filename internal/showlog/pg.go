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
