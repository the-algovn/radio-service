package broadcast_test

import (
	"testing"

	"github.com/the-algovn/radio-service/internal/broadcast"
)

func TestMemStoreContract(t *testing.T) {
	runStoreContract(t, func(t *testing.T) broadcast.Store {
		return broadcast.NewMemStore()
	})
}
