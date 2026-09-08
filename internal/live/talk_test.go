package live

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/cadence"
	"github.com/the-algovn/radio-service/internal/showlog"
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

func TestClipKindConstantsMirrorShowlog(t *testing.T) {
	// feeder.go writes clip.Kind straight through to showlog.Talk.Kind, so the
	// two packages' constants must stay equal by value — if one is renamed
	// without the other, talk_segment.kind silently becomes unrecognisable with
	// no test failure anywhere.
	require.Equal(t, showlog.KindSeam, ClipSeam)
	require.Equal(t, showlog.KindStationID, ClipStationID)
	require.Equal(t, showlog.KindMusing, ClipMusing)
	require.Equal(t, showlog.KindDaypartTransition, ClipDaypartTransition)
	require.Equal(t, showlog.KindWakeGreeting, ClipWakeGreeting)
}

// The director hands cadence.DueKind's answer straight to prepare as a clip
// kind, so a drift here would make Advance fall through to its default branch
// and stop resetting the format clock.
func TestClipKindConstantsMirrorCadence(t *testing.T) {
	require.Equal(t, cadence.KindSeam, ClipSeam)
	require.Equal(t, cadence.KindStationID, ClipStationID)
	require.Equal(t, cadence.KindMusing, ClipMusing)
	require.Equal(t, cadence.KindDaypartTransition, ClipDaypartTransition)
	require.Equal(t, cadence.KindWakeGreeting, ClipWakeGreeting)
}

func TestDJPayloadZeroListenersPresent(t *testing.T) {
	e := Entry{Title: "Tiểu Dương Dương", StartedAt: time.Now(), DurationS: 10}
	require.Contains(t, string(DJPayload(e, 0)), `"listeners":0`)
}
