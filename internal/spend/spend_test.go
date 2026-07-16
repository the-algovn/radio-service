package spend

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAppendAndAll(t *testing.T) {
	l := NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"))
	require.NoError(t, l.Append(Line{TS: time.Now(), Kind: "tts", Provider: "google", Label: "take-1", Chars: 120, CostUSD: 0.0036}))
	require.NoError(t, l.Append(Line{TS: time.Now(), Kind: "llm", Provider: "gemini", Label: "intro", InTokens: 900, OutTokens: 220, CostUSD: 0.0008}))
	lines, err := l.All()
	require.NoError(t, err)
	require.Len(t, lines, 2)
	require.Equal(t, "google", lines[0].Provider)
	require.InDelta(t, 0.0044, Total(lines), 1e-9)
}

func TestAllMissingFileIsEmpty(t *testing.T) {
	l := NewLedger(filepath.Join(t.TempDir(), "none.jsonl"))
	lines, err := l.All()
	require.NoError(t, err)
	require.Empty(t, lines)
}
