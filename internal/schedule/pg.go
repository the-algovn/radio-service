package schedule

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/the-algovn/radio-service/internal/db"
)

// PGStore persists the committed next track on a singleton row (id = TRUE);
// empty yt_id means "none committed".
type PGStore struct{ pool *pgxpool.Pool }

func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

func (s *PGStore) GetNextUp(ctx context.Context) (NextUp, bool, error) {
	row, err := db.New(s.pool).GetNextUp(ctx)
	if err != nil {
		return NextUp{}, false, err
	}
	if row.YtID == "" {
		return NextUp{}, false, nil
	}
	return NextUp{YTID: row.YtID, Title: row.Title, Channel: row.Channel}, true, nil
}

func (s *PGStore) SetNextUp(ctx context.Context, n NextUp) error {
	return db.New(s.pool).SetNextUp(ctx, db.SetNextUpParams{
		YtID: n.YTID, Title: n.Title, Channel: n.Channel,
	})
}

func (s *PGStore) ClearNextUp(ctx context.Context) error {
	return db.New(s.pool).ClearNextUp(ctx)
}
