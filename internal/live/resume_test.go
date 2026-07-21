package live

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestResumeOffset(t *testing.T) {
	e := Entry{StartedAt: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC), DurationS: 240}
	off, expired := ResumeOffset(e, e.StartedAt.Add(90*time.Second))
	require.False(t, expired)
	require.InDelta(t, 90.0, off, 0.001)

	_, expired = ResumeOffset(e, e.StartedAt.Add(241*time.Second))
	require.True(t, expired)

	off, expired = ResumeOffset(e, e.StartedAt.Add(-5*time.Second)) // clock skew
	require.False(t, expired)
	require.Zero(t, off)
}

func TestEncoderCrashStartsNewSessionAndResumes(t *testing.T) {
	store, lib := newFixture(t, "a", "b")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	dec := &offsetRecordingDecoder{inner: fakeDecoder{bytesPerTrack: chunkBytes * 4}}
	f := NewFeeder(FeederDeps{
		Store: store, Library: lib,
		Log: NewMemAirLog(), Listeners: NewMemListeners(time.Now),
		Fetch:   func(_ context.Context, id, _ string) (string, error) { return "/fake/" + id, nil },
		Decoder: dec, Encoder: enc, Producer: prod, Clock: clk, Dir: t.TempDir(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()

	// feed one chunk, then kill the encoder mid-track
	for enc.count() < 1 {
		time.Sleep(time.Millisecond)
	}
	clk.step(250 * time.Millisecond) // one chunk fed (0.25s of audio)
	time.Sleep(5 * time.Millisecond)
	enc.sessions[0].fail(context.DeadlineExceeded) // simulated crash

	// a second encoder session must start and the decoder re-open with
	// offset ≈ 0.25s (one chunk aired)
	for enc.count() < 2 {
		clk.step(250 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	require.InDelta(t, 0.25, dec.lastOffset(), 0.01)
	cancel()
	<-done
}

// TestCrashResumeCapSkipsTrackAfterThreeAttempts covers the retry cap: a
// track whose encoder keeps crashing must not loop forever. After 3
// crash-restarts (4 total attempts) the feeder gives up on the track, starts
// one more fresh session to keep broadcasting, and moves on to the next
// track in rotation.
func TestCrashResumeCapSkipsTrackAfterThreeAttempts(t *testing.T) {
	store, lib := newFixture(t, "a", "b")
	// sessions 0..3 (the initial attempt + 3 restarts) auto-crash; session 4
	// (the 5th, started after the cap is exhausted) survives so track 'b'
	// can actually air on it.
	enc := &crashingEncoder{aliveFrom: 4}
	prod, clk := &fakeProducer{}, newFakeClock()
	f := NewFeeder(FeederDeps{
		Store: store, Library: lib,
		Log: NewMemAirLog(), Listeners: NewMemListeners(time.Now),
		Fetch:   func(_ context.Context, id, _ string) (string, error) { return "/fake/" + id, nil },
		Decoder: fakeDecoder{bytesPerTrack: chunkBytes * 2},
		Encoder: enc, Producer: prod, Clock: clk, Dir: t.TempDir(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()

	// Deliberately never step the fake clock here: the whole cascade (4
	// crash attempts, each an immediate Done() on a session that's never
	// written to) and the move to 'b' happen without any pacing tick — both
	// tracks' NowPlaying publishes fire synchronously right after their
	// decoder opens, before any chunk is fed. Stepping the clock while
	// waiting would queue a pacing tick that races against the crashed
	// session's already-ready Done() in airTrack's select (fakeSession.Write
	// doesn't reject writes on a "crashed" session), nondeterministically
	// masking a crash for a beat. So this just waits on goroutine
	// scheduling.
	deadline := time.Now().Add(2 * time.Second)
	for len(prod.byTopic(TopicNowPlaying)) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for t-b now-playing frame")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	require.Equal(t, 5, enc.count()) // 1 initial + 3 capped restarts + 1 give-up restart
	// 'a' is announced once (it legitimately started airing before its
	// encoder crash-looped to exhaustion) but never again; 'b' is announced
	// once the cap gives up on 'a' and the feeder moves on. (A 3rd frame —
	// the off-air sentinel — follows once cancel() unwinds the session.)
	nps := prod.byTopic(TopicNowPlaying)
	require.Contains(t, nps[0], `"title":"t-a"`)
	require.Contains(t, nps[1], `"title":"t-b"`)
}

func TestBootResumeAirsCurrentTrackAtOffset(t *testing.T) {
	store, lib := newFixture(t, "a", "b")
	log := NewMemAirLog()
	started := time.Now().Add(-30 * time.Second).Truncate(time.Second)
	require.NoError(t, log.Append(context.Background(),
		Entry{YTID: "a", Title: "t-a", Artist: "c-a", StartedAt: started, DurationS: 60}))
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	dec := &offsetRecordingDecoder{inner: fakeDecoder{bytesPerTrack: chunkBytes * 2}}
	f := NewFeeder(FeederDeps{
		Store: store, Library: lib, Log: log, Listeners: NewMemListeners(time.Now),
		Fetch:   func(_ context.Context, id, _ string) (string, error) { return "/fake/" + id, nil },
		Decoder: dec, Encoder: enc, Producer: prod, Clock: clk, Dir: t.TempDir(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()
	for len(prod.byTopic(TopicNowPlaying)) < 1 {
		clk.step(250 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	require.InDelta(t, 30.0, dec.lastOffset(), 1.5)                        // resumed ~30s in
	require.Contains(t, prod.byTopic(TopicNowPlaying)[0], `"title":"t-a"`) // same entry
}
