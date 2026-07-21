package live

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/the-algovn/radio-service/internal/db"
)

// PGAirLog stores the air log in Postgres via sqlc.
type PGAirLog struct{ pool *pgxpool.Pool }

func NewPGAirLog(pool *pgxpool.Pool) *PGAirLog { return &PGAirLog{pool: pool} }

func (l *PGAirLog) Append(ctx context.Context, e Entry) error {
	return db.New(l.pool).AppendAirLog(ctx, db.AppendAirLogParams{
		YtID: e.YTID, Title: e.Title, Artist: e.Artist,
		StartedAt: e.StartedAt, DurationS: int32(e.DurationS),
	})
}

func (l *PGAirLog) Latest(ctx context.Context) (Entry, bool, error) {
	row, err := db.New(l.pool).LatestAirLog(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	return Entry{YTID: row.YtID, Title: row.Title, Artist: row.Artist,
		StartedAt: row.StartedAt, DurationS: int(row.DurationS)}, true, nil
}

func (l *PGAirLog) History(ctx context.Context, limit int) ([]Entry, error) {
	rows, err := db.New(l.pool).AirHistory(ctx, int32(limit))
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(rows))
	for _, r := range rows {
		out = append(out, Entry{YTID: r.YtID, Title: r.Title, Artist: r.Artist,
			StartedAt: r.StartedAt, DurationS: int(r.DurationS)})
	}
	return out, nil
}

// PGListeners stores heartbeats in Postgres; Beat opportunistically prunes.
type PGListeners struct{ pool *pgxpool.Pool }

func NewPGListeners(pool *pgxpool.Pool) *PGListeners { return &PGListeners{pool: pool} }

func (l *PGListeners) Beat(ctx context.Context, sessionID string) error {
	q := db.New(l.pool)
	if err := q.BeatListener(ctx, sessionID); err != nil {
		return err
	}
	// Best-effort cleanup; a failure never breaks the heartbeat.
	_ = q.PruneListeners(ctx)
	return nil
}

func (l *PGListeners) Count(ctx context.Context) (int, error) {
	n, err := db.New(l.pool).CountListeners(ctx)
	return int(n), err
}
