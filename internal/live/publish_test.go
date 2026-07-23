package live

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/schedule"
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
	got := string(RequestQueuePayload(items, nil))
	require.JSONEq(t, `[
	  {"title":"A","artist":"ch-a","thumbnailUrl":"https://img/a","hasDedication":false,"source":"listener","requestedByName":"Ngọc"},
	  {"title":"B","artist":"ch-b","hasDedication":false,"source":"ai"}
	]`, got)
	require.Equal(t, "[]", string(RequestQueuePayload(nil, nil))) // empty array, never null
}

func TestRequestQueuePayloadPrependsNextUp(t *testing.T) {
	items := []request.Item{{Title: "t-req", Channel: "c-req", Source: "listener"}}
	next := &schedule.NextUp{Title: "t-bed", Channel: "c-bed"}
	got := string(RequestQueuePayload(items, next))
	// next-up first, as a source-"" shuffle item, then the request
	require.Contains(t, got, `"title":"t-bed"`)
	require.Less(t, indexOf(got, "t-bed"), indexOf(got, "t-req"))
	require.Contains(t, got, `"source":"listener"`)
}

func indexOf(s, sub string) int { return strings.Index(s, sub) }

func TestNowPlayingPayloadProvenance(t *testing.T) {
	e := Entry{Title: "T", Artist: "A", StartedAt: time.Unix(0, 0).UTC(), DurationS: 1,
		Source: "ai", Reason: "hợp đêm mưa"}
	got := string(NowPlayingPayload(e, 0))
	require.Contains(t, got, `"source":"ai"`)
	require.Contains(t, got, `"reason":"hợp đêm mưa"`)
	require.NotContains(t, got, "requestedByName") // empty ⇒ absent

	lis := Entry{Title: "T", Artist: "A", StartedAt: time.Unix(0, 0).UTC(), DurationS: 1,
		Source: "listener", RequestedByName: "Ngọc"}
	got = string(NowPlayingPayload(lis, 0))
	require.Contains(t, got, `"source":"listener"`)
	require.Contains(t, got, `"requestedByName":"Ngọc"`)
	require.NotContains(t, got, "reason")

	// shuffle: all three absent — existing exact-JSON tests stay green
	got = string(NowPlayingPayload(Entry{Title: "T", StartedAt: time.Unix(0, 0).UTC(), DurationS: 1}, 0))
	require.NotContains(t, got, "source")
}

func TestRequestQueuePayloadReason(t *testing.T) {
	got := string(RequestQueuePayload([]request.Item{
		{Title: "B", Channel: "ch-b", Source: request.SourceAI, Reason: "đổi gió"},
	}, nil))
	require.Contains(t, got, `"reason":"đổi gió"`)
	got = string(RequestQueuePayload([]request.Item{
		{Title: "A", Channel: "ch-a", Source: request.SourceListener, DisplayName: "Ngọc"},
	}, nil))
	require.NotContains(t, got, "reason")
}
