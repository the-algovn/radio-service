package showlog

import (
	"context"
	"sync"
)

// MemStore is an in-memory Store for hermetic tests (no pruning — tests are
// short-lived; retention is a PGStore concern, matching internal/audit).
type MemStore struct {
	mu    sync.Mutex
	talks []Talk // append order == started_at order in practice, not enforced
}

func NewMemStore() *MemStore { return &MemStore{} }

func (m *MemStore) Append(_ context.Context, t Talk) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.talks = append(m.talks, t)
	return nil
}
