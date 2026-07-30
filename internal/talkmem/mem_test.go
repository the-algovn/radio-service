package talkmem_test

import (
	"testing"

	"github.com/the-algovn/radio-service/internal/talkmem"
)

func TestMemStoreContract(t *testing.T) {
	runStoreContract(t, func(t *testing.T) talkmem.Store {
		return talkmem.NewMemStore()
	})
}
