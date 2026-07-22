package live

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEngineStartsSessionOnPoke(t *testing.T) {
	store, lib, reqs := newFixture(t, "a", "b") // on-air fixture
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())
	e := NewEngine(f, nil)

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
	e := NewEngine(f, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	e.Poke()
	time.Sleep(20 * time.Millisecond)
	require.Zero(t, enc.count()) // no session while off-air
	cancel()
	require.NoError(t, <-done)
}
