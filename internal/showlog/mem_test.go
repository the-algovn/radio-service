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
		return showlog.NewMemStore(func(context.Context) ([]showlog.Segment, error) {
			return music, nil
		})
	})
}
