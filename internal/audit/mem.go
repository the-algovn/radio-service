package audit

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemStore is an in-memory Store for hermetic tests (no pruning — tests are
// short-lived; retention is a PGStore concern).
type MemStore struct {
	mu   sync.Mutex
	recs []Rec
	next int64
}

func NewMemStore() *MemStore { return &MemStore{} }

func (m *MemStore) Record(_ context.Context, r Rec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	r.ID = m.next
	m.recs = append(m.recs, r)
	return nil
}

func (m *MemStore) match(r Rec, f Filter) bool {
	switch {
	case f.Label == "": // all
	case f.Label == "script:": // prefix: all script:* call-sites
		if !strings.HasPrefix(r.Label, "script:") {
			return false
		}
	default: // exact
		if r.Label != f.Label {
			return false
		}
	}
	if f.ErrorsOnly && r.Error == "" {
		return false
	}
	return true
}

func (m *MemStore) List(_ context.Context, f Filter, limit, offset int) ([]Rec, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var got []Rec
	for _, r := range m.recs {
		if m.match(r, f) {
			got = append(got, r)
		}
	}
	sort.SliceStable(got, func(i, j int) bool { return got[i].TS.After(got[j].TS) }) // newest first
	if offset > len(got) {
		offset = len(got)
	}
	got = got[offset:]
	if limit >= 0 && limit < len(got) {
		got = got[:limit]
	}
	return got, nil
}

func (m *MemStore) Count(_ context.Context, f Filter) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, r := range m.recs {
		if m.match(r, f) {
			n++
		}
	}
	return n, nil
}

func (m *MemStore) Stats(_ context.Context, since time.Time) ([]Stat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	type key struct{ label, model string }
	agg := map[key]*Stat{}
	for _, r := range m.recs {
		if r.TS.Before(since) {
			continue
		}
		k := key{r.Label, r.Model}
		s := agg[k]
		if s == nil {
			s = &Stat{Label: r.Label, Model: r.Model}
			agg[k] = s
		}
		s.Count++
		s.InTokens += r.InTokens
		s.OutTokens += r.OutTokens
		s.CostUSD += r.CostUSD
	}
	out := make([]Stat, 0, len(agg))
	for _, s := range agg {
		out = append(out, *s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CostUSD > out[j].CostUSD })
	return out, nil
}
