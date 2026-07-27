package programmer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eino-contrib/jsonschema"
	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/audit"
	"github.com/the-algovn/radio-service/internal/ingest"
	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/spend"
)

// scriptedModel scripts a Generate call sequence: each call in order consumes
// the next reply/err pair, and records the label and user prompt it was
// called with, so tests can assert both the shape of the conversation and
// which phase each call belonged to.
type scriptedModel struct {
	replies []string
	errs    []error
	calls   []string // user prompts, in order
	labels  []string // audit label per call
}

func (m *scriptedModel) Name() string     { return "fake-scripted" }
func (m *scriptedModel) Provider() string { return "fake" }

func (m *scriptedModel) Generate(ctx context.Context, _, user string, _ *jsonschema.Schema) (string, error) {
	i := len(m.calls)
	m.calls = append(m.calls, user)
	m.labels = append(m.labels, audit.LabelFrom(ctx))
	if i < len(m.errs) && m.errs[i] != nil {
		return "", m.errs[i]
	}
	if i >= len(m.replies) {
		return "", errors.New("no reply scripted")
	}
	return m.replies[i], nil
}

type frame struct{ topic, value string }
type memProducer struct{ frames []frame }

func (p *memProducer) Publish(_ context.Context, topic string, value []byte) error {
	p.frames = append(p.frames, frame{topic, string(value)})
	return nil
}

// --- RunOnce gates: every gate must stay a quiet skip, never reaching the model. ---

func TestGatesProduceNoModelCalls(t *testing.T) {
	t.Run("off-air", func(t *testing.T) {
		h := newHarness(t)
		_, err := h.station.GoOffAir(h.ctx)
		require.NoError(t, err)
		h.prog.RunOnce(h.ctx)
		require.Zero(t, h.model.calls)
	})

	t.Run("ai disabled", func(t *testing.T) {
		h := newHarness(t)
		_, err := h.station.SetAIEnabled(h.ctx, false)
		require.NoError(t, err)
		h.prog.RunOnce(h.ctx)
		require.Zero(t, h.model.calls)
	})

	t.Run("zero listeners", func(t *testing.T) {
		h := newHarness(t)
		h.prog.RunOnce(h.ctx) // nobody has beat: listener count is 0
		require.Zero(t, h.model.calls)
	})

	t.Run("queue depth at target", func(t *testing.T) {
		h := newHarness(t)
		for _, id := range []string{"q1", "q2", "q3"} {
			_, err := h.requests.Create(h.ctx, request.Item{
				Source: request.SourceListener, RequestedBy: "u", YTID: id, Title: id, Channel: "c",
				DurationS: 100, Status: request.StatusApproved,
			})
			require.NoError(t, err)
		}
		h.prog.RunOnce(h.ctx)
		require.Zero(t, h.model.calls)
	})

	t.Run("budget cap hit", func(t *testing.T) {
		h := newHarness(t)
		require.NoError(t, h.listeners.Beat(h.ctx, "tab-1"))
		require.NoError(t, h.ledger.Append(h.ctx, spend.Line{TS: time.Now(), Kind: "llm", Provider: "fake", CostUSD: 1.0}))
		h.prog.RunOnce(h.ctx)
		require.Zero(t, h.model.calls)
	})
}

// FakeModeReSpinsLibraryWithoutModel: keyless mode never calls the model —
// it goes straight to the tier-3 deterministic re-spin.
func TestFakeModeReSpinsLibraryWithoutModel(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.listeners.Beat(h.ctx, "tab-1"))
	require.NoError(t, h.lib.Add(h.ctx, library.Track{YTID: "libX", Title: "Lib X", DurationS: 200}))
	h.prog.d.Fake = true

	h.prog.RunOnce(h.ctx)

	require.Zero(t, h.model.calls)
	pending, err := h.requests.Pending(h.ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "libX", pending[0].YTID)
	require.Equal(t, request.SourceAI, pending[0].Source)
	require.Equal(t, request.StatusReady, pending[0].Status)
	require.Empty(t, pending[0].Reason, "a re-spin must not fabricate a reason the DJ would speak")
}

// --- decide(): the whole two-phase decision, wired end to end. ---

// The happy path: intent → resolve → choose → enqueue, with the reason attached
// to the track the model actually saw.
func TestDecideEnqueuesWithMatchingReason(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.lib.Add(h.ctx, library.Track{YTID: "lib1", Title: "Lib One", DurationS: 200}))
	h.model.replies = []string{
		`{"searches":[],"library_query":"","respins":["lib1"],"note":"đêm dịu"}`,
		`{"picks":[{"yt_id":"lib1","reason":"bài này hợp đêm mưa"}]}`,
	}

	h.prog.decide(h.ctx, 0)

	pending, err := h.requests.Pending(h.ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "lib1", pending[0].YTID)
	require.Equal(t, "bài này hợp đêm mưa", pending[0].Reason)
	require.Equal(t, request.SourceAI, pending[0].Source)
	require.Equal(t, request.StatusReady, pending[0].Status, "a cached library track is ready, not approved")
	require.Len(t, h.model.calls, 2, "exactly two LLM calls per decision")
}

// A YouTube pick that is not cached must be enqueued as approved so ingest
// fetches it.
func TestDecideEnqueuesUncachedAsApproved(t *testing.T) {
	h := newHarness(t)
	h.search.byQuery["q"] = []ingest.Candidate{{YTID: "yt1", Title: "YT One", DurationS: 200}}
	h.model.replies = []string{
		`{"searches":["q"],"respins":[]}`,
		`{"picks":[{"yt_id":"yt1","reason":"mới lạ"}]}`,
	}

	h.prog.decide(h.ctx, 0)

	pending, _ := h.requests.Pending(h.ctx)
	require.Len(t, pending, 1)
	require.Equal(t, request.StatusApproved, pending[0].Status)
}

// Two picks per decision is what keeps the doubled call count cost-neutral.
func TestDecideEnqueuesUpToWant(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.lib.Add(h.ctx, library.Track{YTID: "l1", Title: "One", DurationS: 200}))
	require.NoError(t, h.lib.Add(h.ctx, library.Track{YTID: "l2", Title: "Two", DurationS: 200}))
	h.model.replies = []string{
		`{"respins":["l1","l2"]}`,
		`{"picks":[{"yt_id":"l1","reason":"a"},{"yt_id":"l2","reason":"b"}]}`,
	}

	h.prog.decide(h.ctx, 0)

	pending, _ := h.requests.Pending(h.ctx)
	require.Len(t, pending, 2)
}

// An empty pool must still program something — never a silent skip.
func TestDecideFallsBackToRespinOnEmptyPool(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.lib.Add(h.ctx, library.Track{YTID: "shelf", Title: "Shelf", DurationS: 200}))
	h.model.replies = []string{`{"searches":[],"respins":[],"library_query":""}`}

	h.prog.decide(h.ctx, 0)

	pending, _ := h.requests.Pending(h.ctx)
	require.Len(t, pending, 1)
	require.Equal(t, "shelf", pending[0].YTID)
	require.Empty(t, pending[0].Reason, "a fallback re-spin must not fabricate a reason the DJ would speak")
	require.Len(t, h.model.calls, 1, "no phase-2 call when the pool is empty")
}

// One repair turn, then the deterministic fallback.
func TestDecideRepairsThenFallsBack(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.lib.Add(h.ctx, library.Track{YTID: "l1", Title: "One", DurationS: 200}))
	h.model.replies = []string{
		`{"respins":["l1"]}`,
		`{"picks":[{"yt_id":"ghost","reason":"invented"}]}`,       // invalid
		`{"picks":[{"yt_id":"ghost","reason":"still invented"}]}`, // repair also invalid
	}

	h.prog.decide(h.ctx, 0)

	require.Len(t, h.model.calls, 3, "intent + choose + one repair")
	pending, _ := h.requests.Pending(h.ctx)
	require.Len(t, pending, 1)
	require.Empty(t, pending[0].Reason, "fell through to respin")
}

func TestDecideRepairSucceeds(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.lib.Add(h.ctx, library.Track{YTID: "l1", Title: "One", DurationS: 200}))
	h.model.replies = []string{
		`{"respins":["l1"]}`,
		`{"picks":[{"yt_id":"ghost","reason":"invented"}]}`,
		`{"picks":[{"yt_id":"l1","reason":"đã sửa"}]}`,
	}

	h.prog.decide(h.ctx, 0)

	pending, _ := h.requests.Pending(h.ctx)
	require.Len(t, pending, 1)
	require.Equal(t, "đã sửa", pending[0].Reason)
	// The repair call must carry its own label — the console inspector groups
	// LLM calls by exact label match, so a mislabelled repair call would
	// silently vanish from the inspector.
	require.Equal(t, []string{"programmer:intent", "programmer:choose", "programmer:repair"}, h.model.labels)
}

// Tier 1: a transient error gets one retry.
func TestDecideRetriesTransientError(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.lib.Add(h.ctx, library.Track{YTID: "l1", Title: "One", DurationS: 200}))
	h.model.errs = []error{context.DeadlineExceeded, nil, nil}
	h.model.replies = []string{
		"",
		`{"respins":["l1"]}`,
		`{"picks":[{"yt_id":"l1","reason":"ổn"}]}`,
	}

	h.prog.decide(h.ctx, 0)

	pending, _ := h.requests.Pending(h.ctx)
	require.Len(t, pending, 1)
	require.Equal(t, "ổn", pending[0].Reason)
}

// Both attempts failing falls through to the deterministic fallback.
func TestDecideFallsBackWhenIntentAlwaysFails(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.lib.Add(h.ctx, library.Track{YTID: "l1", Title: "One", DurationS: 200}))
	h.model.errs = []error{context.DeadlineExceeded, context.DeadlineExceeded}
	h.model.replies = []string{"", ""}

	h.prog.decide(h.ctx, 0)

	pending, _ := h.requests.Pending(h.ctx)
	require.Len(t, pending, 1)
	require.Empty(t, pending[0].Reason)
}

// Each phase must be labelled so the console inspector can tell them apart.
func TestDecideLabelsEachPhase(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.lib.Add(h.ctx, library.Track{YTID: "l1", Title: "One", DurationS: 200}))
	h.model.replies = []string{
		`{"respins":["l1"]}`,
		`{"picks":[{"yt_id":"l1","reason":"x"}]}`,
	}

	h.prog.decide(h.ctx, 0)

	require.Equal(t, []string{"programmer:intent", "programmer:choose"}, h.model.labels)
}

// A missing/misconfigured persona file must not go silent on every tick —
// decide() falls through to respin() same as any other failure.
func TestDecideFallsBackWhenPersonaLoadFails(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.lib.Add(h.ctx, library.Track{YTID: "l1", Title: "One", DurationS: 200}))
	h.prog.d.PersonaDir = t.TempDir() // fresh dir, no persona file written → persona.Load errors

	h.prog.decide(h.ctx, 0)

	require.Zero(t, h.model.calls, "persona failed before any model call")
	pending, _ := h.requests.Pending(h.ctx)
	require.Len(t, pending, 1)
	require.Equal(t, "l1", pending[0].YTID)
	require.Empty(t, pending[0].Reason, "a fallback re-spin must not fabricate a reason")
}

// errCountLibrary wraps a real library.Library and fails only Count, the
// cheapest way to make buildBrief's own error path fire without touching the
// harness. respin still works through it since it only calls AllIDs and Get.
type errCountLibrary struct{ library.Library }

func (errCountLibrary) Count(context.Context, string) (int64, error) {
	return 0, errors.New("count boom")
}

// A transient DB read failure inside buildBrief must not silently skip the
// decision — decide() falls through to respin() same as an LLM failure.
func TestDecideFallsBackWhenBuildBriefFails(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.lib.Add(h.ctx, library.Track{YTID: "l1", Title: "One", DurationS: 200}))
	h.prog.d.Library = errCountLibrary{h.lib}

	h.prog.decide(h.ctx, 0)

	require.Zero(t, h.model.calls, "brief failed before any model call")
	pending, _ := h.requests.Pending(h.ctx)
	require.Len(t, pending, 1)
	require.Equal(t, "l1", pending[0].YTID)
	require.Empty(t, pending[0].Reason, "a fallback re-spin must not fabricate a reason")
}

// --- shared filtering behavior, exercised through decide()'s respin fallback. ---

// filtered must still catch recently-aired and already-queued tracks when
// decide() falls all the way through to respin().
func TestDecideRespinFiltersRecentAndQueued(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.lib.Add(h.ctx, library.Track{YTID: "only", Title: "Only", DurationS: 200}))
	require.NoError(t, h.airlog.Append(h.ctx, live.Entry{YTID: "only", Title: "t", Artist: "a",
		StartedAt: time.Now().Add(-10 * time.Minute), DurationS: 200}))
	h.model.replies = []string{`{"respins":[]}`}

	h.prog.decide(h.ctx, 0)

	pending, _ := h.requests.Pending(h.ctx)
	require.Empty(t, pending, "the only library track was filtered as recently aired")
}
