package showlog_test

import (
	"testing"

	"github.com/the-algovn/radio-service/internal/showlog"
)

func TestMemStoreContract(t *testing.T) {
	runStoreContract(t, func(t *testing.T) showlog.Store {
		return showlog.NewMemStore()
	})
}
