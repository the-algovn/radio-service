package live

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDJPayloadShape(t *testing.T) {
	e := Entry{Title: "Tiểu Dương Dương",
		StartedAt: time.Date(2026, 7, 22, 15, 4, 5, 500_000_000, time.UTC), DurationS: 21}
	var m map[string]any
	require.NoError(t, json.Unmarshal(DJPayload(e, 3), &m))
	require.Equal(t, "dj", m["kind"])
	require.Equal(t, "Tiểu Dương Dương", m["title"])
	require.Equal(t, "2026-07-22T15:04:05.5Z", m["startedAt"])
	require.Equal(t, float64(21), m["durationSeconds"])
	require.Equal(t, float64(3), m["listeners"])
	// A talk break has no artist/provenance — the omitempty fields must be absent.
	for _, k := range []string{"artist", "source", "requestedByName", "reason"} {
		_, has := m[k]
		require.False(t, has, k)
	}
}

func TestDJPayloadZeroListenersPresent(t *testing.T) {
	e := Entry{Title: "Tiểu Dương Dương", StartedAt: time.Now(), DurationS: 10}
	require.Contains(t, string(DJPayload(e, 0)), `"listeners":0`)
}
