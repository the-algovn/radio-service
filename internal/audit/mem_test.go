package audit_test

import (
	"testing"

	"github.com/the-algovn/radio-service/internal/audit"
)

func TestMemStoreContract(t *testing.T) {
	runStoreContract(t, func(t *testing.T) audit.Store { return audit.NewMemStore() })
}
