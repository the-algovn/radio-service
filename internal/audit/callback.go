package audit

import (
	"context"
	"log/slog"
	"time"

	"github.com/cloudwego/eino/callbacks"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/the-algovn/radio-service/internal/brain"
	"github.com/the-algovn/radio-service/internal/spend"
)

type ctxKey struct{}
type startKey struct{}

// WithLabel tags ctx with the call-site label the callback records. Set it at
// each call site immediately before Generate (e.g. "director:backsell").
// It survives the move to Eino because callbacks fire on direct component
// invocation, where RunInfo.Name carries the model id, not the call site.
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

// Clock is the callback's time source (live.RealClock satisfies it).
type Clock interface{ Now() time.Time }

// providerOf maps Eino's implementation identity (RunInfo.Type, e.g. "Claude")
// onto the clean provider labels the console inspector and spend ledger use.
func providerOf(einoType string) string {
	switch einoType {
	case "Claude", "claude", "Anthropic", "anthropic":
		return "anthropic"
	case "Gemini", "gemini":
		return "gemini"
	case "fake", "Fake":
		return "fake"
	default:
		return einoType
	}
}

type callbackHandler struct {
	store  Recorder
	ledger spend.Ledger
	clock  Clock
	logger *slog.Logger
}

// NewCallback returns the single Eino handler that records every LLM call to
// the audit store and appends its cost to the spend ledger. Register it once at
// boot with callbacks.AppendGlobalHandlers; it replaces audit.Wrap and every
// per-call-site Ledger.Append for Kind "llm".
func NewCallback(store Recorder, ledger spend.Ledger, clock Clock, logger *slog.Logger) callbacks.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	h := &callbackHandler{store: store, ledger: ledger, clock: clock, logger: logger}
	return callbacks.NewHandlerBuilder().
		OnStartFn(h.OnStart).
		OnEndFn(h.OnEnd).
		OnErrorFn(h.OnError).
		Build()
}

func (h *callbackHandler) OnStart(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
	// Stash the start time in ctx — the returned context is exactly the
	// mechanism Eino provides for correlating OnStart with OnEnd.
	ctx = context.WithValue(ctx, startKey{}, h.clock.Now())
	in := einomodel.ConvCallbackInput(input)
	if in == nil {
		return ctx
	}
	return context.WithValue(ctx, promptKey{}, promptsOf(in))
}

type promptKey struct{}
type prompts struct{ system, user string }

// promptsOf flattens the message list into the two fields audit.Rec stores.
// Multiple messages of a role concatenate, so a repair turn's appended user
// message is recorded rather than lost.
func promptsOf(in *einomodel.CallbackInput) prompts {
	var p prompts
	for _, m := range in.Messages {
		if m == nil {
			continue
		}
		switch m.Role {
		case "system":
			p.system = join(p.system, m.Content)
		default:
			p.user = join(p.user, m.Content)
		}
	}
	return p
}

func join(a, b string) string {
	if a == "" {
		return b
	}
	return a + "\n" + b
}

func (h *callbackHandler) rec(ctx context.Context, info *callbacks.RunInfo) Rec {
	p, _ := ctx.Value(promptKey{}).(prompts)
	start, ok := ctx.Value(startKey{}).(time.Time)
	if !ok {
		start = h.clock.Now()
	}
	provider := providerOf(info.Type)
	return Rec{
		TS: start, Label: LabelFrom(ctx), Model: info.Name, Provider: provider,
		System: p.system, User: p.user,
		LatencyMS: int(h.clock.Now().Sub(start).Milliseconds()),
		Fake:      provider == "fake",
	}
}

func (h *callbackHandler) OnEnd(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
	r := h.rec(ctx, info)
	out := einomodel.ConvCallbackOutput(output)
	if out != nil {
		if out.Message != nil {
			r.Output = out.Message.Content
		}
		if out.TokenUsage != nil {
			r.InTokens = out.TokenUsage.PromptTokens
			r.OutTokens = out.TokenUsage.CompletionTokens
		}
	}
	if !r.Fake {
		r.CostUSD = brain.CostUSD(r.Model, brain.Usage{In: r.InTokens, Out: r.OutTokens})
	}
	h.write(ctx, r)
	return ctx
}

func (h *callbackHandler) OnError(ctx context.Context, info *callbacks.RunInfo, err error) context.Context {
	r := h.rec(ctx, info)
	r.Error = err.Error()
	r.Output = ""
	h.write(ctx, r)
	return ctx
}

// write records the call and appends its spend. Both are best-effort: an
// accounting failure must never break the air.
func (h *callbackHandler) write(ctx context.Context, r Rec) {
	if err := h.store.Record(ctx, r); err != nil {
		h.logger.Error("audit record failed", "err", err)
	}
	// No tokens reported means nothing was billed (a failed call, or the fake
	// provider) — appending a zero line would only add noise to the ledger.
	if r.InTokens == 0 && r.OutTokens == 0 {
		return
	}
	if err := h.ledger.Append(ctx, spend.Line{
		TS: r.TS, Kind: "llm", Provider: r.Provider, Label: r.Label,
		InTokens: r.InTokens, OutTokens: r.OutTokens, CostUSD: r.CostUSD,
	}); err != nil {
		h.logger.Error("ledger append failed", "err", err)
	}
}
