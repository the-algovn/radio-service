package station

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/the-algovn/radio-service/internal/db"
)

// PGStore stores station state on the singleton station row via sqlc.
// GoOnAir takes SELECT … FOR UPDATE on that row inside a transaction (the
// same LockStationRow pattern the playlist-era station store used, before
// it was removed in v1.2) so the OnAirSince anchor is preserved on
// idempotent calls without a read-then-write race.
type PGStore struct{ pool *pgxpool.Pool }

func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

func (s *PGStore) withTx(ctx context.Context, fn func(q *db.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(db.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PGStore) GetStation(ctx context.Context) (Station, error) {
	row, err := db.New(s.pool).GetStationRow(ctx)
	if err != nil {
		return Station{}, err
	}
	return Station{OnAir: row.OnAir, OnAirSince: row.OnAirSince, AIEnabled: row.AiEnabled}, nil
}

func (s *PGStore) GoOnAir(ctx context.Context) (Station, error) {
	var out Station
	err := s.withTx(ctx, func(q *db.Queries) error {
		st, err := q.LockStationRow(ctx)
		if err != nil {
			return err
		}
		if st.OnAir { // idempotent — preserve the anchor
			out = Station{OnAir: true, OnAirSince: st.OnAirSince, AIEnabled: st.AiEnabled}
			return nil
		}
		row, err := q.StationGoOnAir(ctx)
		if err != nil {
			return err
		}
		out = Station{OnAir: row.OnAir, OnAirSince: row.OnAirSince, AIEnabled: row.AiEnabled}
		return nil
	})
	return out, err
}

func (s *PGStore) GoOffAir(ctx context.Context) (Station, error) {
	row, err := db.New(s.pool).StationGoOffAir(ctx)
	if err != nil {
		return Station{}, err
	}
	return Station{OnAir: row.OnAir, OnAirSince: row.OnAirSince, AIEnabled: row.AiEnabled}, nil
}

func (s *PGStore) SetAIEnabled(ctx context.Context, enabled bool) (Station, error) {
	row, err := db.New(s.pool).SetStationAIEnabled(ctx, enabled)
	if err != nil {
		return Station{}, err
	}
	return Station{OnAir: row.OnAir, OnAirSince: row.OnAirSince, AIEnabled: row.AiEnabled}, nil
}
