package live

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/the-algovn/radio-service/internal/request"
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
	Source          string `json:"source,omitempty"`
	RequestedByName string `json:"requestedByName,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

func NowPlayingPayload(e Entry, listeners int) []byte {
	b, _ := json.Marshal(nowPlayingJSON{
		Kind: "track", Title: e.Title, Artist: e.Artist,
		// RFC3339Nano preserves sub-second sample-clock precision (== RFC3339 for whole seconds).
		StartedAt:       e.StartedAt.UTC().Format(time.RFC3339Nano),
		DurationSeconds: e.DurationS, Listeners: listeners,
		Source: e.Source, RequestedByName: e.RequestedByName, Reason: e.Reason,
	})
	return b
}

// DJPayload is the now-playing frame for an airing talk break (kind "dj").
// listeners rides along so the SPA chip doesn't drop to 0 during breaks; the
// script text never touches this world-readable channel.
func DJPayload(e Entry, listeners int) []byte {
	b, _ := json.Marshal(nowPlayingJSON{
		Kind: "dj", Title: e.Title,
		StartedAt:       e.StartedAt.UTC().Format(time.RFC3339Nano),
		DurationSeconds: e.DurationS, Listeners: listeners,
	})
	return b
}

func OffAirPayload() []byte { return []byte(`{"offAir":true}`) }

type requestQueueItemJSON struct {
	Title           string `json:"title"`
	Artist          string `json:"artist,omitempty"`
	ThumbnailURL    string `json:"thumbnailUrl,omitempty"`
	HasDedication   bool   `json:"hasDedication"`
	Source          string `json:"source"`
	RequestedByName string `json:"requestedByName,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

// RequestQueuePayload is the radio.queue SSE frame: the pending request
// queue in air order (listener FIFO, then AI FIFO), camelCase raw JSON.
// Empty queue → "[]" — shuffle territory is deliberately not previewed.
func RequestQueuePayload(items []request.Item) []byte {
	out := make([]requestQueueItemJSON, 0, len(items))
	for _, it := range items {
		out = append(out, requestQueueItemJSON{
			Title: it.Title, Artist: it.Channel, ThumbnailURL: it.ThumbnailURL,
			Source: it.Source, RequestedByName: it.DisplayName, Reason: it.Reason,
		})
	}
	b, _ := json.Marshal(out)
	return b
}

// PublishQueueSnapshot reads the pending queue and publishes it — the one
// shared publisher used by the feeder, the ingest worker, the programmer
// and RequestTrack. nil producer = feeds disabled (dev without Kafka).
func PublishQueueSnapshot(ctx context.Context, p Producer, reqs request.Store, logger *slog.Logger) {
	if p == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	items, err := reqs.Pending(ctx)
	if err != nil {
		logger.Error("queue read for snapshot failed", "err", err)
		return
	}
	if err := p.Publish(ctx, TopicQueue, RequestQueuePayload(items)); err != nil {
		logger.Error("queue snapshot publish failed", "err", err)
	}
}
