package live

import (
	"context"
	"sync"
	"time"
)

// MemAirLog is an in-memory AirLog for hermetic tests.
type MemAirLog struct {
	mu      sync.Mutex
	entries []Entry // append order
}

func NewMemAirLog() *MemAirLog { return &MemAirLog{} }

func (m *MemAirLog) Append(_ context.Context, e Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, e)
	return nil
}

func (m *MemAirLog) Latest(_ context.Context) (Entry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.entries) == 0 {
		return Entry{}, false, nil
	}
	best := m.entries[0]
	for _, e := range m.entries[1:] {
		if !e.StartedAt.Before(best.StartedAt) {
			best = e
		}
	}
	return best, true, nil
}

func (m *MemAirLog) History(_ context.Context, limit int) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	out := make([]Entry, 0, len(m.entries))
	for i := len(m.entries) - 1; i >= 0 && len(out) < limit; i-- {
		e := m.entries[i]
		if e.StartedAt.Add(time.Duration(e.DurationS) * time.Second).Before(now) {
			out = append(out, e)
		}
	}
	return out, nil
}

// MemListeners is an in-memory Listeners; now is injectable for tests.
type MemListeners struct {
	mu   sync.Mutex
	now  func() time.Time
	seen map[string]time.Time
}

func NewMemListeners(now func() time.Time) *MemListeners {
	return &MemListeners{now: now, seen: map[string]time.Time{}}
}

func (m *MemListeners) Beat(_ context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen[sessionID] = m.now()
	return nil
}

func (m *MemListeners) Count(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := m.now().Add(-75 * time.Second)
	n := 0
	for _, at := range m.seen {
		if at.After(cutoff) {
			n++
		}
	}
	return n, nil
}
