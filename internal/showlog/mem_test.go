package showlog_test

import (
	"context"
	"testing"

	"github.com/the-algovn/radio-service/internal/showlog"
)

func TestMemStoreContract(t *testing.T) {
	runStoreContract(t, func(t *testing.T, music []showlog.Segment) showlog.Store {
		if music == nil {
			return showlog.NewMemStore(nil)
		}
		store := showlog.NewMemStore(func(context.Context) ([]showlog.Segment, error) {
			return music, nil
		})
		// When music carries an explicit ID the caller expects a matching
		// talk segment with the same id and timestamp. Pre-append it so the
		// origin tiebreak (talk before air, when id and started_at are tied)
		// is exercised on this backend too.
		for _, m := range music {
			if m.ID != 0 {
				_ = store.Append(context.Background(), showlog.Talk{
					Kind: showlog.KindSeam, StartedAt: m.StartedAt, DurationS: 10, Script: "Talk",
				})
			}
		}
		return store
	})
}
