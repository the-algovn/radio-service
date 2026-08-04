package showlog_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/showlog"
)

// storeFactory builds a store already containing `music` — the air_log half of
// the show log. Each backend supplies it its own way (MemStore via MusicFunc,
// PGStore by inserting air_log rows), which is what lets the merged-ordering
// case below run against BOTH implementations instead of only the PG one.
type storeFactory func(t *testing.T, music []showlog.Segment) showlog.Store

func runStoreContract(t *testing.T, newStore storeFactory) {
	ctx := context.Background()

	t.Run("append accepts a seam with full provenance", func(t *testing.T) {
		s := newStore(t, nil)
		require.NoError(t, s.Append(ctx, showlog.Talk{
			Kind: showlog.KindSeam, StartedAt: time.Now(), DurationS: 38,
			Script: "Vừa rồi là Lạc Trôi.", BacksellTitle: "Lạc Trôi",
			PromiseTitle: "Chạy Ngay Đi", CorrelationID: "corr-1",
		}))
	})

	t.Run("append accepts a station ID with no correlation id", func(t *testing.T) {
		s := newStore(t, nil)
		require.NoError(t, s.Append(ctx, showlog.Talk{
			Kind: showlog.KindStationID, StartedAt: time.Now(), DurationS: 11,
			Script: "Tần Số 42.",
		}))
	})

	t.Run("append tolerates every optional field being empty", func(t *testing.T) {
		// Every text column is NOT NULL DEFAULT '' — a zero-valued Talk must
		// not violate a constraint, because the failure path would only ever
		// show up in prod on a clip whose script came back blank.
		s := newStore(t, nil)
		require.NoError(t, s.Append(ctx, showlog.Talk{
			Kind: showlog.KindSeam, StartedAt: time.Now(), DurationS: 1,
		}))
	})

	t.Run("recent merges music and talk in one time order", func(t *testing.T) {
		// The whole point of the show log. If this only ever ran against
		// PGStore it would not run in CI at all — CI passes no -tags
		// integration — and Plan 2's projection would be built on an
		// unverified ordering.
		base := time.Date(2026, 8, 4, 21, 0, 0, 0, time.UTC)
		s := newStore(t, []showlog.Segment{
			{Origin: showlog.OriginAir, Kind: "track", YTID: "y1", Title: "Lạc Trôi",
				Artist: "Sơn Tùng M-TP", StartedAt: base, DurationS: 240},
			{Origin: showlog.OriginAir, Kind: "track", YTID: "y2", Title: "Chạy Ngay Đi",
				Artist: "Sơn Tùng M-TP", StartedAt: base.Add(9 * time.Minute), DurationS: 268},
		})
		// The break sits BETWEEN the two tracks in wall-clock order.
		require.NoError(t, s.Append(ctx, showlog.Talk{
			Kind: showlog.KindSeam, StartedAt: base.Add(4 * time.Minute),
			DurationS: 38, Script: "Vừa rồi là Lạc Trôi.", BacksellTitle: "Lạc Trôi",
		}))

		got, err := s.Recent(ctx, 10, 0)
		require.NoError(t, err)
		require.Len(t, got, 3)
		require.Equal(t, "Chạy Ngay Đi", got[0].Title)
		require.Equal(t, showlog.OriginTalk, got[1].Origin, "the break interleaves between the tracks")
		require.Equal(t, "Lạc Trôi", got[2].Title)

		n, err := s.Count(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(3), n, "count spans both halves")
	})

	t.Run("recent returns talk segments newest first", func(t *testing.T) {
		s := newStore(t, nil)
		base := time.Date(2026, 8, 4, 21, 0, 0, 0, time.UTC)
		require.NoError(t, s.Append(ctx, showlog.Talk{
			Kind: showlog.KindSeam, StartedAt: base, DurationS: 30, Script: "một"}))
		require.NoError(t, s.Append(ctx, showlog.Talk{
			Kind: showlog.KindSeam, StartedAt: base.Add(5 * time.Minute),
			DurationS: 30, Script: "hai"}))

		got, err := s.Recent(ctx, 10, 0)
		require.NoError(t, err)
		require.Len(t, got, 2)
		require.Equal(t, "hai", got[0].Script, "newest first")
		require.Equal(t, showlog.OriginTalk, got[0].Origin)
	})

	t.Run("recent pages with limit and offset", func(t *testing.T) {
		s := newStore(t, nil)
		base := time.Date(2026, 8, 4, 21, 0, 0, 0, time.UTC)
		for i := range 5 {
			require.NoError(t, s.Append(ctx, showlog.Talk{
				Kind: showlog.KindSeam, StartedAt: base.Add(time.Duration(i) * time.Minute),
				DurationS: 10, Script: string(rune('a' + i))}))
		}
		got, err := s.Recent(ctx, 2, 2)
		require.NoError(t, err)
		require.Len(t, got, 2)
		require.Equal(t, "c", got[0].Script)

		n, err := s.Count(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(5), n)
	})

	t.Run("limit is clamped to the shared bounds", func(t *testing.T) {
		s := newStore(t, nil)
		require.NoError(t, s.Append(ctx, showlog.Talk{
			Kind: showlog.KindSeam, StartedAt: time.Now(), DurationS: 10}))
		got, err := s.Recent(ctx, 0, 0) // 0 must mean the default, never unbounded
		require.NoError(t, err)
		require.Len(t, got, 1)
	})

	t.Run("equal timestamps order is stable and repeatable", func(t *testing.T) {
		// When music and talk share the exact same StartedAt, and IDs are
		// not unique across origins, the backend must still produce a
		// deterministic order — otherwise paging could skip or repeat a row.
		base := time.Date(2026, 8, 4, 22, 0, 0, 0, time.UTC)
		s := newStore(t, []showlog.Segment{
			{Origin: showlog.OriginAir, Kind: "track", Title: "Music", StartedAt: base},
		})
		require.NoError(t, s.Append(ctx, showlog.Talk{
			Kind: showlog.KindSeam, StartedAt: base, DurationS: 10,
			Script: "Talk",
		}))

		first, err := s.Recent(ctx, 10, 0)
		require.NoError(t, err)
		require.Len(t, first, 2)

		second, err := s.Recent(ctx, 10, 0)
		require.NoError(t, err)
		require.Equal(t, first, second, "same input must produce the same order every time")
	})
}
