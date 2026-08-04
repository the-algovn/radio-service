package live

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeSessions struct {
	mu      sync.Mutex
	order   []string // "orphans" | "open" | "close", in call order
	opened  []time.Time
	closed  []time.Time
	orphans int64
}

func (f *fakeSessions) Open(_ context.Context, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, "open")
	f.opened = append(f.opened, at)
	return nil
}

func (f *fakeSessions) Close(_ context.Context, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, "close")
	f.closed = append(f.closed, at)
	return nil
}

func (f *fakeSessions) CloseOrphans(_ context.Context, _ time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, "orphans")
	return f.orphans, nil
}

// ops records the call order so the boot-vs-open ordering is provable.
func (f *fakeSessions) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

func TestEngineClosesOrphansBeforeOpeningAnything(t *testing.T) {
	// A pod killed on air leaves a row open forever. If a new session opens
	// first, `ended_at IS NULL` matches two rows and stops identifying the
	// current broadcast — so reconciliation must run FIRST, not merely early.
	s := &fakeSessions{orphans: 1}
	store, lib, reqs := newFixture(t, "a", "b") // on-air fixture
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())
	e := NewEngine(f, s, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	e.Poke()
	require.Eventually(t, func() bool {
		clk.step(250 * time.Millisecond)
		return len(prod.byTopic(TopicNowPlaying)) >= 1
	}, 2*time.Second, 5*time.Millisecond)

	cancel()
	require.NoError(t, <-done)

	// Assert the ORDER, not an exact transcript. After cancel() the loop's
	// select may pick the buffered Poke over ctx.Done(), and the mem-backed
	// GetStation does not fail on a cancelled ctx, so a second open is
	// possible and harmless. What must hold is that reconciliation ran once,
	// before anything opened.
	ops := s.ops()
	require.GreaterOrEqual(t, len(ops), 3)
	require.Equal(t, "orphans", ops[0], "reconciliation must precede the first open")
	require.Equal(t, "open", ops[1])
	require.Contains(t, ops, "close", "a finished session must be closed")
	require.Equal(t, 1, countOf(ops, "orphans"), "reconciliation runs once per process")
}

func countOf(ss []string, want string) int {
	n := 0
	for _, s := range ss {
		if s == want {
			n++
		}
	}
	return n
}

func TestEngineWithNilSessionsRunsNormally(t *testing.T) {
	store, lib, reqs := newFixture(t, "a", "b")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())
	e := NewEngine(f, nil, nil) // nil sessions = feature absent

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	e.Poke()
	require.Eventually(t, func() bool {
		clk.step(250 * time.Millisecond)
		return len(prod.byTopic(TopicNowPlaying)) >= 1
	}, 2*time.Second, 5*time.Millisecond)
	cancel()
	require.NoError(t, <-done) // must not panic
}

func TestEngineStartsSessionOnPoke(t *testing.T) {
	store, lib, reqs := newFixture(t, "a", "b") // on-air fixture
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())
	e := NewEngine(f, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	e.Poke()
	// a session starts: encoder appears, now-playing published
	require.Eventually(t, func() bool {
		clk.step(250 * time.Millisecond)
		return len(prod.byTopic(TopicNowPlaying)) >= 1
	}, 2*time.Second, 5*time.Millisecond)

	cancel()
	require.NoError(t, <-done)
}

func TestEngineIdleWhenOffAir(t *testing.T) {
	store, lib, reqs := newFixture(t, "a")
	_, err := store.GoOffAir(context.Background())
	require.NoError(t, err)
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())
	e := NewEngine(f, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	e.Poke()
	time.Sleep(20 * time.Millisecond)
	require.Zero(t, enc.count()) // no session while off-air
	cancel()
	require.NoError(t, <-done)
}
