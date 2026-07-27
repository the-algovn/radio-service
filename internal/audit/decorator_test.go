package audit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/audit"
	"github.com/the-algovn/radio-service/internal/brain"
)

type recStore struct {
	recs    []audit.Rec
	failErr error
}

func (r *recStore) Record(_ context.Context, rec audit.Rec) error {
	r.recs = append(r.recs, rec)
	return r.failErr
}

// stepClock advances 5ms on every Now() call, so a wrapped call's latency is
// deterministic (start=Now(), end=Now() → 5ms).
type stepClock struct{ t time.Time }

func (c *stepClock) Now() time.Time { c.t = c.t.Add(5 * time.Millisecond); return c.t }

type stubModel struct {
	name  string
	raw   string
	usage brain.Usage
	err   error
}

func (m stubModel) Name() string { return m.name }
func (m stubModel) Generate(context.Context, string, string) (string, brain.Usage, error) {
	return m.raw, m.usage, m.err
}

func TestWrapRecordsSuccess(t *testing.T) {
	store := &recStore{}
	inner := stubModel{name: "gemini-2.5-flash", raw: `{"script":"x"}`, usage: brain.Usage{In: 1000, Out: 200}}
	m := audit.Wrap(inner, store, "gemini", &stepClock{}, nil)

	ctx := audit.WithLabel(context.Background(), "director:backsell")
	raw, usage, err := m.Generate(ctx, "SYS", "USR")

	require.NoError(t, err)
	require.Equal(t, `{"script":"x"}`, raw)
	require.Equal(t, brain.Usage{In: 1000, Out: 200}, usage)
	require.Equal(t, "gemini-2.5-flash", m.Name())
	require.Len(t, store.recs, 1)
	r := store.recs[0]
	require.Equal(t, "director:backsell", r.Label)
	require.Equal(t, "gemini-2.5-flash", r.Model)
	require.Equal(t, "gemini", r.Provider)
	require.Equal(t, "SYS", r.System)
	require.Equal(t, "USR", r.User)
	require.Equal(t, `{"script":"x"}`, r.Output)
	require.Equal(t, 1000, r.InTokens)
	require.Equal(t, 200, r.OutTokens)
	require.InDelta(t, brain.CostUSD("gemini-2.5-flash", brain.Usage{In: 1000, Out: 200}), r.CostUSD, 1e-12)
	require.Equal(t, 5, r.LatencyMS)
	require.False(t, r.Fake)
	require.Empty(t, r.Error)
}

func TestWrapRecordsErrorWithEmptyOutput(t *testing.T) {
	store := &recStore{}
	inner := stubModel{name: "claude-x", err: errors.New("model down")}
	m := audit.Wrap(inner, store, "anthropic", &stepClock{}, nil)

	_, _, err := m.Generate(context.Background(), "s", "u")
	require.Error(t, err)
	require.Len(t, store.recs, 1)
	require.Equal(t, "model down", store.recs[0].Error)
	require.Empty(t, store.recs[0].Output)
	require.Equal(t, "", store.recs[0].Label) // no WithLabel set → empty
}

func TestWrapStoreFailureDoesNotBreakGenerate(t *testing.T) {
	store := &recStore{failErr: errors.New("db down")}
	inner := stubModel{name: "gemini-2.5-flash", raw: "ok", usage: brain.Usage{In: 1, Out: 1}}
	m := audit.Wrap(inner, store, "gemini", &stepClock{}, nil)

	raw, _, err := m.Generate(context.Background(), "s", "u")
	require.NoError(t, err) // audit failure swallowed
	require.Equal(t, "ok", raw)
}

func TestWrapFakeZeroCost(t *testing.T) {
	store := &recStore{}
	m := audit.Wrap(brain.Fake{}, store, "fake", &stepClock{}, nil)
	_, _, err := m.Generate(context.Background(), "s", "u")
	require.NoError(t, err)
	require.Len(t, store.recs, 1)
	require.True(t, store.recs[0].Fake)
	require.Zero(t, store.recs[0].CostUSD)
}
