package broadcast

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/the-algovn/radio-service/internal/db"
)

// PGStore persists broadcast sessions. No retention: one row per broadcast is
// a handful per day, and losing an old boundary would silently misdraw the
// console's dividers.
type PGStore struct{ pool *pgxpool.Pool }

func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

func (s *PGStore) Open(ctx context.Context, at time.Time) error {
	return db.New(s.pool).OpenBroadcastSession(ctx, at)
}

func (s *PGStore) Close(ctx context.Context, at time.Time) error {
	// The generated query takes *time.Time because sqlc overrides nullable
	// timestamptz; &at is the address of this parameter's own copy.
	return db.New(s.pool).CloseOpenBroadcastSessions(ctx, &at)
}

func (s *PGStore) CloseOrphans(ctx context.Context, at time.Time) (int64, error) {
	return db.New(s.pool).CloseOrphanBroadcastSessions(ctx, &at)
}

func (s *PGStore) Overlapping(ctx context.Context, from, to time.Time, limit int) ([]Session, error) {
	if limit <= 0 {
		limit = defaultLimit
	}
	rows, err := db.New(s.pool).OverlappingBroadcastSessions(ctx,
		db.OverlappingBroadcastSessionsParams{FromTs: &from, ToTs: to, Lim: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(rows))
	for _, r := range rows {
		out = append(out, Session{ID: r.ID, StartedAt: r.StartedAt, EndedAt: r.EndedAt})
	}
	return out, nil
}
