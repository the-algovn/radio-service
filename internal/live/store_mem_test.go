package live_test

import (
	"testing"
	"time"

	"github.com/the-algovn/radio-service/internal/live"
)

func TestMemStoresContract(t *testing.T) {
	runStoreContract(t, func(t *testing.T) (live.AirLog, live.Listeners) {
		return live.NewMemAirLog(), live.NewMemListeners(time.Now)
	})
}
