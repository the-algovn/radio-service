package request

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/the-algovn/radio-service/internal/db"
)

// PGStore stores requests in Postgres via sqlc. Single-replica service —
// no row locking needed; every mutation is a single-row UPDATE.
type PGStore struct{ pool *pgxpool.Pool }

func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

func notFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// itemOf converts any of the generated request-row structs (identical
// field sets) into an Item. sqlc emits one struct per query, so each call
// site passes the fields explicitly through this one constructor.
func itemOf(id, source, requestedBy, displayName, ytID, title, channel string,
	durationS int64, thumbnailURL, status, failReason string, attempts int32,
	createdAt time.Time, airedAt *time.Time, reason string) Item {
	return Item{
		ID: id, Source: source, RequestedBy: requestedBy, DisplayName: displayName,
		YTID: ytID, Title: title, Channel: channel, DurationS: durationS,
		ThumbnailURL: thumbnailURL, Status: status, FailReason: failReason,
		Attempts: int(attempts), CreatedAt: createdAt, AiredAt: airedAt,
		Reason: reason,
	}
}

func (s *PGStore) Create(ctx context.Context, it Item) (Item, error) {
	r, err := db.New(s.pool).CreateRequest(ctx, db.CreateRequestParams{
		Source: it.Source, RequestedBy: it.RequestedBy, DisplayName: it.DisplayName,
		YtID: it.YTID, Title: it.Title, Channel: it.Channel,
		DurationS: it.DurationS, ThumbnailUrl: it.ThumbnailURL, Status: it.Status,
		Reason: it.Reason,
	})
	if err != nil {
		return Item{}, err
	}
	return itemOf(r.ID, r.Source, r.RequestedBy, r.DisplayName, r.YtID, r.Title,
		r.Channel, r.DurationS, r.ThumbnailUrl, r.Status, r.FailReason,
		r.Attempts, r.CreatedAt, r.AiredAt, r.Reason), nil
}

func (s *PGStore) NextReady(ctx context.Context) (Item, bool, error) {
	r, err := db.New(s.pool).NextReadyRequest(ctx)
	if notFound(err) {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, err
	}
	return itemOf(r.ID, r.Source, r.RequestedBy, r.DisplayName, r.YtID, r.Title,
		r.Channel, r.DurationS, r.ThumbnailUrl, r.Status, r.FailReason,
		r.Attempts, r.CreatedAt, r.AiredAt, r.Reason), true, nil
}

func (s *PGStore) OldestApproved(ctx context.Context) (Item, bool, error) {
	r, err := db.New(s.pool).OldestApprovedRequest(ctx)
	if notFound(err) {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, err
	}
	return itemOf(r.ID, r.Source, r.RequestedBy, r.DisplayName, r.YtID, r.Title,
		r.Channel, r.DurationS, r.ThumbnailUrl, r.Status, r.FailReason,
		r.Attempts, r.CreatedAt, r.AiredAt, r.Reason), true, nil
}

func (s *PGStore) Pending(ctx context.Context) ([]Item, error) {
	rows, err := db.New(s.pool).PendingRequests(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(rows))
	for _, r := range rows {
		out = append(out, itemOf(r.ID, r.Source, r.RequestedBy, r.DisplayName,
			r.YtID, r.Title, r.Channel, r.DurationS, r.ThumbnailUrl, r.Status,
			r.FailReason, r.Attempts, r.CreatedAt, r.AiredAt, r.Reason))
	}
	return out, nil
}

func (s *PGStore) ByUser(ctx context.Context, sub string, limit int) ([]Item, error) {
	rows, err := db.New(s.pool).RequestsByUser(ctx, db.RequestsByUserParams{
		RequestedBy: sub, Limit: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(rows))
	for _, r := range rows {
		out = append(out, itemOf(r.ID, r.Source, r.RequestedBy, r.DisplayName,
			r.YtID, r.Title, r.Channel, r.DurationS, r.ThumbnailUrl, r.Status,
			r.FailReason, r.Attempts, r.CreatedAt, r.AiredAt, r.Reason))
	}
	return out, nil
}

func (s *PGStore) CountPendingByUser(ctx context.Context, sub string) (int, error) {
	n, err := db.New(s.pool).CountPendingByUser(ctx, sub)
	return int(n), err
}

func (s *PGStore) CountSince(ctx context.Context, sub string, since time.Time) (int, error) {
	n, err := db.New(s.pool).CountRequestsSince(ctx, db.CountRequestsSinceParams{
		RequestedBy: sub, CreatedAt: since,
	})
	return int(n), err
}

func (s *PGStore) HasPendingYTID(ctx context.Context, ytID string) (bool, error) {
	return db.New(s.pool).HasPendingYTID(ctx, ytID)
}

func (s *PGStore) MarkReady(ctx context.Context, id string) error {
	n, err := db.New(s.pool).MarkRequestReady(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PGStore) MarkAired(ctx context.Context, id string, at time.Time) error {
	n, err := db.New(s.pool).MarkRequestAired(ctx, db.MarkRequestAiredParams{ID: id, AiredAt: &at})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PGStore) MarkFailed(ctx context.Context, id, reason string) error {
	n, err := db.New(s.pool).MarkRequestFailed(ctx, db.MarkRequestFailedParams{ID: id, FailReason: reason})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PGStore) BumpAttempts(ctx context.Context, id, reason string) (int, error) {
	n, err := db.New(s.pool).BumpRequestAttempts(ctx, db.BumpRequestAttemptsParams{ID: id, FailReason: reason})
	if notFound(err) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return int(n), nil
}
