package acquire

import (
	"context"
	"log/slog"
	"time"

	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/request"
)

// MaxAttempts is how many downloads a request gets before failed (spec §5).
const MaxAttempts = 3

const pollEvery = 5 * time.Second

type WorkerDeps struct {
	Requests request.Store
	Acquire  func(ctx context.Context, ytID, title, channel string) (library.Track, bool, error)
	Producer live.Producer // nil = feeds disabled
	Clock    live.Clock
	Logger   *slog.Logger
}

// Worker drains approved requests oldest-first through the acquire
// pipeline, flipping them ready (or failed after MaxAttempts).
type Worker struct{ d WorkerDeps }

func NewWorker(d WorkerDeps) *Worker {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Worker{d: d}
}

func (w *Worker) Run(ctx context.Context) error {
	tick := w.d.Clock.Tick(pollEvery)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
			w.RunOnce(ctx)
		}
	}
}

// RunOnce processes at most one approved request.
func (w *Worker) RunOnce(ctx context.Context) {
	it, found, err := w.d.Requests.OldestApproved(ctx)
	if err != nil {
		w.d.Logger.Error("worker: read approved failed", "err", err)
		return
	}
	if !found {
		return
	}
	if _, _, err := w.d.Acquire(ctx, it.YTID, it.Title, it.Channel); err != nil {
		attempts, berr := w.d.Requests.BumpAttempts(ctx, it.ID, err.Error())
		if berr != nil {
			w.d.Logger.Error("worker: bump attempts failed", "id", it.ID, "err", berr)
			return
		}
		w.d.Logger.Error("worker: acquire failed", "yt_id", it.YTID, "attempt", attempts, "err", err)
		if attempts >= MaxAttempts {
			if ferr := w.d.Requests.MarkFailed(ctx, it.ID, err.Error()); ferr != nil {
				w.d.Logger.Error("worker: mark failed failed", "id", it.ID, "err", ferr)
				return
			}
			live.PublishQueueSnapshot(ctx, w.d.Producer, w.d.Requests, w.d.Logger)
		}
		return
	}
	if err := w.d.Requests.MarkReady(ctx, it.ID); err != nil {
		w.d.Logger.Error("worker: mark ready failed", "id", it.ID, "err", err)
		return
	}
	w.d.Logger.Info("request ready", "yt_id", it.YTID, "source", it.Source)
	live.PublishQueueSnapshot(ctx, w.d.Producer, w.d.Requests, w.d.Logger)
}
