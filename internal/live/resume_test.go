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
	store, lib, reqs := newFixture(t, "a", "b")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	dec := &offsetRecordingDecoder{inner: fakeDecoder{bytesPerTrack: chunkBytes * 4}}
	f := NewFeeder(FeederDeps{
		Store: store, Requests: reqs, Library: lib,
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
	store, lib, reqs := newFixture(t, "a", "b")
	// sessions 0..3 (the initial attempt + 3 restarts) auto-crash; session 4
	// (the 5th, started after the cap is exhausted) survives so track 'b'
	// can actually air on it.
	enc := &crashingEncoder{aliveFrom: 4}
	prod, clk := &fakeProducer{}, newFakeClock()
	f := NewFeeder(FeederDeps{
		Store: store, Requests: reqs, Library: lib,
		Log: NewMemAirLog(), Listeners: NewMemListeners(time.Now),
		Fetch:   func(_ context.Context, id, _ string) (string, error) { return "/fake/" + id, nil },
		Decoder: fakeDecoder{bytesPerTrack: chunkBytes * 2},
		Encoder: enc, Producer: prod, Clock: clk, Dir: t.TempDir(),
		// Deterministic shuffle so the bed is a then b: the cap-exhaustion
		// path must give up on the first-aired track and move to the next.
		Rand: func(int) int { return 0 },
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
	store, lib, reqs := newFixture(t, "a", "b")
	log := NewMemAirLog()
	started := time.Now().Add(-30 * time.Second).Truncate(time.Second)
	require.NoError(t, log.Append(context.Background(),
		Entry{YTID: "a", Title: "t-a", Artist: "c-a", StartedAt: started, DurationS: 60}))
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	dec := &offsetRecordingDecoder{inner: fakeDecoder{bytesPerTrack: chunkBytes * 2}}
	f := NewFeeder(FeederDeps{
		Store: store, Requests: reqs, Library: lib, Log: log, Listeners: NewMemListeners(time.Now),
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

// TestBootResumeThenCrashKeepsOriginalOffset pins the subtlest line in the
// boot-resume implementation: trackStartSamples is special-cased to 0 for a
// resumed track (not `= samplesFed`, the pattern every OTHER track uses),
// because samplesFed is pre-loaded with the resume offset while the track's
// true start is 0 frames from the (now entry.StartedAt) anchor. A refactor
// that "simplified" this back to the general `trackStartSamples = samplesFed`
// pattern would silently double-count the resume offset on any mid-track
// crash. This test forces exactly that path: resume track 'a' ~30s in, feed
// two more chunks (0.5s), then crash the encoder — the reopened decoder must
// see offset ≈ original 30s + 0.5s newly fed, not just the 0.5s.
func TestBootResumeThenCrashKeepsOriginalOffset(t *testing.T) {
	store, lib, reqs := newFixture(t, "a", "b")
	log := NewMemAirLog()
	started := time.Now().Add(-30 * time.Second).Truncate(time.Second)
	require.NoError(t, log.Append(context.Background(),
		Entry{YTID: "a", Title: "t-a", Artist: "c-a", StartedAt: started, DurationS: 60}))
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	dec := &offsetRecordingDecoder{inner: fakeDecoder{bytesPerTrack: chunkBytes * 4}}
	f := NewFeeder(FeederDeps{
		Store: store, Requests: reqs, Library: lib, Log: log, Listeners: NewMemListeners(time.Now),
		Fetch:   func(_ context.Context, id, _ string) (string, error) { return "/fake/" + id, nil },
		Decoder: dec, Encoder: enc, Producer: prod, Clock: clk, Dir: t.TempDir(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()

	// wait for the resumed session + its first now-playing frame
	require.Eventually(t, func() bool { return f.SessionDir() != "" }, time.Second, time.Millisecond)
	for len(prod.byTopic(TopicNowPlaying)) < 1 {
		clk.step(250 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}

	// feed ~2 chunks (0.5s) of the resumed track, then kill the encoder mid-track
	for i := 0; i < 2; i++ {
		clk.step(250 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	time.Sleep(5 * time.Millisecond)
	enc.sessions[0].fail(context.DeadlineExceeded) // simulated crash

	// a second encoder session must start and the decoder re-open with
	// offset ≈ original resume offset (30s) + newly fed audio (0.5s)
	for enc.count() < 2 {
		clk.step(250 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	require.InDelta(t, 30.5, dec.lastOffset(), 1.5)
	cancel()
	<-done
}
