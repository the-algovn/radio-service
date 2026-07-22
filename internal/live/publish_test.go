package live

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/request"
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

func TestRequestQueuePayload(t *testing.T) {
	items := []request.Item{
		{Title: "A", Channel: "ch-a", Source: request.SourceListener,
			DisplayName: "Ngọc", ThumbnailURL: "https://img/a"},
		{Title: "B", Channel: "ch-b", Source: request.SourceAI},
	}
	got := string(RequestQueuePayload(items))
	require.JSONEq(t, `[
	  {"title":"A","artist":"ch-a","thumbnailUrl":"https://img/a","hasDedication":false,"source":"listener","requestedByName":"Ngọc"},
	  {"title":"B","artist":"ch-b","hasDedication":false,"source":"ai"}
	]`, got)
	require.Equal(t, "[]", string(RequestQueuePayload(nil))) // empty array, never null
}
