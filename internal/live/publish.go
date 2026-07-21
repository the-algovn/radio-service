package live

import (
	"context"
	"encoding/json"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/the-algovn/radio-service/internal/playlist"
)

// SSE topics — the gateway routes these to channels radio.nowplaying /
// radio.queue and passes record values through VERBATIM as SSE data frames,
// so every payload here is exactly what the SPA parses (camelCase raw JSON,
// never protojson).
const (
	TopicNowPlaying = "sse.radio.nowplaying"
	TopicQueue      = "sse.radio.queue"
)

type Producer interface {
	Publish(ctx context.Context, topic string, value []byte) error
}

// KafkaProducer mirrors the-button's franz-go setup.
type KafkaProducer struct{ cl *kgo.Client }

func NewKafkaProducer(brokers []string) (*KafkaProducer, error) {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.RecordDeliveryTimeout(10*time.Second),
	)
	if err != nil {
		return nil, err
	}
	return &KafkaProducer{cl: cl}, nil
}

func (p *KafkaProducer) Publish(ctx context.Context, topic string, value []byte) error {
	// Fixed key: single partition, strict ordering per topic.
	rec := &kgo.Record{Topic: topic, Key: []byte("radio"), Value: value}
	return p.cl.ProduceSync(ctx, rec).FirstErr()
}

func (p *KafkaProducer) Close() { p.cl.Close() }

type nowPlayingJSON struct {
	Kind            string `json:"kind"`
	Title           string `json:"title"`
	Artist          string `json:"artist,omitempty"`
	StartedAt       string `json:"startedAt"`
	DurationSeconds int    `json:"durationSeconds"`
	Listeners       int    `json:"listeners"`
}

func NowPlayingPayload(e Entry, listeners int) []byte {
	b, _ := json.Marshal(nowPlayingJSON{
		Kind: "track", Title: e.Title, Artist: e.Artist,
		StartedAt: e.StartedAt.UTC().Format(time.RFC3339),
		DurationSeconds: e.DurationS, Listeners: listeners,
	})
	return b
}

func OffAirPayload() []byte { return []byte(`{"offAir":true}`) }

// QueueAfter returns the rotation order starting after currentYTID
// (wrapping), excluding the current track. Unknown current → the whole list.
func QueueAfter(items []playlist.Item, currentYTID string) []playlist.Item {
	cur := -1
	for i, it := range items {
		if it.YTID == currentYTID {
			cur = i
			break
		}
	}
	if cur < 0 {
		return items
	}
	out := make([]playlist.Item, 0, len(items)-1)
	out = append(out, items[cur+1:]...)
	out = append(out, items[:cur]...)
	return out
}

type queueItemJSON struct {
	Title         string `json:"title"`
	Artist        string `json:"artist,omitempty"`
	HasDedication bool   `json:"hasDedication"`
}

func QueuePayload(items []playlist.Item, currentYTID string) []byte {
	after := QueueAfter(items, currentYTID)
	out := make([]queueItemJSON, 0, len(after))
	for _, it := range after {
		out = append(out, queueItemJSON{Title: it.Title, Artist: it.Channel})
	}
	b, _ := json.Marshal(out)
	return b
}
