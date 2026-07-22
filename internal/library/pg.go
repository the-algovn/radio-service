package library

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/the-algovn/radio-service/internal/db"
)

// PGLibrary stores tracks in Postgres via sqlc.
type PGLibrary struct{ pool *pgxpool.Pool }

func NewPGLibrary(pool *pgxpool.Pool) *PGLibrary { return &PGLibrary{pool: pool} }

func (l *PGLibrary) Get(ctx context.Context, ytID string) (Track, bool, error) {
	row, err := db.New(l.pool).GetTrack(ctx, ytID)
	if errors.Is(err, sql.ErrNoRows) {
		return Track{}, false, nil
	}
	if err != nil {
		return Track{}, false, err
	}
	return trackFromRow(row), true, nil
}

func (l *PGLibrary) Add(ctx context.Context, t Track) error {
	return db.New(l.pool).InsertTrack(ctx, db.InsertTrackParams{
		YtID: t.YTID, Title: t.Title, Channel: t.Channel, DurationS: t.DurationS,
		ArtifactID: t.ArtifactID, InputI: t.InputI, InputTp: t.InputTP, InputLra: t.InputLRA,
	})
}

func (l *PGLibrary) List(ctx context.Context, query string, limit, offset int) ([]Track, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.New(l.pool).ListTracks(ctx, db.ListTracksParams{
		Column1: query, Limit: int32(limit), Offset: int32(offset),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Track, 0, len(rows))
	for _, r := range rows {
		out = append(out, trackFromRow(r))
	}
	return out, nil
}

func (l *PGLibrary) Count(ctx context.Context, query string) (int64, error) {
	return db.New(l.pool).CountTracks(ctx, query)
}

func (l *PGLibrary) Delete(ctx context.Context, ytID string) (string, bool, error) {
	artifactID, err := db.New(l.pool).DeleteTrack(ctx, ytID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return artifactID, true, nil
}

func (l *PGLibrary) AllIDs(ctx context.Context) ([]string, error) {
	return db.New(l.pool).AllTrackIDs(ctx)
}

func trackFromRow(r db.Track) Track {
	return Track{
		YTID: r.YtID, Title: r.Title, Channel: r.Channel, ArtifactID: r.ArtifactID,
		DurationS: r.DurationS, InputI: r.InputI, InputTP: r.InputTp, InputLRA: r.InputLra,
		AddedAt: r.AddedAt,
	}
}
