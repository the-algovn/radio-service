package library

import (
	"context"
	"sort"
	"strings"
	"sync"
)

// MemLibrary is an in-memory Library for hermetic tests.
type MemLibrary struct {
	mu     sync.Mutex
	tracks map[string]Track
}

func NewMemLibrary() *MemLibrary { return &MemLibrary{tracks: map[string]Track{}} }

func (m *MemLibrary) Get(_ context.Context, ytID string) (Track, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tracks[ytID]
	return t, ok, nil
}

func (m *MemLibrary) Add(_ context.Context, t Track) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.tracks[t.YTID]; exists {
		return nil
	}
	m.tracks[t.YTID] = t
	return nil
}

func (m *MemLibrary) List(_ context.Context, query string, limit int) ([]Track, error) {
	if limit <= 0 {
		limit = 50
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	q := strings.ToLower(query)
	out := make([]Track, 0, len(m.tracks))
	for _, t := range m.tracks {
		if q == "" || strings.Contains(strings.ToLower(t.Title), q) || strings.Contains(strings.ToLower(t.Channel), q) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AddedAt.After(out[j].AddedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemLibrary) Delete(_ context.Context, ytID string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tracks[ytID]
	if !ok {
		return "", false, nil
	}
	delete(m.tracks, ytID)
	return t.ArtifactID, true, nil
}
