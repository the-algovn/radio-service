package acquire

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/request"
)

type frame struct{ topic, value string }
type memProducer struct{ frames []frame }

func (p *memProducer) Publish(_ context.Context, topic string, value []byte) error {
	p.frames = append(p.frames, frame{topic, string(value)})
	return nil
}

func newWorker(reqs request.Store, acq func(context.Context, string, string, string) (library.Track, bool, error), prod live.Producer) *Worker {
	return NewWorker(WorkerDeps{Requests: reqs, Acquire: acq, Producer: prod, Clock: live.RealClock()})
}

func TestWorkerMarksReadyAndPublishes(t *testing.T) {
	reqs := request.NewMemStore()
	ctx := context.Background()
	it, err := reqs.Create(ctx, request.Item{Source: request.SourceListener, RequestedBy: "u1",
		YTID: "yta", Title: "T", Channel: "C", DurationS: 240, Status: request.StatusApproved})
	require.NoError(t, err)

	prod := &memProducer{}
	w := newWorker(reqs, func(_ context.Context, ytID, title, channel string) (library.Track, bool, error) {
		require.Equal(t, "yta", ytID)
		require.Equal(t, "T", title)
		require.Equal(t, "C", channel)
		return library.Track{YTID: ytID}, false, nil
	}, prod)

	w.RunOnce(ctx)
	next, found, err := reqs.NextReady(ctx)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, it.ID, next.ID)
	require.Len(t, prod.frames, 1)
	require.Equal(t, live.TopicQueue, prod.frames[0].topic)
}

func TestWorkerRetriesThenFails(t *testing.T) {
	reqs := request.NewMemStore()
	ctx := context.Background()
	_, err := reqs.Create(ctx, request.Item{Source: request.SourceListener, RequestedBy: "u1",
		YTID: "yta", Title: "T", Channel: "C", DurationS: 240, Status: request.StatusApproved})
	require.NoError(t, err)

	prod := &memProducer{}
	w := newWorker(reqs, func(context.Context, string, string, string) (library.Track, bool, error) {
		return library.Track{}, false, errors.New("yt-dlp: timeout")
	}, prod)

	w.RunOnce(ctx) // attempt 1
	w.RunOnce(ctx) // attempt 2
	mine, err := reqs.ByUser(ctx, "u1", 1)
	require.NoError(t, err)
	require.Equal(t, request.StatusApproved, mine[0].Status) // still retrying
	require.Empty(t, prod.frames)                            // no queue change yet

	w.RunOnce(ctx) // attempt 3 → failed
	mine, err = reqs.ByUser(ctx, "u1", 1)
	require.NoError(t, err)
	require.Equal(t, request.StatusFailed, mine[0].Status)
	require.Equal(t, "yt-dlp: timeout", mine[0].FailReason)
	require.Len(t, prod.frames, 1) // failure removes it from the queue → snapshot
}

// A probed-too-long error must fail the request on the FIRST RunOnce, with
// no BumpAttempts retry cycle — a hostile client can't buy three attempts'
// worth of downloads just by lying about duration.
func TestWorkerTooLongFailsImmediatelyNoRetry(t *testing.T) {
	reqs := request.NewMemStore()
	ctx := context.Background()
	_, err := reqs.Create(ctx, request.Item{Source: request.SourceListener, RequestedBy: "u1",
		YTID: "yta", Title: "T", Channel: "C", DurationS: 240, Status: request.StatusApproved})
	require.NoError(t, err)

	prod := &memProducer{}
	w := newWorker(reqs, func(context.Context, string, string, string) (library.Track, bool, error) {
		return library.Track{}, false, fmt.Errorf("probed 36000s: %w", ErrTooLong)
	}, prod)

	w.RunOnce(ctx)
	mine, err := reqs.ByUser(ctx, "u1", 1)
	require.NoError(t, err)
	require.Equal(t, request.StatusFailed, mine[0].Status)
	require.Equal(t, "bài dài quá mười phút, đài không phát được", mine[0].FailReason)
	require.Zero(t, mine[0].Attempts) // no BumpAttempts retry cycle
	require.Len(t, prod.frames, 1)    // exactly one queue snapshot published
}

func TestWorkerIdleWhenNothingApproved(t *testing.T) {
	prod := &memProducer{}
	w := newWorker(request.NewMemStore(), func(context.Context, string, string, string) (library.Track, bool, error) {
		t.Fatal("acquire must not run")
		return library.Track{}, false, nil
	}, prod)
	w.RunOnce(context.Background())
	require.Empty(t, prod.frames)
}

func TestWorkerRunStopsOnCancel(t *testing.T) {
	w := newWorker(request.NewMemStore(), func(context.Context, string, string, string) (library.Track, bool, error) {
		return library.Track{}, false, nil
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}
