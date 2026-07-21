package live

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/playlist"
)

func TestNowPlayingPayload(t *testing.T) {
	e := Entry{YTID: "x", Title: "Lạc Trôi", Artist: "Sơn Tùng M-TP - Topic",
		StartedAt: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC), DurationS: 240}
	got := string(NowPlayingPayload(e, 3))
	// SPA-native camelCase JSON — parseNowPlaying requires kind/title/
	// startedAt/durationSeconds/listeners; numbers must be JSON numbers.
	require.JSONEq(t,
		`{"kind":"track","title":"Lạc Trôi","artist":"Sơn Tùng M-TP - Topic",
          "startedAt":"2026-07-21T12:00:00Z","durationSeconds":240,"listeners":3}`,
		got)
}

func TestNowPlayingPayloadZeroListeners(t *testing.T) {
	e := Entry{Title: "T", Artist: "A", StartedAt: time.Unix(0, 0).UTC(), DurationS: 1}
	// listeners must be PRESENT even at 0 (hand JSON, not protojson omit-zero).
	require.Contains(t, string(NowPlayingPayload(e, 0)), `"listeners":0`)
}

func TestOffAirPayload(t *testing.T) {
	require.JSONEq(t, `{"offAir":true}`, string(OffAirPayload()))
}

func TestQueueAfterAndPayload(t *testing.T) {
	items := []playlist.Item{
		{Position: 0, YTID: "a", Title: "A", Channel: "ch-a"},
		{Position: 1, YTID: "b", Title: "B", Channel: "ch-b"},
		{Position: 2, YTID: "c", Title: "C", Channel: "ch-c"},
	}
	// rotation order after current "b", wrapping, current excluded
	after := QueueAfter(items, "b")
	require.Equal(t, []string{"c", "a"}, []string{after[0].YTID, after[1].YTID})

	// unknown current — whole list in order
	after = QueueAfter(items, "zz")
	require.Len(t, after, 3)

	got := string(QueuePayload(items, "b"))
	require.JSONEq(t,
		`[{"title":"C","artist":"ch-c","hasDedication":false},
          {"title":"A","artist":"ch-a","hasDedication":false}]`, got)
}

func TestQueuePayloadEmptyIsBareArray(t *testing.T) {
	require.Equal(t, "[]", string(QueuePayload(nil, "whatever")))
}
