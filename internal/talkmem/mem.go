package talkmem

import (
	"context"
	"sync"
	"time"
)

// MemStore is an in-memory Store for hermetic tests.
type MemStore struct {
	mu      sync.Mutex
	entries []Entry // append order == created_at order
}

func NewMemStore() *MemStore { return &MemStore{} }

func (m *MemStore) Append(_ context.Context, e Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.CreatedAt = time.Now()
	m.entries = append(m.entries, e)
	return nil
}

func (m *MemStore) Recent(_ context.Context, since time.Time, limit int) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var got []Entry
	for _, e := range m.entries {
		if !e.CreatedAt.Before(since) {
			got = append(got, e)
		}
	}
	if limit > 0 && len(got) > limit {
		got = got[len(got)-limit:] // newest limit, still oldest-first
	}
	return got, nil
}
