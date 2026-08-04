package live

import (
	"context"
	"log/slog"
	"time"
)

// BroadcastSessions records when the station was on the air. nil = feature
// absent.
//
// The engine owns this, not radioserver's GoOnAir/GoOffAir, for three
// independent reasons: a boot resume never calls GoOnAir at all (the 5s poll
// below is the resume trigger); GoOnAir is "an unconditional idempotent flip"
// that cannot tell a transition from a repeat click, so it would append a
// duplicate row per click; and a write inside station.PGStore's transaction
// would turn a view feature into a new way to fail going on air.
//
// Declared over stdlib types on purpose — internal/broadcast satisfies it
// structurally, so this package need not import it.
type BroadcastSessions interface {
	Open(ctx context.Context, at time.Time) error
	Close(ctx context.Context, at time.Time) error
	CloseOrphans(ctx context.Context) (int64, error)
}

// Engine supervises broadcast sessions: it waits for a Poke (radioserver's
// GoOnAir/GoOffAir) or a 5s poll, and runs one Feeder session whenever the
// station is on-air. The poll doubles as the boot-resume trigger after a
// restart with on_air=true.
type Engine struct {
	f        *Feeder
	sessions BroadcastSessions
	poke     chan struct{}
	logger   *slog.Logger
}

func NewEngine(f *Feeder, sessions BroadcastSessions, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{f: f, sessions: sessions, poke: make(chan struct{}, 1), logger: logger}
}

// Poke is radioserver's Notifier: non-blocking, coalescing.
func (e *Engine) Poke() {
	select {
	case e.poke <- struct{}{}:
	default:
	}
}

func (e *Engine) Run(ctx context.Context) error {
	// Boot reconciliation, BEFORE any session can be opened.
	if e.sessions != nil {
		if n, err := e.sessions.CloseOrphans(ctx); err != nil {
			e.logger.ErrorContext(ctx, "closing orphan broadcast sessions failed", "err", err)
		} else if n > 0 {
			e.logger.InfoContext(ctx, "closed orphan broadcast sessions", "n", n)
		}
	}

	poll := time.NewTicker(5 * time.Second)
	defer poll.Stop()
	for {
		st, err := e.f.d.Store.GetStation(ctx)
		if err != nil {
			e.logger.ErrorContext(ctx, "station read failed", "err", err)
		} else if st.OnAir {
			e.logger.InfoContext(ctx, "broadcast session starting")
			// Best-effort: a bookkeeping failure must never stop a broadcast.
			if e.sessions != nil {
				if err := e.sessions.Open(ctx, e.f.d.Clock.Now()); err != nil {
					e.logger.ErrorContext(ctx, "opening broadcast session failed", "err", err)
				}
			}
			if err := e.f.RunSession(ctx); err != nil && ctx.Err() == nil {
				e.logger.ErrorContext(ctx, "broadcast session failed", "err", err)
			} else {
				e.logger.InfoContext(ctx, "broadcast session ended")
			}
			if e.sessions != nil {
				// WithoutCancel: a shutdown cancels ctx before we get here, and
				// a session left open would be reconciled as an orphan on the
				// next boot rather than closed at its real end.
				cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
				if err := e.sessions.Close(cctx, e.f.d.Clock.Now()); err != nil {
					e.logger.ErrorContext(cctx, "closing broadcast session failed", "err", err)
				}
				cancel()
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
