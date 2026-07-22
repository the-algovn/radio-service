package live_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/live"
)

type storeFactory func(t *testing.T) (live.AirLog, live.Listeners)

func runStoreContract(t *testing.T, newStores storeFactory) {
	ctx := context.Background()

	t.Run("air log latest and history", func(t *testing.T) {
		log, _ := newStores(t)

		_, found, err := log.Latest(ctx)
		require.NoError(t, err)
		require.False(t, found)

		base := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
		// two completed tracks + one still airing (short past + long current)
		require.NoError(t, log.Append(ctx, live.Entry{YTID: "a", Title: "A", Artist: "ch-a", StartedAt: base, DurationS: 60}))
		require.NoError(t, log.Append(ctx, live.Entry{YTID: "b", Title: "B", Artist: "ch-b", StartedAt: base.Add(60 * time.Second), DurationS: 60}))
		require.NoError(t, log.Append(ctx, live.Entry{YTID: "c", Title: "C", Artist: "ch-c", StartedAt: time.Now().Add(-5 * time.Second), DurationS: 3600}))

		cur, found, err := log.Latest(ctx)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, "c", cur.YTID)
		require.Equal(t, 3600, cur.DurationS)

		hist, err := log.History(ctx, 20)
		require.NoError(t, err)
		require.Len(t, hist, 2)
		require.Equal(t, []string{"b", "a"}, []string{hist[0].YTID, hist[1].YTID}) // completed only, newest first

		hist, err = log.History(ctx, 1)
		require.NoError(t, err)
		require.Len(t, hist, 1)
		require.Equal(t, "b", hist[0].YTID)
	})

	t.Run("air log recent and since", func(t *testing.T) {
		log, _ := newStores(t)
		testAirLogRecentAndSince(t, log)
	})

	t.Run("listeners window", func(t *testing.T) {
		_, ls := newStores(t)
		n, err := ls.Count(ctx)
		require.NoError(t, err)
		require.Zero(t, n)

		require.NoError(t, ls.Beat(ctx, "tab-1"))
		require.NoError(t, ls.Beat(ctx, "tab-2"))
		require.NoError(t, ls.Beat(ctx, "tab-1")) // upsert, not a third row
		n, err = ls.Count(ctx)
		require.NoError(t, err)
		require.Equal(t, 2, n)
	})
}

func testAirLogRecentAndSince(t *testing.T, log live.AirLog) {
	ctx := context.Background()
	base := time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
	for i, id := range []string{"a", "b", "c"} {
		require.NoError(t, log.Append(ctx, live.Entry{
			YTID: id, Title: "t-" + id, Artist: "c-" + id,
			StartedAt: base.Add(time.Duration(i) * 5 * time.Minute), DurationS: 240,
		}))
	}

	ids, err := log.RecentYTIDs(ctx, 2)
	require.NoError(t, err)
	require.Equal(t, []string{"c", "b"}, ids) // newest first, limit respected

	ok, err := log.AiredSince(ctx, "a", base.Add(-time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = log.AiredSince(ctx, "a", base.Add(time.Minute)) // a started AT base
	require.NoError(t, err)
	require.False(t, ok)
	ok, err = log.AiredSince(ctx, "zz", base.Add(-time.Hour))
	require.NoError(t, err)
	require.False(t, ok)
}
