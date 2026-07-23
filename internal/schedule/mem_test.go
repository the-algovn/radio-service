package schedule_test

import (
	"testing"

	"github.com/the-algovn/radio-service/internal/schedule"
)

func TestMemStoreContract(t *testing.T) {
	runStoreContract(t, func(t *testing.T) schedule.Store {
		return schedule.NewMemStore()
	})
}
