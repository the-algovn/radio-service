package playlist

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/the-algovn/radio-service/internal/db"
)

// PGStore stores playlists and station state in Postgres via sqlc. Guarded
// mutations open a transaction and take SELECT … FOR UPDATE on the singleton
// station row, serializing station-affecting operations across operators.
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

func notFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// summaryOf builds a Summary for one playlist row using q (pool or tx).
func summaryOf(ctx context.Context, q *db.Queries, row db.GetPlaylistRowRow, activeID string) (Summary, error) {
	stats, err := q.PlaylistStats(ctx, row.ID)
	if err != nil {
		return Summary{}, err
	}
	return Summary{
		ID: row.ID, Name: row.Name, TrackCount: int(stats.TrackCount),
		TotalDurationS: stats.TotalDurationS, IsActive: row.ID == activeID,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

func itemsOf(ctx context.Context, q *db.Queries, playlistID string) ([]Item, error) {
	rows, err := q.ListPlaylistItems(ctx, playlistID)
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(rows))
	for _, r := range rows {
		out = append(out, Item{
			Position: int(r.Position), YTID: r.YtID, Title: r.Title,
			Channel: r.Channel, DurationS: r.DurationS,
		})
	}
	return out, nil
}

// stationOf projects the station row plus active-playlist metadata.
func stationOf(ctx context.Context, q *db.Queries, activeID string, onAir bool, since *time.Time) (Station, error) {
	st := Station{ActivePlaylistID: activeID, OnAir: onAir, OnAirSince: since}
	if activeID == "" {
		return st, nil
	}
	row, err := q.GetPlaylistRow(ctx, activeID)
	if err != nil {
		if notFound(err) { // pointer raced a delete; treat as unset
			st.ActivePlaylistID = ""
			return st, nil
		}
		return Station{}, err
	}
	n, err := q.CountPlaylistItems(ctx, activeID)
	if err != nil {
		return Station{}, err
	}
	st.ActivePlaylistName, st.ActiveTrackCount = row.Name, int(n)
	return st, nil
}

func (s *PGStore) Create(ctx context.Context, name string) (Summary, error) {
	row, err := db.New(s.pool).CreatePlaylist(ctx, name)
	if err != nil {
		return Summary{}, err
	}
	return Summary{ID: row.ID, Name: row.Name, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}, nil
}

func (s *PGStore) List(ctx context.Context) ([]Summary, error) {
	q := db.New(s.pool)
	st, err := q.GetStationRow(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.ListPlaylistRows(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(rows))
	for _, r := range rows {
		out = append(out, Summary{
			ID: r.ID, Name: r.Name, TrackCount: int(r.TrackCount),
			TotalDurationS: r.TotalDurationS, IsActive: r.ID == st.ActivePlaylistID,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	return out, nil
}

func (s *PGStore) Get(ctx context.Context, id string) (Summary, []Item, error) {
	return s.fetch(ctx, db.New(s.pool), id)
}

func (s *PGStore) fetch(ctx context.Context, q *db.Queries, id string) (Summary, []Item, error) {
	row, err := q.GetPlaylistRow(ctx, id)
	if notFound(err) {
		return Summary{}, nil, ErrNotFound
	}
	if err != nil {
		return Summary{}, nil, err
	}
	st, err := q.GetStationRow(ctx)
	if err != nil {
		return Summary{}, nil, err
	}
	sum, err := summaryOf(ctx, q, row, st.ActivePlaylistID)
	if err != nil {
		return Summary{}, nil, err
	}
	items, err := itemsOf(ctx, q, id)
	return sum, items, err
}

func (s *PGStore) Rename(ctx context.Context, id, name string) (Summary, error) {
	q := db.New(s.pool)
	row, err := q.RenamePlaylist(ctx, db.RenamePlaylistParams{ID: id, Name: name})
	if notFound(err) {
		return Summary{}, ErrNotFound
	}
	if err != nil {
		return Summary{}, err
	}
	st, err := q.GetStationRow(ctx)
	if err != nil {
		return Summary{}, err
	}
	return summaryOf(ctx, q, db.GetPlaylistRowRow(row), st.ActivePlaylistID)
}

func (s *PGStore) Delete(ctx context.Context, id string) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		st, err := q.LockStationRow(ctx)
		if err != nil {
			return err
		}
		if st.OnAir && st.ActivePlaylistID == id {
			return ErrActiveOnAir
		}
		n, err := q.DeletePlaylist(ctx, id)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func (s *PGStore) AddTrack(ctx context.Context, playlistID, ytID string) (Summary, []Item, error) {
	var sum Summary
	var items []Item
	err := s.withTx(ctx, func(q *db.Queries) error {
		if _, err := q.GetPlaylistRow(ctx, playlistID); err != nil {
			if notFound(err) {
				return ErrNotFound
			}
			return err
		}
		if _, err := q.GetTrack(ctx, ytID); err != nil {
			if notFound(err) {
				return ErrNotFound
			}
			return err
		}
		// 0 rows = duplicate — idempotent no-op by design.
		if _, err := q.AppendPlaylistItem(ctx, db.AppendPlaylistItemParams{PlaylistID: playlistID, YtID: ytID}); err != nil {
			return err
		}
		var err error
		sum, items, err = s.fetch(ctx, q, playlistID)
		return err
	})
	return sum, items, err
}

func (s *PGStore) RemoveTrack(ctx context.Context, playlistID, ytID string) (Summary, []Item, error) {
	var sum Summary
	var items []Item
	err := s.withTx(ctx, func(q *db.Queries) error {
		st, err := q.LockStationRow(ctx)
		if err != nil {
			return err
		}
		if _, err := q.GetPlaylistRow(ctx, playlistID); err != nil {
			if notFound(err) {
				return ErrNotFound
			}
			return err
		}
		if st.OnAir && st.ActivePlaylistID == playlistID {
			n, err := q.CountPlaylistItems(ctx, playlistID)
			if err != nil {
				return err
			}
			if n <= 1 {
				return ErrActiveOnAir
			}
		}
		n, err := q.DeletePlaylistItem(ctx, db.DeletePlaylistItemParams{PlaylistID: playlistID, YtID: ytID})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		sum, items, err = s.fetch(ctx, q, playlistID)
		return err
	})
	return sum, items, err
}

func (s *PGStore) Reorder(ctx context.Context, playlistID string, ytIDs []string) (Summary, []Item, error) {
	var sum Summary
	var items []Item
	err := s.withTx(ctx, func(q *db.Queries) error {
		if _, err := q.GetPlaylistRow(ctx, playlistID); err != nil {
			if notFound(err) {
				return ErrNotFound
			}
			return err
		}
		current, err := q.ListPlaylistItemIDs(ctx, playlistID)
		if err != nil {
			return err
		}
		if len(current) != len(ytIDs) {
			return ErrStale
		}
		have := map[string]bool{}
		for _, yt := range current {
			have[yt] = true
		}
		used := map[string]bool{}
		for _, yt := range ytIDs {
			if !have[yt] || used[yt] { // unknown or duplicated id — stale/corrupt list
				return ErrStale
			}
			used[yt] = true
		}
		if err := q.DeleteAllPlaylistItems(ctx, playlistID); err != nil {
			return err
		}
		for i, yt := range ytIDs {
			if err := q.InsertPlaylistItemAt(ctx, db.InsertPlaylistItemAtParams{
				PlaylistID: playlistID, Position: int32(i), YtID: yt,
			}); err != nil {
				return err
			}
		}
		sum, items, err = s.fetch(ctx, q, playlistID)
		return err
	})
	return sum, items, err
}

func (s *PGStore) GetStation(ctx context.Context) (Station, error) {
	q := db.New(s.pool)
	row, err := q.GetStationRow(ctx)
	if err != nil {
		return Station{}, err
	}
	return stationOf(ctx, q, row.ActivePlaylistID, row.OnAir, row.OnAirSince)
}

func (s *PGStore) SetActive(ctx context.Context, playlistID string) (Station, error) {
	var out Station
	err := s.withTx(ctx, func(q *db.Queries) error {
		st, err := q.LockStationRow(ctx)
		if err != nil {
			return err
		}
		if _, err := q.GetPlaylistRow(ctx, playlistID); err != nil {
			if notFound(err) {
				return ErrNotFound
			}
			return err
		}
		n, err := q.CountPlaylistItems(ctx, playlistID)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrEmptyPlaylist
		}
		if err := q.SetStationActive(ctx, playlistID); err != nil {
			return err
		}
		out, err = stationOf(ctx, q, playlistID, st.OnAir, st.OnAirSince)
		return err
	})
	return out, err
}

func (s *PGStore) GoOnAir(ctx context.Context) (Station, error) {
	var out Station
	err := s.withTx(ctx, func(q *db.Queries) error {
		st, err := q.LockStationRow(ctx)
		if err != nil {
			return err
		}
		if st.OnAir { // idempotent — preserve the anchor
			out, err = stationOf(ctx, q, st.ActivePlaylistID, true, st.OnAirSince)
			return err
		}
		if st.ActivePlaylistID == "" {
			return ErrNoActivePlaylist
		}
		n, err := q.CountPlaylistItems(ctx, st.ActivePlaylistID)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrEmptyPlaylist
		}
		row, err := q.StationGoOnAir(ctx)
		if err != nil {
			return err
		}
		out, err = stationOf(ctx, q, row.ActivePlaylistID, row.OnAir, row.OnAirSince)
		return err
	})
	return out, err
}

func (s *PGStore) GoOffAir(ctx context.Context) (Station, error) {
	var out Station
	err := s.withTx(ctx, func(q *db.Queries) error {
		if _, err := q.LockStationRow(ctx); err != nil {
			return err
		}
		row, err := q.StationGoOffAir(ctx)
		if err != nil {
			return err
		}
		out, err = stationOf(ctx, q, row.ActivePlaylistID, row.OnAir, row.OnAirSince)
		return err
	})
	return out, err
}
