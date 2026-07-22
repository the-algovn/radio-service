package station_test

import (
	"testing"

	"github.com/the-algovn/radio-service/internal/station"
)

func TestMemStoreContract(t *testing.T) {
	runStoreContract(t, func(t *testing.T) station.Store {
		return station.NewMemStore()
	})
}
