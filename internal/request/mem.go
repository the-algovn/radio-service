package request

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MemStore is an in-memory Store for hermetic tests.
type MemStore struct {
	mu    sync.Mutex
	seq   int
	items []*Item // insertion order (== created_at order)
}

func NewMemStore() *MemStore { return &MemStore{} }

func (m *MemStore) Create(_ context.Context, it Item) (Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	it.ID = fmt.Sprintf("mem-%d", m.seq)
	it.CreatedAt = time.Now()
	it.FailReason = ""
	it.Attempts = 0
	it.AiredAt = nil
	stored := it
	m.items = append(m.items, &stored)
	return stored, nil
}

func (m *MemStore) find(id string) *Item {
	for _, it := range m.items {
		if it.ID == id {
			return it
		}
	}
	return nil
}

// pendingLocked returns approved+ready in air order: listener FIFO, then ai
// FIFO. m.items is already FIFO, so two passes suffice.
func (m *MemStore) pendingLocked() []*Item {
	var out []*Item
	for _, src := range []string{SourceListener, SourceAI} {
		for _, it := range m.items {
			if it.Source == src && (it.Status == StatusApproved || it.Status == StatusReady) {
				out = append(out, it)
			}
		}
	}
	return out
}

func (m *MemStore) NextReady(_ context.Context) (Item, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, it := range m.pendingLocked() {
		if it.Status == StatusReady {
			return *it, true, nil
		}
	}
	return Item{}, false, nil
}

func (m *MemStore) OldestApproved(_ context.Context) (Item, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, it := range m.items { // insertion order, any source
		if it.Status == StatusApproved {
			return *it, true, nil
		}
	}
	return Item{}, false, nil
}

func (m *MemStore) Pending(_ context.Context) ([]Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ps := m.pendingLocked()
	out := make([]Item, 0, len(ps))
	for _, it := range ps {
		out = append(out, *it)
	}
	return out, nil
}

func (m *MemStore) ByUser(_ context.Context, sub string, limit int) ([]Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Item
	for i := len(m.items) - 1; i >= 0 && len(out) < limit; i-- {
		if m.items[i].RequestedBy == sub {
			out = append(out, *m.items[i])
		}
	}
	return out, nil
}

func (m *MemStore) CountPendingByUser(_ context.Context, sub string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, it := range m.items {
		if it.RequestedBy == sub && (it.Status == StatusApproved || it.Status == StatusReady) {
			n++
		}
	}
	return n, nil
}

func (m *MemStore) CountSince(_ context.Context, sub string, since time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, it := range m.items {
		if it.RequestedBy == sub && !it.CreatedAt.Before(since) {
			n++
		}
	}
	return n, nil
}

func (m *MemStore) HasPendingYTID(_ context.Context, ytID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, it := range m.items {
		if it.YTID == ytID && (it.Status == StatusApproved || it.Status == StatusReady) {
			return true, nil
		}
	}
	return false, nil
}

func (m *MemStore) MarkReady(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	it := m.find(id)
	if it == nil || it.Status != StatusApproved {
		return ErrNotFound
	}
	it.Status = StatusReady
	return nil
}

func (m *MemStore) MarkAired(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	it := m.find(id)
	if it == nil {
		return ErrNotFound
	}
	it.Status = StatusAired
	stamp := at
	it.AiredAt = &stamp
	return nil
}

func (m *MemStore) MarkFailed(_ context.Context, id, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	it := m.find(id)
	if it == nil {
		return ErrNotFound
	}
	it.Status = StatusFailed
	it.FailReason = reason
	return nil
}

func (m *MemStore) BumpAttempts(_ context.Context, id, reason string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it := m.find(id)
	if it == nil {
		return 0, ErrNotFound
	}
	it.Attempts++
	it.FailReason = reason
	return it.Attempts, nil
}
