package request

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MemStore is an in-memory Store for hermetic tests.
type MemStore struct {
	mu        sync.Mutex
	seq       int
	items     []*Item        // insertion order (== created_at order)
	positions map[string]int // id → explicit position; absent = natural tier
}

func NewMemStore() *MemStore { return &MemStore{positions: map[string]int{}} }

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

// pendingLocked returns approved+ready in air order: the positioned tier
// (ascending position), then the natural tier (listener FIFO, then ai FIFO).
func (m *MemStore) pendingLocked() []*Item {
	var positioned, natural []*Item
	for _, it := range m.items {
		if it.Status != StatusApproved && it.Status != StatusReady {
			continue
		}
		if _, ok := m.positions[it.ID]; ok {
			positioned = append(positioned, it)
		} else {
			natural = append(natural, it)
		}
	}
	sort.SliceStable(positioned, func(i, j int) bool {
		return m.positions[positioned[i].ID] < m.positions[positioned[j].ID]
	})
	var out []*Item
	out = append(out, positioned...)
	for _, src := range []string{SourceListener, SourceAI} {
		for _, it := range natural {
			if it.Source == src {
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

func (m *MemStore) Reorder(_ context.Context, ids []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := map[string]bool{}
	for _, it := range m.pendingLocked() {
		current[it.ID] = true
	}
	if len(ids) != len(current) {
		return ErrStale
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !current[id] || seen[id] {
			return ErrStale
		}
		seen[id] = true
	}
	for i, id := range ids {
		m.positions[id] = i
	}
	return nil
}

func (m *MemStore) RecentTerminal(_ context.Context, n int) ([]Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	type keyed struct {
		it *Item
		at time.Time
	}
	var term []keyed
	for _, it := range m.items {
		switch it.Status {
		case StatusAired:
			at := it.CreatedAt
			if it.AiredAt != nil {
				at = *it.AiredAt
			}
			term = append(term, keyed{it, at})
		case StatusFailed:
			term = append(term, keyed{it, it.CreatedAt})
		}
	}
	sort.SliceStable(term, func(i, j int) bool { return term[i].at.After(term[j].at) })
	if len(term) > n {
		term = term[:n]
	}
	out := make([]Item, 0, len(term))
	for _, k := range term {
		out = append(out, *k.it)
	}
	return out, nil
}

func (m *MemStore) FailPending(_ context.Context, id, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	it := m.find(id)
	if it == nil || (it.Status != StatusApproved && it.Status != StatusReady) {
		return ErrNotFound
	}
	it.Status = StatusFailed
	it.FailReason = reason
	return nil
}
