package playlist

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/the-algovn/radio-service/internal/library"
)

// MemStore is an in-memory Store for hermetic tests. It does NOT mirror the
// pg FK cascade from library deletes (that behavior is pg-only, covered by
// the integration test).
type MemStore struct {
	mu      sync.Mutex
	lib     library.Library
	seq     int
	lists   []*memPlaylist // newest first
	station struct {
		activeID string
		onAir    bool
		since    *time.Time
	}
}

type memPlaylist struct {
	id, name           string
	createdAt, updated time.Time
	items              []string // yt_ids in order
}

func NewMemStore(lib library.Library) *MemStore { return &MemStore{lib: lib} }

func (m *MemStore) find(id string) *memPlaylist {
	for _, p := range m.lists {
		if p.id == id {
			return p
		}
	}
	return nil
}

// summary and itemsOf are called with m.mu held.
func (m *MemStore) summary(ctx context.Context, p *memPlaylist) (Summary, error) {
	var total int64
	for _, yt := range p.items {
		tr, ok, err := m.lib.Get(ctx, yt)
		if err != nil {
			return Summary{}, err
		}
		if ok {
			total += int64(tr.DurationS)
		}
	}
	return Summary{
		ID: p.id, Name: p.name, TrackCount: len(p.items), TotalDurationS: total,
		IsActive: m.station.activeID == p.id, CreatedAt: p.createdAt, UpdatedAt: p.updated,
	}, nil
}

func (m *MemStore) itemsOf(ctx context.Context, p *memPlaylist) ([]Item, error) {
	out := make([]Item, 0, len(p.items))
	for i, yt := range p.items {
		tr, ok, err := m.lib.Get(ctx, yt)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out = append(out, Item{Position: i, YTID: yt, Title: tr.Title, Channel: tr.Channel, DurationS: int64(tr.DurationS)})
	}
	return out, nil
}

func (m *MemStore) Create(_ context.Context, name string) (Summary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	now := time.Now()
	p := &memPlaylist{id: fmt.Sprintf("mem-%d", m.seq), name: name, createdAt: now, updated: now}
	m.lists = append([]*memPlaylist{p}, m.lists...)
	return Summary{ID: p.id, Name: p.name, CreatedAt: now, UpdatedAt: now}, nil
}

func (m *MemStore) List(ctx context.Context) ([]Summary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Summary, 0, len(m.lists))
	for _, p := range m.lists {
		s, err := m.summary(ctx, p)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (m *MemStore) Get(ctx context.Context, id string) (Summary, []Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.find(id)
	if p == nil {
		return Summary{}, nil, ErrNotFound
	}
	s, err := m.summary(ctx, p)
	if err != nil {
		return Summary{}, nil, err
	}
	items, err := m.itemsOf(ctx, p)
	return s, items, err
}

func (m *MemStore) Rename(ctx context.Context, id, name string) (Summary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.find(id)
	if p == nil {
		return Summary{}, ErrNotFound
	}
	p.name, p.updated = name, time.Now()
	return m.summary(ctx, p)
}

func (m *MemStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.find(id)
	if p == nil {
		return ErrNotFound
	}
	if m.station.activeID == id && m.station.onAir {
		return ErrActiveOnAir
	}
	for i, q := range m.lists {
		if q.id == id {
			m.lists = append(m.lists[:i], m.lists[i+1:]...)
			break
		}
	}
	if m.station.activeID == id {
		m.station.activeID = ""
	}
	return nil
}

func (m *MemStore) AddTrack(ctx context.Context, playlistID, ytID string) (Summary, []Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.find(playlistID)
	if p == nil {
		return Summary{}, nil, ErrNotFound
	}
	_, ok, err := m.lib.Get(ctx, ytID)
	if err != nil {
		return Summary{}, nil, err
	}
	if !ok {
		return Summary{}, nil, ErrNotFound
	}
	present := false
	for _, yt := range p.items {
		if yt == ytID {
			present = true
			break
		}
	}
	if !present {
		p.items = append(p.items, ytID)
		p.updated = time.Now()
	}
	return m.getLocked(ctx, p)
}

func (m *MemStore) getLocked(ctx context.Context, p *memPlaylist) (Summary, []Item, error) {
	s, err := m.summary(ctx, p)
	if err != nil {
		return Summary{}, nil, err
	}
	items, err := m.itemsOf(ctx, p)
	return s, items, err
}

func (m *MemStore) RemoveTrack(ctx context.Context, playlistID, ytID string) (Summary, []Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.find(playlistID)
	if p == nil {
		return Summary{}, nil, ErrNotFound
	}
	idx := -1
	for i, yt := range p.items {
		if yt == ytID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return Summary{}, nil, ErrNotFound
	}
	if m.station.onAir && m.station.activeID == playlistID && len(p.items) == 1 {
		return Summary{}, nil, ErrActiveOnAir
	}
	p.items = append(p.items[:idx], p.items[idx+1:]...)
	p.updated = time.Now()
	return m.getLocked(ctx, p)
}

func (m *MemStore) Reorder(ctx context.Context, playlistID string, ytIDs []string) (Summary, []Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.find(playlistID)
	if p == nil {
		return Summary{}, nil, ErrNotFound
	}
	if len(ytIDs) != len(p.items) {
		return Summary{}, nil, ErrStale
	}
	current := map[string]bool{}
	for _, yt := range p.items {
		current[yt] = true
	}
	used := map[string]bool{}
	for _, yt := range ytIDs {
		if !current[yt] || used[yt] { // unknown or duplicated id — stale/corrupt list
			return Summary{}, nil, ErrStale
		}
		used[yt] = true
	}
	p.items = append([]string(nil), ytIDs...)
	p.updated = time.Now()
	return m.getLocked(ctx, p)
}

// stationLocked projects the station with active-playlist metadata. Caller
// holds m.mu.
func (m *MemStore) stationLocked() Station {
	s := Station{ActivePlaylistID: m.station.activeID, OnAir: m.station.onAir, OnAirSince: m.station.since}
	if p := m.find(m.station.activeID); p != nil {
		s.ActivePlaylistName, s.ActiveTrackCount = p.name, len(p.items)
	}
	return s
}

func (m *MemStore) GetStation(_ context.Context) (Station, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stationLocked(), nil
}

func (m *MemStore) SetActive(_ context.Context, playlistID string) (Station, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.find(playlistID)
	if p == nil {
		return Station{}, ErrNotFound
	}
	if len(p.items) == 0 {
		return Station{}, ErrEmptyPlaylist
	}
	m.station.activeID = playlistID
	return m.stationLocked(), nil
}

func (m *MemStore) GoOnAir(_ context.Context) (Station, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.station.onAir {
		now := time.Now()
		m.station.onAir, m.station.since = true, &now
	}
	return m.stationLocked(), nil
}

func (m *MemStore) GoOffAir(_ context.Context) (Station, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.station.onAir, m.station.since = false, nil
	return m.stationLocked(), nil
}
