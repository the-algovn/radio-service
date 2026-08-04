package broadcast

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemStore is an in-memory Store for hermetic tests.
type MemStore struct {
	mu       sync.Mutex
	sessions []Session
	next     int64
}

func NewMemStore() *MemStore { return &MemStore{} }

func (m *MemStore) Open(_ context.Context, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	m.sessions = append(m.sessions, Session{ID: m.next, StartedAt: at})
	return nil
}

func (m *MemStore) Close(_ context.Context, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.sessions {
		if m.sessions[i].EndedAt == nil {
			end := at
			m.sessions[i].EndedAt = &end
		}
	}
	return nil
}

// CloseOrphans has no air log to consult in memory, so it closes each open
// session at its own start — the same fallback the SQL uses when nothing
// aired inside the session.
func (m *MemStore) CloseOrphans(_ context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for i := range m.sessions {
		if m.sessions[i].EndedAt == nil {
			end := m.sessions[i].StartedAt
			m.sessions[i].EndedAt = &end
			n++
		}
	}
	return n, nil
}

func (m *MemStore) Overlapping(_ context.Context, from, to time.Time, limit int) ([]Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = defaultLimit
	}
	var got []Session
	for _, s := range m.sessions {
		if s.StartedAt.After(to) {
			continue
		}
		if s.EndedAt != nil && s.EndedAt.Before(from) {
			continue
		}
		got = append(got, s)
	}
	sort.SliceStable(got, func(i, j int) bool { return got[i].StartedAt.After(got[j].StartedAt) })
	if len(got) > limit {
		got = got[:limit]
	}
	return got, nil
}
