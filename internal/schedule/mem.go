package schedule

import (
	"context"
	"sync"
)

// MemStore is an in-memory Store for hermetic tests.
type MemStore struct {
	mu  sync.Mutex
	n   NextUp
	set bool
}

func NewMemStore() *MemStore { return &MemStore{} }

func (m *MemStore) GetNextUp(_ context.Context) (NextUp, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n, m.set, nil
}

func (m *MemStore) SetNextUp(_ context.Context, n NextUp) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n, m.set = n, true
	return nil
}

func (m *MemStore) ClearNextUp(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n, m.set = NextUp{}, false
	return nil
}
