package live

import (
	"context"
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

	// let it air a→b→a (loop proves wrap-around), then cancel
	for len(prod.byTopic(TopicNowPlaying)) < 3 {
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
	for len(prod.byTopic(TopicNowPlaying)) < 2 {
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
