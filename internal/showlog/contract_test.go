package showlog_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/showlog"
)

type storeFactory func(t *testing.T) showlog.Store

func runStoreContract(t *testing.T, newStore storeFactory) {
	ctx := context.Background()

	t.Run("append accepts a seam with full provenance", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.Append(ctx, showlog.Talk{
			Kind: showlog.KindSeam, StartedAt: time.Now(), DurationS: 38,
			Script: "Vừa rồi là Lạc Trôi.", BacksellTitle: "Lạc Trôi",
			PromiseTitle: "Chạy Ngay Đi", CorrelationID: "corr-1",
		}))
	})

	t.Run("append accepts a station ID with no correlation id", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.Append(ctx, showlog.Talk{
			Kind: showlog.KindStationID, StartedAt: time.Now(), DurationS: 11,
			Script: "Tần Số 42.",
		}))
	})

	t.Run("append tolerates every optional field being empty", func(t *testing.T) {
		// Every text column is NOT NULL DEFAULT '' — a zero-valued Talk must
		// not violate a constraint, because the failure path would only ever
		// show up in prod on a clip whose script came back blank.
		s := newStore(t)
		require.NoError(t, s.Append(ctx, showlog.Talk{
			Kind: showlog.KindSeam, StartedAt: time.Now(), DurationS: 1,
		}))
	})
}
