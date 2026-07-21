package live

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/playlist"
)

// --- fakes ---

// fakeClock distinguishes the 250ms pacing ticker from the 25s republish
// ticker by requested duration: step() fires ONLY pacing channels, so unit
// tests never get surprise republish frames interleaved with track starts.
type fakeClock struct {
	mu   sync.Mutex
	t    time.Time
	pace []chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)}
}
func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}
func (f *fakeClock) Tick(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := make(chan time.Time, 1)
	if d < time.Second { // the 250ms pacing ticker
		f.pace = append(f.pace, c)
	} // republish (25s) channels are registered but never fired by step()
	return c
}

// step advances time and fires one pacing tick (non-blocking; a busy feeder
// that missed a tick just gets the next one — tests pump in loops).
func (f *fakeClock) step(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	now := f.t
	pace := append([]chan time.Time(nil), f.pace...)
	f.mu.Unlock()
	for _, c := range pace {
		select {
		case c <- now:
		default:
		}
	}
}

// fakeDecoder yields n bytes of PCM then EOF.
type fakeDecoder struct{ bytesPerTrack int }

func (d fakeDecoder) Open(_ context.Context, _ string, _ Loudness, _ float64) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(strings.Repeat("\x00", d.bytesPerTrack))), nil
}

type fakeSession struct {
	mu     sync.Mutex
	wrote  int
	done   chan error
	closed bool
}

func (s *fakeSession) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wrote += len(p)
	return len(p), nil
}
func (s *fakeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
	return nil
}
func (s *fakeSession) Stdin() io.WriteCloser { return s }
func (s *fakeSession) Done() <-chan error    { return s.done }
func (s *fakeSession) Stop()                 { _ = s.Close() }

type fakeEncoder struct {
	mu       sync.Mutex
	sessions []*fakeSession
}

func (e *fakeEncoder) Start(_ context.Context, _ string) (Session, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := &fakeSession{done: make(chan error, 1)}
	e.sessions = append(e.sessions, s)
	return s, nil
}

func (e *fakeEncoder) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.sessions)
}

func (s *fakeSession) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.done <- err
		close(s.done)
	}
}

// crashingEncoder auto-fails every session with index < aliveFrom right
// after Start, driving the crash-resume cap deterministically without
// having to pump the clock through each failed attempt (the crash is
// observed via the already-closed Done() channel the instant airTrack's
// select runs, before any tick fires).
type crashingEncoder struct {
	fakeEncoder
	aliveFrom int
}

func (e *crashingEncoder) Start(ctx context.Context, dir string) (Session, error) {
	sess, err := e.fakeEncoder.Start(ctx, dir)
	if err != nil {
		return sess, err
	}
	if e.count()-1 < e.aliveFrom {
		sess.(*fakeSession).fail(errors.New("simulated crash"))
	}
	return sess, nil
}

// offsetRecordingDecoder wraps fakeDecoder to record the offsetS passed to
// Open, so crash-resume tests can assert the reader was reopened at the
// aired offset.
type offsetRecordingDecoder struct {
	mu     sync.Mutex
	inner  fakeDecoder
	offset float64
}

func (d *offsetRecordingDecoder) Open(ctx context.Context, path string, l Loudness, offsetS float64) (io.ReadCloser, error) {
	d.mu.Lock()
	d.offset = offsetS
	d.mu.Unlock()
	return d.inner.Open(ctx, path, l, offsetS)
}
func (d *offsetRecordingDecoder) lastOffset() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.offset
}

type publishedFrame struct {
	topic string
	value string
}
type fakeProducer struct {
	mu     sync.Mutex
	frames []publishedFrame
}

func (p *fakeProducer) Publish(_ context.Context, topic string, value []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frames = append(p.frames, publishedFrame{topic, string(value)})
	return nil
}
func (p *fakeProducer) byTopic(topic string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, f := range p.frames {
		if f.topic == topic {
			out = append(out, f.value)
		}
	}
	return out
}

// fixture: station on-air with playlist tracks a,b (60s each at 48kHz s16le
// stereo → 11,520,000 bytes; fake tracks are far smaller for test speed —
// duration comes from the library, bytes from fakeDecoder).
func newFixture(t *testing.T, ytIDs ...string) (playlist.Store, library.Library) {
	t.Helper()
	lib := library.NewMemLibrary()
	ctx := context.Background()
	for _, id := range ytIDs {
		require.NoError(t, lib.Add(ctx, library.Track{
			YTID: id, Title: "t-" + id, Channel: "c-" + id, DurationS: 60, ArtifactID: "art-" + id,
		}))
	}
	st := playlist.NewMemStore(lib)
	p, err := st.Create(ctx, "mix")
	require.NoError(t, err)
	for _, id := range ytIDs {
		_, _, err = st.AddTrack(ctx, p.ID, id)
		require.NoError(t, err)
	}
	_, err = st.SetActive(ctx, p.ID)
	require.NoError(t, err)
	_, err = st.GoOnAir(ctx)
	require.NoError(t, err)
	return st, lib
}

func newTestFeeder(store playlist.Store, lib library.Library, enc *fakeEncoder, prod *fakeProducer, clk Clock, dir string) *Feeder {
	return NewFeeder(FeederDeps{
		Store: store, Library: lib,
		Log: NewMemAirLog(), Listeners: NewMemListeners(time.Now),
		Fetch:   func(_ context.Context, id, _ string) (string, error) { return "/fake/" + id, nil },
		Decoder: fakeDecoder{bytesPerTrack: chunkBytes * 2}, // 2 chunks per track
		Encoder: enc, Producer: prod, Clock: clk, Dir: dir,
	})
}

// drive pumps the fake clock until the session goroutine finishes or times out.
func drive(t *testing.T, clk *fakeClock, done <-chan error, maxSteps int) error {
	t.Helper()
	for i := 0; i < maxSteps; i++ {
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Millisecond):
			clk.step(250 * time.Millisecond)
		}
	}
	t.Fatal("session did not finish")
	return nil
}

func TestSessionPublishesAndLogsEachTrackThenLoops(t *testing.T) {
	store, lib := newFixture(t, "a", "b")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, enc, prod, clk, t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()
	require.Eventually(t, func() bool { return f.SessionDir() != "" }, time.Second, time.Millisecond)

	// let it air a→b→a (loop proves wrap-around), then cancel
	deadline := time.Now().Add(2 * time.Second)
	for len(prod.byTopic(TopicNowPlaying)) < 3 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for 3 now-playing frames")
		}
		select {
		case err := <-done:
			t.Fatalf("session ended early: %v", err)
		default:
			clk.step(250 * time.Millisecond)
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done

	nps := prod.byTopic(TopicNowPlaying)
	require.Contains(t, nps[0], `"title":"t-a"`)
	require.Contains(t, nps[1], `"title":"t-b"`)
	require.Contains(t, nps[2], `"title":"t-a"`) // looped
	require.Contains(t, nps[0], `"listeners":0`)
	// queue frames: bare arrays, other track first
	qs := prod.byTopic(TopicQueue)
	require.True(t, strings.HasPrefix(qs[0], "["))
	require.Contains(t, qs[0], `"title":"t-b"`)
}

func TestAutoOffAirWhenActivePlaylistEmpties(t *testing.T) {
	// Single-track playlist; after track 'a' airs once, delete the library
	// track — mem store playlists don't cascade, so instead delete the
	// playlist itself off-air? No: simulate §10 by making Get fail — the
	// real cascade path is pg-only. Simplest deterministic simulation:
	// empty the active playlist between boundaries while OFF-air is not
	// possible on-air via RemoveTrack guard… so use a store wrapper that
	// reports an empty playlist at the second boundary.
	store, lib := newFixture(t, "a")
	wrapped := &emptyAfterFirstBoundary{Store: store}
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(wrapped, lib, enc, prod, clk, t.TempDir())

	done := make(chan error, 1)
	go func() { done <- f.RunSession(context.Background()) }()
	require.Eventually(t, func() bool { return f.SessionDir() != "" }, time.Second, time.Millisecond)
	err := drive(t, clk, done, 100)
	require.NoError(t, err)

	// §10: session ended by itself, station persisted off-air, sentinel sent
	st, gerr := store.GetStation(context.Background())
	require.NoError(t, gerr)
	require.False(t, st.OnAir)
	nps := prod.byTopic(TopicNowPlaying)
	require.Contains(t, nps[len(nps)-1], `"offAir":true`)
	require.Empty(t, f.SessionDir())
}

// emptyAfterFirstBoundary lets the first boundary read through, then reports
// the active playlist as empty (simulating the DeleteTrack cascade).
type emptyAfterFirstBoundary struct {
	playlist.Store
	boundaries int
}

func (w *emptyAfterFirstBoundary) Get(ctx context.Context, id string) (playlist.Summary, []playlist.Item, error) {
	w.boundaries++
	if w.boundaries > 1 {
		s, _, err := w.Store.Get(ctx, id)
		s.TrackCount = 0
		return s, nil, err
	}
	return w.Store.Get(ctx, id)
}

func TestOperatorOffAirEndsSession(t *testing.T) {
	store, lib := newFixture(t, "a", "b")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, enc, prod, clk, t.TempDir())

	done := make(chan error, 1)
	go func() { done <- f.RunSession(context.Background()) }()
	require.Eventually(t, func() bool { return f.SessionDir() != "" }, time.Second, time.Millisecond)
	// wait for first track publish, then flip off-air
	for len(prod.byTopic(TopicNowPlaying)) < 1 {
		clk.step(250 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	_, err := store.GoOffAir(context.Background())
	require.NoError(t, err)
	require.NoError(t, drive(t, clk, done, 100))

	require.Contains(t, prod.byTopic(TopicNowPlaying), string(OffAirPayload()))
	require.Empty(t, f.SessionDir())
	require.True(t, enc.sessions[0].closed) // encoder stdin closed
}

func TestStartedAtFollowsSampleClock(t *testing.T) {
	store, lib := newFixture(t, "a", "b")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, enc, prod, clk, t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()
	require.Eventually(t, func() bool { return f.SessionDir() != "" }, time.Second, time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for len(prod.byTopic(TopicNowPlaying)) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for 2 now-playing frames")
		}
		clk.step(250 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	// track a = 2 chunks = 96,000 bytes = 0.5s of audio → track b's
	// startedAt must be exactly a's startedAt + 500ms (sample math, not
	// wall-clock guesses).
	nps := prod.byTopic(TopicNowPlaying)
	require.Contains(t, nps[0], `"startedAt":"2026-07-21T12:00:00Z"`)
	require.Contains(t, nps[1], `"startedAt":"2026-07-21T12:00:00.5`)
}

// TestFetchFailureSkipsTrackWithoutPublishOrLog covers the CRITICAL fix: a
// track that fails to fetch must never be announced (NowPlaying/Queue) or
// air-logged — it never actually aired. The feeder should silently skip past
// it to the next track in rotation.
func TestFetchFailureSkipsTrackWithoutPublishOrLog(t *testing.T) {
	store, lib := newFixture(t, "a", "b")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	log := NewMemAirLog()
	f := NewFeeder(FeederDeps{
		Store: store, Library: lib,
		Log: log, Listeners: NewMemListeners(time.Now),
		Fetch: func(_ context.Context, id, _ string) (string, error) {
			if id == "art-a" {
				return "", errors.New("fetch failed for a")
			}
			return "/fake/" + id, nil
		},
		Decoder: fakeDecoder{bytesPerTrack: chunkBytes * 2},
		Encoder: enc, Producer: prod, Clock: clk, Dir: t.TempDir(),
	})

	done := make(chan error, 1)
	go func() { done <- f.RunSession(context.Background()) }()
	require.Eventually(t, func() bool { return f.SessionDir() != "" }, time.Second, time.Millisecond)

	// 'a' fails to fetch every time it comes up; 'b' airs fine — wait for
	// b's now-playing frame, then flip off-air to end the session cleanly.
	deadline := time.Now().Add(2 * time.Second)
	for len(prod.byTopic(TopicNowPlaying)) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for t-b now-playing frame")
		}
		clk.step(250 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	_, err := store.GoOffAir(context.Background())
	require.NoError(t, err)
	require.NoError(t, drive(t, clk, done, 100))

	nps := prod.byTopic(TopicNowPlaying)
	require.Contains(t, nps[0], `"title":"t-b"`)
	for _, frame := range nps {
		require.NotContains(t, frame, `"title":"t-a"`) // 'a' never aired
	}

	latest, ok, lerr := log.Latest(context.Background())
	require.NoError(t, lerr)
	require.True(t, ok)
	require.Equal(t, "b", latest.YTID) // 'a' never air-logged either
}

// TestOperatorOffAirStopsMidTrackWithinAChunk covers the IMPORTANT off-air
// latency fix: flipping off-air must stop feeding within about one chunk,
// not wait for the whole track to finish.
func TestOperatorOffAirStopsMidTrackWithinAChunk(t *testing.T) {
	store, lib := newFixture(t, "a")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := NewFeeder(FeederDeps{
		Store: store, Library: lib,
		Log: NewMemAirLog(), Listeners: NewMemListeners(time.Now),
		Fetch:   func(_ context.Context, id, _ string) (string, error) { return "/fake/" + id, nil },
		Decoder: fakeDecoder{bytesPerTrack: chunkBytes * 8}, // 8 chunks
		Encoder: enc, Producer: prod, Clock: clk, Dir: t.TempDir(),
	})

	done := make(chan error, 1)
	go func() { done <- f.RunSession(context.Background()) }()
	require.Eventually(t, func() bool { return f.SessionDir() != "" }, time.Second, time.Millisecond)

	// feed ~2 of the 8 chunks, then flip off-air mid-track
	for i := 0; i < 2; i++ {
		clk.step(250 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	_, err := store.GoOffAir(context.Background())
	require.NoError(t, err)
	require.NoError(t, drive(t, clk, done, 100))

	require.Empty(t, f.SessionDir())
	require.Contains(t, prod.byTopic(TopicNowPlaying), string(OffAirPayload()))
	require.Less(t, enc.sessions[0].wrote, 8*chunkBytes) // stopped mid-track
}
