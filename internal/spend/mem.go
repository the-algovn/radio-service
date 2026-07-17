package spend

import (
	"context"
	"sync"
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
