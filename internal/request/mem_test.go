package request_test

import (
	"testing"

	"github.com/the-algovn/radio-service/internal/request"
)

func TestMemStoreContract(t *testing.T) {
	runStoreContract(t, func(t *testing.T) request.Store {
		return request.NewMemStore()
	})
}
