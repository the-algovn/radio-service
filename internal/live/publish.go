package live

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/the-algovn/gopkg/obs"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/schedule"
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
	obs.InjectKafka(ctx, &rec.Headers)
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

// RequestQueuePayload is the radio.queue SSE frame: the committed next-up
// first, then the pending request queue in air order (listener FIFO, then AI
// FIFO), camelCase raw JSON.
//
// The next-up is USUALLY a shuffle pick, which is absent from the pending
// list. But the director pins a REQUEST when it promises one on air (spec
// 2026-07-29-radio-dj-seam-breaks §3), and a pinned request is still `ready`
// — so it is in items too. It is emitted once, from the pending row so it
// keeps its thumbnail, source, requester and reason, and skipped in the loop.
func RequestQueuePayload(items []request.Item, next *schedule.NextUp) []byte {
	out := make([]requestQueueItemJSON, 0, len(items)+1)
	if next != nil {
		e := requestQueueItemJSON{Title: next.Title, Artist: next.Channel}
		if next.RequestID != "" {
			for _, it := range items {
				if it.ID == next.RequestID {
					e = itemJSON(it)
					break
				}
			}
		}
		out = append(out, e)
	}
	for _, it := range items {
		if next != nil && next.RequestID != "" && it.ID == next.RequestID {
			continue // already emitted as the next-up, attributed
		}
		out = append(out, itemJSON(it))
	}
	b, _ := json.Marshal(out)
	return b
}

func itemJSON(it request.Item) requestQueueItemJSON {
	return requestQueueItemJSON{
		Title: it.Title, Artist: it.Channel, ThumbnailURL: it.ThumbnailURL,
		Source: it.Source, RequestedByName: it.DisplayName, Reason: it.Reason,
	}
}

// PublishQueueSnapshot reads the pending queue plus any committed next-up and
// publishes them — the one shared publisher used by the feeder, the ingest
// worker, the programmer and RequestTrack. nil producer = feeds disabled.
func PublishQueueSnapshot(ctx context.Context, p Producer, reqs request.Store, sched schedule.Store, logger *slog.Logger) {
	if p == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	items, err := reqs.Pending(ctx)
	if err != nil {
		logger.ErrorContext(ctx, "queue read for snapshot failed", "err", err)
		return
	}
	var next *schedule.NextUp
	if nu, ok, gerr := sched.GetNextUp(ctx); gerr != nil {
		logger.ErrorContext(ctx, "queue next-up read failed", "err", gerr)
	} else if ok {
		next = &nu
	}
	if err := p.Publish(ctx, TopicQueue, RequestQueuePayload(items, next)); err != nil {
		logger.ErrorContext(ctx, "queue snapshot publish failed", "err", err)
	}
}
