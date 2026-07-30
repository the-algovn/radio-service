package talkmem

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/the-algovn/radio-service/internal/db"
)

// PGStore persists show memory with prune-on-write retention, the same
// pattern internal/audit uses (the ledger's unbounded growth is not repeated).
type PGStore struct {
	pool      *pgxpool.Pool
	retention time.Duration
}

// NewPGStore builds the store. retention <= 0 disables pruning.
func NewPGStore(pool *pgxpool.Pool, retention time.Duration) *PGStore {
	return &PGStore{pool: pool, retention: retention}
}

func (s *PGStore) Append(ctx context.Context, e Entry) error {
	q := db.New(s.pool)
	phrases := e.Phrases
	if phrases == nil {
		phrases = []string{} // NOT NULL column; nil would violate it
	}
	if err := q.InsertTalkMemory(ctx, db.InsertTalkMemoryParams{
		Kind: e.Kind, Summary: e.Summary, Phrases: phrases,
	}); err != nil {
		return err
	}
	// Best-effort prune; a failure never fails the append (audit's pattern).
	if s.retention > 0 {
		_ = q.PruneTalkMemory(ctx, time.Now().Add(-s.retention))
	}
	return nil
}

func (s *PGStore) Recent(ctx context.Context, since time.Time, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 8
	}
	rows, err := db.New(s.pool).RecentTalkMemory(ctx, db.RecentTalkMemoryParams{
		CreatedAt: since, Lim: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	// The query is newest-first for the LIMIT to keep the newest n; reverse
	// into narrative order.
	out := make([]Entry, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		out = append(out, Entry{Kind: r.Kind, Summary: r.Summary,
			Phrases: r.Phrases, CreatedAt: r.CreatedAt})
	}
	return out, nil
}
