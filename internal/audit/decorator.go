package audit

import (
	"context"
	"log/slog"
	"time"

	"github.com/the-algovn/radio-service/internal/brain"
)

type ctxKey struct{}

// WithLabel tags ctx with the call-site label the decorator records. Set it at
// each call site immediately before Generate (e.g. "director:backsell").
func WithLabel(ctx context.Context, label string) context.Context {
	return context.WithValue(ctx, ctxKey{}, label)
}

// LabelFrom returns the label set by WithLabel, or "" if none.
func LabelFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}

// Clock is the decorator's time source (live.RealClock satisfies it).
type Clock interface{ Now() time.Time }

type auditedModel struct {
	inner    brain.Model
	store    Recorder
	provider string
	clock    Clock
	logger   *slog.Logger
}

// Wrap returns a brain.Model that records every Generate to store (best-effort)
// and returns the inner result unchanged. provider is the clean label
// (gemini|anthropic|fake); model is taken from inner.Name().
func Wrap(inner brain.Model, store Recorder, provider string, clock Clock, logger *slog.Logger) brain.Model {
	if logger == nil {
		logger = slog.Default()
	}
	return &auditedModel{inner: inner, store: store, provider: provider, clock: clock, logger: logger}
}

func (a *auditedModel) Name() string { return a.inner.Name() }

func (a *auditedModel) Generate(ctx context.Context, system, user string) (string, brain.Usage, error) {
	start := a.clock.Now()
	raw, usage, err := a.inner.Generate(ctx, system, user)
	rec := Rec{
		TS: start, Label: LabelFrom(ctx), Model: a.inner.Name(), Provider: a.provider,
		System: system, User: user, Output: raw,
		InTokens: usage.In, OutTokens: usage.Out,
		CostUSD:   brain.CostUSD(a.inner.Name(), usage),
		LatencyMS: int(a.clock.Now().Sub(start).Milliseconds()),
		Fake:      a.provider == "fake",
	}
	if err != nil {
		rec.Error = err.Error()
		rec.Output = ""
	}
	if rerr := a.store.Record(ctx, rec); rerr != nil {
		a.logger.Error("audit record failed", "err", rerr)
	}
	return raw, usage, err
}
