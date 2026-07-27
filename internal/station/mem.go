package station

import (
	"context"
	"sync"
	"time"
)

// MemStore is an in-memory Store for hermetic tests. AI starts enabled and
// DJ settings start at the column defaults, matching the migration.
type MemStore struct {
	mu    sync.Mutex
	onAir bool
	since *time.Time
	ai    bool
	dj    DJSettings
}

func NewMemStore() *MemStore { return &MemStore{ai: true, dj: DefaultDJSettings()} }

func (m *MemStore) snapshot() Station {
	return Station{OnAir: m.onAir, OnAirSince: m.since, AIEnabled: m.ai, DJ: m.dj}
}

func (m *MemStore) GetStation(_ context.Context) (Station, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshot(), nil
}

func (m *MemStore) GoOnAir(_ context.Context) (Station, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.onAir {
		m.onAir = true
		now := time.Now()
		m.since = &now
	}
	return m.snapshot(), nil
}

func (m *MemStore) GoOffAir(_ context.Context) (Station, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onAir = false
	m.since = nil
	return m.snapshot(), nil
}

func (m *MemStore) SetAIEnabled(_ context.Context, enabled bool) (Station, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ai = enabled
	return m.snapshot(), nil
}

func (m *MemStore) UpdateDJSettings(_ context.Context, s DJSettings) (Station, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dj = s
	return m.snapshot(), nil
}
