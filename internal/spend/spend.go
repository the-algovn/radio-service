// Package spend is the lab's cost ledger: an append-only JSONL file. Its
// arithmetic is what Phase 1's budget cap will reuse — the Phase-0 exit
// criteria include matching these totals against provider dashboards.
package spend

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Line struct {
	TS        time.Time `json:"ts"`
	Kind      string    `json:"kind"`     // tts | llm
	Provider  string    `json:"provider"` // google | gemini | anthropic | fake
	Label     string    `json:"label"`
	Chars     int       `json:"chars,omitempty"`
	InTokens  int       `json:"in_tokens,omitempty"`
	OutTokens int       `json:"out_tokens,omitempty"`
	CostUSD   float64   `json:"cost_usd"`
}

type Ledger struct {
	path string
	mu   sync.Mutex
}

func NewLedger(path string) *Ledger { return &Ledger{path: path} }

func (l *Ledger) Append(line Line) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(line)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

func (l *Ledger) All() ([]Line, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Line
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var ln Line
		if json.Unmarshal(sc.Bytes(), &ln) == nil {
			out = append(out, ln)
		}
	}
	return out, sc.Err()
}

func Total(lines []Line) float64 {
	var t float64
	for _, ln := range lines {
		t += ln.CostUSD
	}
	return t
}
