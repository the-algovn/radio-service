package showlog

import (
	"context"
	"sort"
	"sync"
)

// MemStore is an in-memory Store for hermetic tests (no pruning — tests are
// short-lived; retention is a PGStore concern, matching internal/audit).
type MemStore struct {
	mu    sync.Mutex
	talks []Talk
	music MusicFunc // nil = talk-only
	next  int64
}

// NewMemStore builds the store. music supplies the air_log half; pass nil for
// talk-only. See MusicFunc for why this is injected rather than read directly.
func NewMemStore(music MusicFunc) *MemStore { return &MemStore{music: music} }

func (m *MemStore) Append(_ context.Context, t Talk) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	m.talks = append(m.talks, t)
	return nil
}

// merged returns talk ∪ music, newest first. Caller must NOT hold mu.
func (m *MemStore) merged(ctx context.Context) ([]Segment, error) {
	m.mu.Lock()
	got := make([]Segment, 0, len(m.talks))
	for i, t := range m.talks {
		got = append(got, Segment{
			ID: int64(i + 1), Origin: OriginTalk, Kind: t.Kind,
			StartedAt: t.StartedAt, DurationS: t.DurationS, Script: t.Script,
			BacksellTitle: t.BacksellTitle, PromiseTitle: t.PromiseTitle,
			CorrelationID: t.CorrelationID,
		})
	}
	music := m.music
	m.mu.Unlock()

	// Called with mu released: music is caller-supplied and must not run
	// under this store's lock.
	if music != nil {
		ms, err := music(ctx)
		if err != nil {
			return nil, err
		}
		got = append(got, ms...)
	}
	sort.SliceStable(got, func(i, j int) bool { return got[i].StartedAt.After(got[j].StartedAt) })
	return got, nil
}

func (m *MemStore) Recent(ctx context.Context, limit, offset int) ([]Segment, error) {
	got, err := m.merged(ctx)
	if err != nil {
		return nil, err
	}
	if offset < 0 {
		offset = 0
	}
	if offset > len(got) {
		offset = len(got)
	}
	got = got[offset:]
	if n := clampLimit(limit); n < len(got) {
		got = got[:n]
	}
	return got, nil
}

func (m *MemStore) Count(ctx context.Context) (int64, error) {
	got, err := m.merged(ctx)
	if err != nil {
		return 0, err
	}
	return int64(len(got)), nil
}
