package live

import (
	"context"
	"log/slog"
	"time"
)

// Engine supervises broadcast sessions: it waits for a Poke (radioserver's
// GoOnAir/GoOffAir) or a 5s poll, and runs one Feeder session whenever the
// station is on-air. The poll doubles as the boot-resume trigger after a
// restart with on_air=true.
type Engine struct {
	f      *Feeder
	poke   chan struct{}
	logger *slog.Logger
}

func NewEngine(f *Feeder, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{f: f, poke: make(chan struct{}, 1), logger: logger}
}

// Poke is radioserver's Notifier: non-blocking, coalescing.
func (e *Engine) Poke() {
	select {
	case e.poke <- struct{}{}:
	default:
	}
}

func (e *Engine) Run(ctx context.Context) error {
	poll := time.NewTicker(5 * time.Second)
	defer poll.Stop()
	for {
		st, err := e.f.d.Store.GetStation(ctx)
		if err != nil {
			e.logger.Error("station read failed", "err", err)
		} else if st.OnAir {
			e.logger.Info("broadcast session starting")
			if err := e.f.RunSession(ctx); err != nil && ctx.Err() == nil {
				e.logger.Error("broadcast session failed", "err", err)
			} else {
				e.logger.Info("broadcast session ended")
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-e.poke:
		case <-poll.C:
		}
	}
}
