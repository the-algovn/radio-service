package spend

import (
	"context"
	"sync"
	"time"
)

// MemLedger is an in-memory Ledger for hermetic tests.
type MemLedger struct {
	mu    sync.Mutex
	lines []Line
}

func NewMemLedger() *MemLedger { return &MemLedger{} }

func (m *MemLedger) Append(_ context.Context, line Line) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lines = append(m.lines, line)
	return nil
}

func (m *MemLedger) All(_ context.Context) ([]Line, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Line, len(m.lines))
	copy(out, m.lines)
	return out, nil
}

// SpentSince sums cost at or after since — the programmer's daily budget
// gate (station day boundaries come from the caller).
func (m *MemLedger) SpentSince(_ context.Context, since time.Time) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var t float64
	for _, ln := range m.lines {
		if !ln.TS.Before(since) {
			t += ln.CostUSD
		}
	}
	return t, nil
}
