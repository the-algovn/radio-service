package live

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/playlist"
	"github.com/the-algovn/radio-service/internal/request"
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

// fixture: station on-air; tracks in the library only (playlists are
// curation tools now — the engine never reads them). Requests seed per-test.
func newFixture(t *testing.T, ytIDs ...string) (playlist.Store, library.Library, *request.MemStore) {
	t.Helper()
	lib := library.NewMemLibrary()
	ctx := context.Background()
	for _, id := range ytIDs {
		require.NoError(t, lib.Add(ctx, library.Track{
			YTID: id, Title: "t-" + id, Channel: "c-" + id, DurationS: 60, ArtifactID: "art-" + id,
		}))
	}
	st := playlist.NewMemStore(lib)
	_, err := st.GoOnAir(ctx)
	require.NoError(t, err)
	return st, lib, request.NewMemStore()
}

func newTestFeeder(store playlist.Store, lib library.Library, reqs request.Store, enc *fakeEncoder, prod *fakeProducer, clk Clock, dir string) *Feeder {
	return NewFeeder(FeederDeps{
		Store: store, Requests: reqs, Library: lib,
		Log: NewMemAirLog(), Listeners: NewMemListeners(time.Now),
		Fetch:   func(_ context.Context, id, _ string) (string, error) { return "/fake/" + id, nil },
		Decoder: fakeDecoder{bytesPerTrack: chunkBytes * 2}, // 2 chunks per track
		Encoder: enc, Producer: prod, Clock: clk, Dir: dir,
		Rand: func(int) int { return 0 }, // deterministic shuffle for tests
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

// Shuffle-only: with a 2-track library the no-repeat window is 1, so the
// bed alternates a,b,a deterministically regardless of Rand.
func TestShuffleBedAlternatesWithNoRepeatWindow(t *testing.T) {
	store, lib, reqs := newFixture(t, "a", "b")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()
	require.Eventually(t, func() bool { return f.SessionDir() != "" }, time.Second, time.Millisecond)

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
	first := nps[0]
	second := nps[1]
	require.NotEqual(t, first, second) // window=1 forbids an immediate repeat
	require.Contains(t, nps[2], titleOf(first))
}

// titleOf extracts the "title" value from a now-playing frame for
// alternation assertions without caring which track aired first.
func titleOf(frame string) string {
	var v struct {
		Title string `json:"title"`
	}
	_ = json.Unmarshal([]byte(frame), &v)
	return v.Title
}

// Provenance flows: a listener request airs with name+source, an AI pick
// with reason, and the shuffle bed with none of the three.
func TestNowPlayingCarriesProvenance(t *testing.T) {
	store, lib, reqs := newFixture(t, "a", "b", "req", "pick")
	ctx0 := context.Background()
	_, err := reqs.Create(ctx0, request.Item{Source: request.SourceAI,
		YTID: "pick", Title: "t-pick", Channel: "c-pick", DurationS: 60,
		Status: request.StatusReady, Reason: "hợp đêm mưa"})
	require.NoError(t, err)
	_, err = reqs.Create(ctx0, request.Item{Source: request.SourceListener,
		RequestedBy: "u1", DisplayName: "Ngọc", YTID: "req", Title: "t-req",
		Channel: "c-req", DurationS: 60, Status: request.StatusReady})
	require.NoError(t, err)

	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for len(prod.byTopic(TopicNowPlaying)) < 3 {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		clk.step(250 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	nps := prod.byTopic(TopicNowPlaying)
	// frame 0: the listener request
	require.Contains(t, nps[0], `"source":"listener"`)
	require.Contains(t, nps[0], `"requestedByName":"Ngọc"`)
	require.NotContains(t, nps[0], `"reason"`)
	// frame 1: the AI pick
	require.Contains(t, nps[1], `"source":"ai"`)
	require.Contains(t, nps[1], `"reason":"hợp đêm mưa"`)
	require.NotContains(t, nps[1], "requestedByName")
	// frame 2: shuffle — unattributed
	require.NotContains(t, nps[2], `"source"`)
	require.NotContains(t, nps[2], "requestedByName")
	require.NotContains(t, nps[2], `"reason"`)
}

// A ready listener request airs before a ready AI pick, which airs before
// shuffle; aired requests are marked and leave the queue payload.
func TestBoundaryPriorityRequestThenAIThenShuffle(t *testing.T) {
	store, lib, reqs := newFixture(t, "a", "b", "req", "pick")
	ctx0 := context.Background()
	aiIt, err := reqs.Create(ctx0, request.Item{Source: request.SourceAI,
		YTID: "pick", Title: "t-pick", Channel: "c-pick", DurationS: 60, Status: request.StatusReady})
	require.NoError(t, err)
	lIt, err := reqs.Create(ctx0, request.Item{Source: request.SourceListener,
		RequestedBy: "u1", DisplayName: "Ngọc", YTID: "req", Title: "t-req", Channel: "c-req",
		DurationS: 60, Status: request.StatusReady})
	require.NoError(t, err)

	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for len(prod.byTopic(TopicNowPlaying)) < 3 {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		clk.step(250 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	nps := prod.byTopic(TopicNowPlaying)
	require.Contains(t, nps[0], `"title":"t-req"`)  // listener first
	require.Contains(t, nps[1], `"title":"t-pick"`) // then AI
	// third frame is shuffle — either library track, never the aired requests
	require.NotContains(t, nps[2], "t-req")
	require.NotContains(t, nps[2], "t-pick")

	mine, err := reqs.ByUser(ctx0, "u1", 10)
	require.NoError(t, err)
	require.Equal(t, request.StatusAired, mine[0].Status)
	require.NotNil(t, mine[0].AiredAt)
	_ = aiIt
	_ = lIt

	// queue frames: first frame (published at t-req's start) still holds the
	// AI pick with its source badge; later frames drain to [].
	qs := prod.byTopic(TopicQueue)
	require.NotEmpty(t, qs)
	require.Contains(t, qs[0], `"source":"ai"`)
	require.Contains(t, qs[0], `"title":"t-pick"`)
	require.Equal(t, "[]", qs[len(qs)-1])
}

// A ready request whose track vanished from the library fails and is
// skipped without airing or being announced.
func TestVanishedRequestTrackMarksFailed(t *testing.T) {
	store, lib, reqs := newFixture(t, "a", "b")
	ctx0 := context.Background()
	it, err := reqs.Create(ctx0, request.Item{Source: request.SourceListener,
		RequestedBy: "u1", YTID: "ghost", Title: "t-ghost", Channel: "c", DurationS: 60,
		Status: request.StatusReady})
	require.NoError(t, err)

	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for len(prod.byTopic(TopicNowPlaying)) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		clk.step(250 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	require.NotContains(t, prod.byTopic(TopicNowPlaying)[0], "t-ghost")
	mine, err := reqs.ByUser(ctx0, "u1", 10)
	require.NoError(t, err)
	require.Equal(t, request.StatusFailed, mine[0].Status)
	require.Equal(t, "track vanished from library", mine[0].FailReason)
	_ = it
}

var errMarkFailedBroken = errors.New("store: write failed")

// failingMarkFailedStore wraps request.MemStore so MarkFailed always errors,
// simulating a persistently-failing store (reads OK, writes failing). Used
// to prove boundary()'s vanished-request branch surfaces a MarkFailed
// failure as fatal instead of silently looping (skip=true would let
// RunSession re-pick the same ready, still-unmarked request on every
// iteration with no pacing — a hot spin inside the audio goroutine).
type failingMarkFailedStore struct {
	*request.MemStore
}

func (failingMarkFailedStore) MarkFailed(context.Context, string, string) error {
	return errMarkFailedBroken
}

// A MarkFailed error on the vanished-track branch must end the session with
// that error (not skip=true) so Engine.Run's 5s poll paces the retry,
// instead of RunSession re-picking the same ready request immediately.
func TestBoundaryMarkFailedErrorIsFatal(t *testing.T) {
	store, lib, _ := newFixture(t, "a", "b")
	mem := request.NewMemStore()
	reqs := failingMarkFailedStore{mem}
	ctx0 := context.Background()
	_, err := mem.Create(ctx0, request.Item{Source: request.SourceListener,
		RequestedBy: "u1", YTID: "ghost", Title: "t-ghost", Channel: "c", DurationS: 60,
		Status: request.StatusReady})
	require.NoError(t, err)

	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())
	done := make(chan error, 1)
	go func() { done <- f.RunSession(context.Background()) }()

	runErr := drive(t, clk, done, 200)
	require.ErrorIs(t, runErr, errMarkFailedBroken)
	// No track was ever announced — the only now-playing-topic frame
	// allowed is RunSession's own end-of-session off-air marker (published
	// unconditionally by its teardown defer on any exit), never a
	// NowPlayingPayload for the vanished track.
	for _, frame := range prod.byTopic(TopicNowPlaying) {
		require.NotContains(t, frame, `"kind":"track"`)
	}
}

// Empty library (the only remaining engine-side closure): auto off-air.
func TestEmptyLibraryAutoOffAir(t *testing.T) {
	store, lib, reqs := newFixture(t) // no tracks at all
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())
	done := make(chan error, 1)
	go func() { done <- f.RunSession(context.Background()) }()
	require.NoError(t, drive(t, clk, done, 100))
	st, err := store.GetStation(context.Background())
	require.NoError(t, err)
	require.False(t, st.OnAir)
	// nothing ever aired: the only now-playing frame is the teardown sentinel
	frames := prod.byTopic(TopicNowPlaying)
	require.Len(t, frames, 1)
	require.JSONEq(t, `{"offAir":true}`, frames[0])
}

func TestOperatorOffAirEndsSession(t *testing.T) {
	store, lib, reqs := newFixture(t, "a", "b")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())

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
	store, lib, reqs := newFixture(t, "a", "b")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())

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
	store, lib, reqs := newFixture(t, "a", "b")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	log := NewMemAirLog()
	f := NewFeeder(FeederDeps{
		Store: store, Requests: reqs, Library: lib,
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
	store, lib, reqs := newFixture(t, "a")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := NewFeeder(FeederDeps{
		Store: store, Requests: reqs, Library: lib,
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
