package request_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/request"
)

type storeFactory func(t *testing.T) request.Store

// mk creates a request with sane defaults; overrides via the args.
func mk(source, sub, ytID, status string) request.Item {
	return request.Item{
		Source: source, RequestedBy: sub, DisplayName: "tên-" + sub,
		YTID: ytID, Title: "t-" + ytID, Channel: "c-" + ytID,
		DurationS: 240, ThumbnailURL: "https://img/" + ytID, Status: status,
		Reason: "lý-do-" + ytID,
	}
}

func runStoreContract(t *testing.T, newStore storeFactory) {
	ctx := context.Background()

	t.Run("create fills id and created_at, round-trips every field", func(t *testing.T) {
		s := newStore(t)
		it, err := s.Create(ctx, mk(request.SourceListener, "sub-1", "yta", request.StatusApproved))
		require.NoError(t, err)
		require.NotEmpty(t, it.ID)
		require.False(t, it.CreatedAt.IsZero())
		require.Equal(t, request.SourceListener, it.Source)
		require.Equal(t, "sub-1", it.RequestedBy)
		require.Equal(t, "tên-sub-1", it.DisplayName)
		require.Equal(t, "yta", it.YTID)
		require.Equal(t, "t-yta", it.Title)
		require.Equal(t, "c-yta", it.Channel)
		require.Equal(t, int64(240), it.DurationS)
		require.Equal(t, "https://img/yta", it.ThumbnailURL)
		require.Equal(t, request.StatusApproved, it.Status)
		require.Zero(t, it.Attempts)
		require.Nil(t, it.AiredAt)
		require.Equal(t, "lý-do-yta", it.Reason)
	})

	t.Run("air order: ready listener before ready ai, FIFO within source", func(t *testing.T) {
		s := newStore(t)
		ai1, err := s.Create(ctx, mk(request.SourceAI, "", "ai1", request.StatusReady))
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond) // created_at tiebreak
		l1, err := s.Create(ctx, mk(request.SourceListener, "u1", "l1", request.StatusReady))
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
		l2, err := s.Create(ctx, mk(request.SourceListener, "u2", "l2", request.StatusReady))
		require.NoError(t, err)

		next, found, err := s.NextReady(ctx)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, l1.ID, next.ID) // listener beats older AI pick

		pending, err := s.Pending(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{l1.ID, l2.ID, ai1.ID},
			[]string{pending[0].ID, pending[1].ID, pending[2].ID})
	})

	t.Run("approved items are pending but never NextReady", func(t *testing.T) {
		s := newStore(t)
		_, err := s.Create(ctx, mk(request.SourceListener, "u1", "l1", request.StatusApproved))
		require.NoError(t, err)
		_, found, err := s.NextReady(ctx)
		require.NoError(t, err)
		require.False(t, found)
		pending, err := s.Pending(ctx)
		require.NoError(t, err)
		require.Len(t, pending, 1)
	})

	t.Run("worker path: oldest approved, ready, bump, failed", func(t *testing.T) {
		s := newStore(t)
		a, err := s.Create(ctx, mk(request.SourceListener, "u1", "a", request.StatusApproved))
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
		_, err = s.Create(ctx, mk(request.SourceAI, "", "b", request.StatusApproved))
		require.NoError(t, err)

		got, found, err := s.OldestApproved(ctx)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, a.ID, got.ID)

		n, err := s.BumpAttempts(ctx, a.ID, "yt-dlp: 403")
		require.NoError(t, err)
		require.Equal(t, 1, n)
		n, err = s.BumpAttempts(ctx, a.ID, "yt-dlp: timeout")
		require.NoError(t, err)
		require.Equal(t, 2, n)

		require.NoError(t, s.MarkReady(ctx, a.ID))
		next, found, err := s.NextReady(ctx)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, a.ID, next.ID)
		require.Equal(t, 2, next.Attempts)
		require.Equal(t, "yt-dlp: timeout", next.FailReason)

		// ready is no longer approved: MarkReady again → ErrNotFound
		require.ErrorIs(t, s.MarkReady(ctx, a.ID), request.ErrNotFound)

		require.NoError(t, s.MarkFailed(ctx, next.ID, "artifact failed to open"))
		_, found, err = s.NextReady(ctx)
		require.NoError(t, err)
		require.False(t, found)
	})

	t.Run("aired leaves the queue and stamps aired_at", func(t *testing.T) {
		s := newStore(t)
		it, err := s.Create(ctx, mk(request.SourceListener, "u1", "a", request.StatusReady))
		require.NoError(t, err)
		at := time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
		require.NoError(t, s.MarkAired(ctx, it.ID, at))
		pending, err := s.Pending(ctx)
		require.NoError(t, err)
		require.Empty(t, pending)
		mine, err := s.ByUser(ctx, "u1", 10)
		require.NoError(t, err)
		require.Len(t, mine, 1)
		require.Equal(t, request.StatusAired, mine[0].Status)
		require.NotNil(t, mine[0].AiredAt)
		require.WithinDuration(t, at, *mine[0].AiredAt, time.Second)
	})

	t.Run("quota reads: pending count, daily count, dup guard", func(t *testing.T) {
		s := newStore(t)
		_, err := s.Create(ctx, mk(request.SourceListener, "u1", "a", request.StatusApproved))
		require.NoError(t, err)
		it, err := s.Create(ctx, mk(request.SourceListener, "u1", "b", request.StatusReady))
		require.NoError(t, err)
		_, err = s.Create(ctx, mk(request.SourceListener, "u2", "c", request.StatusReady))
		require.NoError(t, err)
		require.NoError(t, s.MarkAired(ctx, it.ID, time.Now()))

		n, err := s.CountPendingByUser(ctx, "u1")
		require.NoError(t, err)
		require.Equal(t, 1, n) // a approved; b aired no longer pending

		total, err := s.CountSince(ctx, "u1", time.Now().Add(-time.Hour))
		require.NoError(t, err)
		require.Equal(t, 2, total) // aired still counts toward the daily quota
		zero, err := s.CountSince(ctx, "u1", time.Now().Add(time.Hour))
		require.NoError(t, err)
		require.Zero(t, zero)

		has, err := s.HasPendingYTID(ctx, "a")
		require.NoError(t, err)
		require.True(t, has)
		has, err = s.HasPendingYTID(ctx, "b") // aired → not pending
		require.NoError(t, err)
		require.False(t, has)
	})

	t.Run("ByUser newest first with cap; unknown ids → ErrNotFound", func(t *testing.T) {
		s := newStore(t)
		_, err := s.Create(ctx, mk(request.SourceListener, "u1", "a", request.StatusApproved))
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
		b, err := s.Create(ctx, mk(request.SourceListener, "u1", "b", request.StatusApproved))
		require.NoError(t, err)

		mine, err := s.ByUser(ctx, "u1", 1)
		require.NoError(t, err)
		require.Len(t, mine, 1)
		require.Equal(t, b.ID, mine[0].ID)

		const missing = "00000000-0000-0000-0000-000000000000"
		require.ErrorIs(t, s.MarkReady(ctx, missing), request.ErrNotFound)
		require.ErrorIs(t, s.MarkAired(ctx, missing, time.Now()), request.ErrNotFound)
		require.ErrorIs(t, s.MarkFailed(ctx, missing, "x"), request.ErrNotFound)
		_, err = s.BumpAttempts(ctx, missing, "x")
		require.ErrorIs(t, err, request.ErrNotFound)
	})

	t.Run("reorder pins an explicit tier above the natural order", func(t *testing.T) {
		s := newStore(t)
		l1, err := s.Create(ctx, mk(request.SourceListener, "u1", "l1", request.StatusReady))
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
		a1, err := s.Create(ctx, mk(request.SourceAI, "", "a1", request.StatusReady))
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
		l2, err := s.Create(ctx, mk(request.SourceListener, "u2", "l2", request.StatusApproved))
		require.NoError(t, err)

		// natural order: l1, l2, a1 — pin the AI pick first
		require.NoError(t, s.Reorder(ctx, []string{a1.ID, l1.ID, l2.ID}))
		pending, err := s.Pending(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{a1.ID, l1.ID, l2.ID},
			[]string{pending[0].ID, pending[1].ID, pending[2].ID})

		// NextReady honors the explicit order: a1 (ready) now beats l1
		next, found, err := s.NextReady(ctx)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, a1.ID, next.ID)

		// a NEW arrival lands in the natural tier, AFTER positioned items
		time.Sleep(10 * time.Millisecond)
		l3, err := s.Create(ctx, mk(request.SourceListener, "u3", "l3", request.StatusReady))
		require.NoError(t, err)
		pending, err = s.Pending(ctx)
		require.NoError(t, err)
		require.Equal(t, l3.ID, pending[3].ID)
	})

	t.Run("reorder rejects stale sets", func(t *testing.T) {
		s := newStore(t)
		a, err := s.Create(ctx, mk(request.SourceListener, "u1", "a", request.StatusReady))
		require.NoError(t, err)
		b, err := s.Create(ctx, mk(request.SourceListener, "u1", "b", request.StatusReady))
		require.NoError(t, err)

		require.ErrorIs(t, s.Reorder(ctx, []string{a.ID}), request.ErrStale)                // missing
		require.ErrorIs(t, s.Reorder(ctx, []string{a.ID, b.ID, "ghost"}), request.ErrStale) // extra
		require.ErrorIs(t, s.Reorder(ctx, []string{a.ID, a.ID}), request.ErrStale)          // duplicate
		require.NoError(t, s.Reorder(ctx, []string{b.ID, a.ID}))                            // exact set OK
	})

	t.Run("fail-pending guards status and records the reason", func(t *testing.T) {
		s := newStore(t)
		a, err := s.Create(ctx, mk(request.SourceListener, "u1", "a", request.StatusReady))
		require.NoError(t, err)
		require.NoError(t, s.FailPending(ctx, a.ID, "đài đã gỡ yêu cầu này"))
		mine, err := s.ByUser(ctx, "u1", 1)
		require.NoError(t, err)
		require.Equal(t, request.StatusFailed, mine[0].Status)
		require.Equal(t, "đài đã gỡ yêu cầu này", mine[0].FailReason)

		require.ErrorIs(t, s.FailPending(ctx, a.ID, "x"), request.ErrNotFound) // already terminal
		require.ErrorIs(t, s.FailPending(ctx, "00000000-0000-0000-0000-000000000000", "x"), request.ErrNotFound)
	})

	t.Run("recent terminal, newest first with cap", func(t *testing.T) {
		s := newStore(t)
		a, err := s.Create(ctx, mk(request.SourceListener, "u1", "a", request.StatusReady))
		require.NoError(t, err)
		require.NoError(t, s.MarkAired(ctx, a.ID, time.Now().Add(-2*time.Hour)))
		b, err := s.Create(ctx, mk(request.SourceAI, "", "b", request.StatusApproved))
		require.NoError(t, err)
		require.NoError(t, s.MarkFailed(ctx, b.ID, "yt-dlp: 403")) // failed uses created_at ordering key
		c, err := s.Create(ctx, mk(request.SourceListener, "u2", "c", request.StatusReady))
		require.NoError(t, err)
		// Ordering is COALESCE(aired_at, created_at) DESC, which mixes an
		// app-clock timestamp (aired_at) with a DB-clock one (b's created_at).
		// ±1h margins on a/c dwarf any realistic clock skew between the two
		// clocks, keeping the ordering assertion deterministic.
		require.NoError(t, s.MarkAired(ctx, c.ID, time.Now().Add(time.Hour)))

		rec, err := s.RecentTerminal(ctx, 2)
		require.NoError(t, err)
		require.Len(t, rec, 2)
		require.Equal(t, c.ID, rec[0].ID) // newest aired first
	})
}

func TestDayStart(t *testing.T) {
	ict := time.FixedZone("ICT", 7*3600)
	// 2026-07-22 01:30 ICT is 2026-07-21 18:30 UTC — DayStart must be the
	// ICT midnight, not the UTC one.
	now := time.Date(2026, 7, 21, 18, 30, 0, 0, time.UTC)
	got := request.DayStart(now, ict)
	require.Equal(t, time.Date(2026, 7, 22, 0, 0, 0, 0, ict), got)
}
