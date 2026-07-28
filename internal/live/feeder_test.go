package live

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/schedule"
	"github.com/the-algovn/radio-service/internal/station"
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

// pumpChunk fires one pacing tick and waits for the feeder to write the
// resulting chunk, so a test's step count equals the chunks actually fed.
// Returns false when no write arrives before the deadline, which is how a
// test observes "the item ended on this tick" rather than hanging.
func pumpChunk(t *testing.T, clk *fakeClock, s *fakeSession) bool {
	t.Helper()
	clk.step(250 * time.Millisecond)
	select {
	case <-s.writes:
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}

// fakeDecoder yields bytesPerTrack of PCM then EOF. sample, when non-nil, is
// consulted per artifact path for the constant s16le value that artifact
// decodes to — which is what makes a mix of two tracks verifiable by
// arithmetic. nil keeps the historical all-zero fill, so tests that predate
// transitions are unaffected.
type fakeDecoder struct {
	bytesPerTrack int
	sample        func(path string) int16
}

func (d fakeDecoder) Open(_ context.Context, path string, _ Loudness, _ float64) (io.ReadCloser, error) {
	var v int16
	if d.sample != nil {
		v = d.sample(path)
	}
	buf := make([]byte, d.bytesPerTrack)
	for i := 0; i+1 < len(buf); i += 2 {
		binary.LittleEndian.PutUint16(buf[i:], uint16(v))
	}
	return io.NopCloser(bytes.NewReader(buf)), nil
}

type fakeSession struct {
	mu     sync.Mutex
	wrote  int
	buf    bytes.Buffer  // the mixer's only witness — see pumpChunk
	writes chan struct{} // one token per Write, for the synchronous pump
	done   chan error
	closed bool
}

func (s *fakeSession) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.wrote += len(p)
	s.buf.Write(p)
	s.mu.Unlock()
	// Non-blocking: a test that is not pumping must never deadlock the feeder.
	select {
	case s.writes <- struct{}{}:
	default:
	}
	return len(p), nil
}

// bytes returns a copy of everything fed to this session so far.
func (s *fakeSession) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf.Bytes()...)
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
	s := &fakeSession{done: make(chan error, 1), writes: make(chan struct{}, 1024)}
	e.sessions = append(e.sessions, s)
	return s, nil
}

func (e *fakeEncoder) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.sessions)
}

// firstSessionBytes reports how many bytes the first encoder session has been
// fed so far — a test's stand-in for "how far into the stream are we". It is a
// method because the two pieces live under different locks: the session slice
// under the encoder's mutex, the byte count under the session's own.
func (e *fakeEncoder) firstSessionBytes() int {
	e.mu.Lock()
	if len(e.sessions) == 0 {
		e.mu.Unlock()
		return 0
	}
	s := e.sessions[0]
	e.mu.Unlock()
	return len(s.bytes())
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
//
// Offsets are keyed by artifact path rather than kept as "the last Open": the
// lookahead opens the NEXT item — always at offset 0 — while the current one
// is still airing, so the most recent Open is routinely not the one a resume
// test is asking about.
type offsetRecordingDecoder struct {
	mu      sync.Mutex
	inner   fakeDecoder
	offsets map[string]float64
}

func (d *offsetRecordingDecoder) Open(ctx context.Context, path string, l Loudness, offsetS float64) (io.ReadCloser, error) {
	d.mu.Lock()
	if d.offsets == nil {
		d.offsets = map[string]float64{}
	}
	d.offsets[path] = offsetS
	d.mu.Unlock()
	return d.inner.Open(ctx, path, l, offsetS)
}

// offsetFor reports the offset this artifact was most recently opened at.
func (d *offsetRecordingDecoder) offsetFor(path string) float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.offsets[path]
}

// countingReadCloser records whether Close was called.
type countingReadCloser struct {
	io.Reader
	closed *int32
}

func (c countingReadCloser) Close() error {
	atomic.AddInt32(c.closed, 1)
	return nil
}

// closeRecordingDecoder hands out readers and counts opens and closes, so a
// test can assert that a session leaks no decoder. A leak here is permanent
// in production: FFDecoder binds the ffmpeg process to RunSession's ctx,
// which is Engine.Run's ctx, which is process lifetime.
type closeRecordingDecoder struct {
	bytesPerTrack int
	opened        int32
	closed        int32
}

func (d *closeRecordingDecoder) Open(_ context.Context, _ string, _ Loudness, _ float64) (io.ReadCloser, error) {
	atomic.AddInt32(&d.opened, 1)
	return countingReadCloser{
		Reader: bytes.NewReader(make([]byte, d.bytesPerTrack)),
		closed: &d.closed,
	}, nil
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
func newFixture(t *testing.T, ytIDs ...string) (station.Store, library.Library, *request.MemStore) {
	t.Helper()
	lib := library.NewMemLibrary()
	ctx := context.Background()
	for _, id := range ytIDs {
		require.NoError(t, lib.Add(ctx, library.Track{
			YTID: id, Title: "t-" + id, Channel: "c-" + id, DurationS: 60, ArtifactID: "art-" + id,
		}))
	}
	st := station.NewMemStore()
	_, err := st.GoOnAir(ctx)
	require.NoError(t, err)
	return st, lib, request.NewMemStore()
}

func newTestFeeder(store station.Store, lib library.Library, reqs request.Store, enc *fakeEncoder, prod *fakeProducer, clk Clock, dir string) *Feeder {
	return newTestFeederWith(store, lib, reqs, enc, prod, clk, dir)
}

// newTestFeederWith is newTestFeeder with per-test overrides applied to the
// deps before construction — used by tests that need a sighted decoder or a
// fetch that fails on demand.
func newTestFeederWith(store station.Store, lib library.Library, reqs request.Store, enc *fakeEncoder, prod *fakeProducer, clk Clock, dir string, opts ...func(*FeederDeps)) *Feeder {
	d := FeederDeps{
		Store: store, Requests: reqs, Library: lib, Sched: schedule.NewMemStore(),
		Log: NewMemAirLog(), Listeners: NewMemListeners(time.Now),
		Fetch:   func(_ context.Context, id, _ string) (string, error) { return "/fake/" + id, nil },
		Decoder: fakeDecoder{bytesPerTrack: chunkBytes * 2}, // 2 chunks per track
		Encoder: enc, Producer: prod, Clock: clk, Dir: dir,
		Rand: func(int) int { return 0 }, // deterministic shuffle for tests
	}
	for _, o := range opts {
		o(&d)
	}
	return NewFeeder(d)
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

func TestFakeDecoderEmitsPerArtifactSamples(t *testing.T) {
	d := fakeDecoder{bytesPerTrack: 8, sample: func(path string) int16 {
		if strings.HasSuffix(path, "a") {
			return 1000
		}
		return -1000
	}}

	ra, err := d.Open(context.Background(), "/fake/a", Loudness{}, 0)
	require.NoError(t, err)
	got, err := io.ReadAll(ra)
	require.NoError(t, err)
	require.Len(t, got, 8)
	require.Equal(t, int16(1000), int16(binary.LittleEndian.Uint16(got[0:])))

	rb, err := d.Open(context.Background(), "/fake/b", Loudness{}, 0)
	require.NoError(t, err)
	got, err = io.ReadAll(rb)
	require.NoError(t, err)
	require.Equal(t, int16(-1000), int16(binary.LittleEndian.Uint16(got[0:])))
}

func TestFakeSessionCapturesWrittenBytes(t *testing.T) {
	s := &fakeSession{done: make(chan error, 1), writes: make(chan struct{}, 4)}
	_, err := s.Write([]byte{0x01, 0x02})
	require.NoError(t, err)
	_, err = s.Write([]byte{0x03})
	require.NoError(t, err)

	require.Equal(t, []byte{0x01, 0x02, 0x03}, s.bytes())
	require.Equal(t, 3, s.wrote)
}

func TestPumpChunkCountsExactChunks(t *testing.T) {
	s := &fakeSession{done: make(chan error, 1), writes: make(chan struct{}, 4)}
	clk := newFakeClock()

	go func() {
		_, _ = s.Write(make([]byte, chunkBytes))
	}()
	require.True(t, pumpChunk(t, clk, s), "a write should have been observed")
	require.Equal(t, chunkBytes, s.wrote)

	require.False(t, pumpChunk(t, clk, s), "no further write should arrive")
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
	// AI pick with its source badge; once pending drains, the committed
	// shuffle next-up covers the slot instead of an empty array (design
	// 2026-07-23: queue never empty).
	qs := prod.byTopic(TopicQueue)
	require.NotEmpty(t, qs)
	require.Contains(t, qs[0], `"source":"ai"`)
	require.Contains(t, qs[0], `"title":"t-pick"`)
	last := qs[len(qs)-1]
	require.NotEqual(t, "[]", last)
	require.Contains(t, last, `"source":""`)
}

// A committed next-up (a shuffle pick pinned earlier) airs before a pending
// listener request that arrived afterward — the locked contract.
func TestCommittedNextUpAirsBeforePendingRequest(t *testing.T) {
	store, lib, reqs := newFixture(t, "committed", "req", "bed")
	ctx0 := context.Background()
	_, err := reqs.Create(ctx0, request.Item{Source: request.SourceListener,
		RequestedBy: "u1", DisplayName: "Ngọc", YTID: "req", Title: "t-req", Channel: "c-req",
		DurationS: 60, Status: request.StatusReady})
	require.NoError(t, err)

	sched := schedule.NewMemStore()
	require.NoError(t, sched.SetNextUp(ctx0, schedule.NextUp{YTID: "committed", Title: "t-committed", Channel: "c"}))

	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := NewFeeder(FeederDeps{
		Store: store, Requests: reqs, Library: lib, Sched: sched,
		Log: NewMemAirLog(), Listeners: NewMemListeners(time.Now),
		Fetch:   func(_ context.Context, id, _ string) (string, error) { return "/fake/" + id, nil },
		Decoder: fakeDecoder{bytesPerTrack: chunkBytes * 2},
		Encoder: enc, Producer: prod, Clock: clk, Dir: t.TempDir(),
		Rand: func(int) int { return 0 },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for len(prod.byTopic(TopicNowPlaying)) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		clk.step(250 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	nps := prod.byTopic(TopicNowPlaying)
	require.Contains(t, nps[0], `"title":"t-committed"`) // committed next-up first
	require.Contains(t, nps[1], `"title":"t-req"`)       // request waits behind it

	// The committed next-up is consumed once — but by the time track 2
	// (the request) starts, the queue is empty again, so commitNextUp
	// (Task 6) pins a fresh shuffle pick rather than leaving it cleared.
	nu, ok, err := sched.GetNextUp(ctx0)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotEqual(t, "committed", nu.YTID)
}

// At track start with an empty request queue, a shuffle next-up is committed;
// with a pending request, any stale commitment is cleared.
func TestCommitNextUpTracksPending(t *testing.T) {
	store, lib, reqs := newFixture(t, "a", "b")
	sched := schedule.NewMemStore()
	f := NewFeeder(FeederDeps{
		Store: store, Requests: reqs, Library: lib, Sched: sched,
		Log: NewMemAirLog(), Listeners: NewMemListeners(time.Now),
		Rand: func(int) int { return 0 },
	})
	ctx := context.Background()

	// Empty queue → commit a shuffle pick.
	f.commitNextUp(ctx)
	_, ok, err := sched.GetNextUp(ctx)
	require.NoError(t, err)
	require.True(t, ok)

	// Pending request present → clear the stale commitment.
	_, err = reqs.Create(ctx, request.Item{Source: request.SourceListener,
		YTID: "a", Title: "t-a", Channel: "c", DurationS: 60, Status: request.StatusReady})
	require.NoError(t, err)
	f.commitNextUp(ctx)
	_, ok, err = sched.GetNextUp(ctx)
	require.NoError(t, err)
	require.False(t, ok)
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

func TestPlanNextDoesNotConsumeTheCommittedNextUp(t *testing.T) {
	store, lib, reqs := newFixture(t, "t1")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())
	ctx := context.Background()
	require.NoError(t, f.d.Sched.SetNextUp(ctx, schedule.NextUp{YTID: "t1", Title: "T1"}))

	p, err := f.planNext(ctx)
	require.NoError(t, err)
	require.Equal(t, "t1", p.item.track.YTID)
	require.True(t, p.consumedNextUp)

	got, ok, err := f.d.Sched.GetNextUp(ctx)
	require.NoError(t, err)
	require.True(t, ok, "planNext must not clear the next-up — only commitNext may")
	require.Equal(t, "t1", got.YTID)
}

func TestPlanNextDoesNotMarkAVanishedRequestFailed(t *testing.T) {
	store, lib, reqs := newFixture(t, "t1")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())
	ctx := context.Background()

	created, err := reqs.Create(ctx, request.Item{
		Source: request.SourceListener, RequestedBy: "sub-1", DisplayName: "Ngoc",
		YTID: "gone", Title: "Gone", Status: request.StatusReady,
	})
	require.NoError(t, err)

	p, err := f.planNext(ctx)
	require.NoError(t, err)
	require.True(t, p.skip, "a request whose track is absent from the library is a skip")
	require.Equal(t, created.ID, p.failRequestID)

	items, err := reqs.Pending(ctx)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, request.StatusReady, items[0].Status,
		"planNext must not mark the request failed — only commitNext may")
}

func TestBoundaryStillConsumesTheNextUp(t *testing.T) {
	store, lib, reqs := newFixture(t, "t1")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())
	ctx := context.Background()
	require.NoError(t, f.d.Sched.SetNextUp(ctx, schedule.NextUp{YTID: "t1", Title: "T1"}))

	item, skip, stop, err := f.boundary(ctx)
	require.NoError(t, err)
	require.False(t, skip)
	require.False(t, stop)
	require.Equal(t, "t1", item.track.YTID)

	_, ok, err := f.d.Sched.GetNextUp(ctx)
	require.NoError(t, err)
	require.False(t, ok, "boundary must still consume the next-up exactly as before")
}

var errLibraryGetBroken = errors.New("library: get failed")

// failingGetLibrary wraps a library.Library so Get always errors (e.g. a
// row-specific scan/malformed-row failure), reads otherwise unaffected. Used
// to prove boundary() still clears a committed next-up before surfacing this
// error — exactly as the pre-split code did by clearing unconditionally
// before this Get ever ran — so the station self-heals on the next restart
// instead of wedging on the same broken commitment forever.
type failingGetLibrary struct {
	library.Library
}

func (failingGetLibrary) Get(context.Context, string) (library.Track, bool, error) {
	return library.Track{}, false, errLibraryGetBroken
}

// A Library.Get error on the committed next-up's own track must still clear
// that commitment before the error propagates, matching the pre-split
// behaviour: ClearNextUp ran unconditionally before this Get, so a
// persistent Get failure never left the next-up stuck — the next restart's
// GetNextUp found nothing and fell through to requests/shuffle.
func TestBoundaryConsumesNextUpEvenWhenLibraryGetErrors(t *testing.T) {
	store, lib, reqs := newFixture(t, "t1")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, failingGetLibrary{lib}, reqs, enc, prod, clk, t.TempDir())
	ctx := context.Background()
	require.NoError(t, f.d.Sched.SetNextUp(ctx, schedule.NextUp{YTID: "t1", Title: "T1"}))

	_, _, _, err := f.boundary(ctx)
	require.ErrorIs(t, err, errLibraryGetBroken)

	_, ok, gerr := f.d.Sched.GetNextUp(ctx)
	require.NoError(t, gerr)
	require.False(t, ok, "boundary must still clear a consumed next-up even when the Get reading it errors")
}

// planNext must not put the station off-air itself on an empty library —
// only commitNext (via autoOffAir) may. Acting on it at plan time would be
// the most visibly wrong of the three deferred writes: it would end the
// broadcast while the current item is still airing, from a prefetch
// goroutine that was only ever supposed to look ahead.
func TestPlanNextDoesNotAutoOffAirOnEmptyLibrary(t *testing.T) {
	store, lib, reqs := newFixture(t) // no tracks at all
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())
	ctx := context.Background()

	p, err := f.planNext(ctx)
	require.NoError(t, err)
	require.True(t, p.stop)
	require.True(t, p.emptyLib)

	st, err := store.GetStation(ctx)
	require.NoError(t, err)
	require.True(t, st.OnAir, "planNext must not put the station off-air — only commitNext may")
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
		Store: store, Requests: reqs, Library: lib, Sched: schedule.NewMemStore(),
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

// The queue snapshot published at every track start never shows an empty
// array: with no pending requests, commitNextUp always pins a shuffle track
// first, so the SSE frame carries it (design 2026-07-23 — queue never empty).
func TestEmptyQueueCommitsShuffleNextUp(t *testing.T) {
	store, lib, reqs := newFixture(t, "a", "b", "c")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())

	done := make(chan error, 1)
	go func() { done <- f.RunSession(context.Background()) }()
	require.Eventually(t, func() bool { return f.SessionDir() != "" }, time.Second, time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for len(prod.byTopic(TopicNowPlaying)) < 3 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for 3 now-playing frames")
		}
		clk.step(250 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	_, err := store.GoOffAir(context.Background())
	require.NoError(t, err)
	require.NoError(t, drive(t, clk, done, 100))

	qs := prod.byTopic(TopicQueue)
	require.NotEmpty(t, qs)
	for _, frame := range qs {
		require.NotEqual(t, "[]", frame)
	}
}

// TestOperatorOffAirStopsMidTrackWithinAChunk covers the IMPORTANT off-air
// latency fix: flipping off-air must stop feeding within about one chunk,
// not wait for the whole track to finish.
func TestOperatorOffAirStopsMidTrackWithinAChunk(t *testing.T) {
	store, lib, reqs := newFixture(t, "a")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := NewFeeder(FeederDeps{
		Store: store, Requests: reqs, Library: lib, Sched: schedule.NewMemStore(),
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

// RequestSkip ends the current track at the next tick; the next boundary
// picks the following item.
func TestRequestSkipAdvancesAtNextTick(t *testing.T) {
	store, lib, reqs := newFixture(t, "a", "b")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()

	// wait for the first track to announce
	deadline := time.Now().Add(2 * time.Second)
	for len(prod.byTopic(TopicNowPlaying)) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for first track")
		}
		clk.step(250 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	first := prod.byTopic(TopicNowPlaying)[0]

	f.RequestSkip()
	// one tick consumes the skip; the next boundary announces track 2 well
	// before the fake track's 2-chunk length would have elapsed naturally
	for len(prod.byTopic(TopicNowPlaying)) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the skip to advance")
		}
		clk.step(250 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	second := prod.byTopic(TopicNowPlaying)[1]
	require.NotEqual(t, titleOf(first), titleOf(second))
}

// A skip requested while no session runs must NOT bleed into the next
// session's first track.
func TestStaleSkipClearedAtSessionStart(t *testing.T) {
	store, lib, reqs := newFixture(t, "a", "b")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())

	f.RequestSkip() // stale: nothing is airing

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()

	// the first track must play a full fake-track length (2 chunks) without
	// being cut by the stale flag: pump exactly one chunk and assert we are
	// still on frame 1, then let it finish naturally.
	deadline := time.Now().Add(2 * time.Second)
	for len(prod.byTopic(TopicNowPlaying)) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		clk.step(250 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	clk.step(250 * time.Millisecond) // one chunk fed — a live stale flag would end the track here
	time.Sleep(5 * time.Millisecond)
	require.Len(t, prod.byTopic(TopicNowPlaying), 1) // still the first track
	cancel()
	<-done
}

func TestSessionClosesEveryReaderItOpened(t *testing.T) {
	store, lib, reqs := newFixture(t, "a", "b")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	dec := &closeRecordingDecoder{bytesPerTrack: chunkBytes * 2}
	f := newTestFeederWith(store, lib, reqs, enc, prod, clk, t.TempDir(),
		func(d *FeederDeps) { d.Decoder = dec })

	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()

	// Pump until the SECOND track has been announced. A second reader existing
	// no longer means the first track ended — the lookahead opens the next
	// item while the current one is still airing, which is the whole point of
	// it — so the now-playing frame for track two is what marks the boundary
	// as actually crossed.
	deadline := time.Now().Add(2 * time.Second)
	for len(prod.byTopic(TopicNowPlaying)) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the second track to air (opened=%d)", atomic.LoadInt32(&dec.opened))
		}
		select {
		case err := <-done:
			t.Fatalf("session ended early: %v", err)
		default:
			clk.step(250 * time.Millisecond)
			time.Sleep(time.Millisecond)
		}
	}

	// Assert MID-SESSION, not just at return: the finished track's reader must
	// be closed at item end. Checking only after RunSession returns is blind,
	// because the session-end backstop would close everything anyway — a build
	// that dropped the per-item cleanup entirely would keep every track's
	// ffmpeg process and its temp dir alive for the whole session (weeks, on
	// this station) and still satisfy an at-return-only assertion.
	require.Positive(t, atomic.LoadInt32(&dec.closed),
		"the finished track's reader must close at item end, not at session end")

	// Now take the station off air mid-track and drain the session.
	_, err := store.GoOffAir(ctx)
	require.NoError(t, err)
	require.NoError(t, drive(t, clk, done, 40))

	opened := atomic.LoadInt32(&dec.opened)
	require.GreaterOrEqual(t, opened, int32(2), "the session should have aired past its first track")
	require.Equal(t, opened, atomic.LoadInt32(&dec.closed),
		"every opened reader must be closed by the time RunSession returns")
}

// The lookahead exists so the incoming item is already fetched and decoded by
// the time the outgoing one ends. Its whole observable signature is WHEN the
// fetch happens: the second item's fetch must be requested while the first is
// still being fed, not after the first has hit EOF.
func TestPrefetchOpensBeforeTheBoundary(t *testing.T) {
	const trackBytes = chunkBytes * 6

	var mu sync.Mutex
	var fetchAt []int // bytes fed to the stream when each fetch was requested

	store, lib, reqs := newFixture(t, "a", "b")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeederWith(store, lib, reqs, enc, prod, clk, t.TempDir(),
		func(d *FeederDeps) {
			d.Decoder = fakeDecoder{bytesPerTrack: trackBytes}
			d.Fetch = func(_ context.Context, id, _ string) (string, error) {
				written := enc.firstSessionBytes()
				mu.Lock()
				fetchAt = append(fetchAt, written)
				mu.Unlock()
				return "/fake/" + id, nil
			}
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()

	for range 12 {
		clk.step(250 * time.Millisecond)
		time.Sleep(5 * time.Millisecond)
	}
	_, err := store.GoOffAir(ctx)
	require.NoError(t, err)
	require.NoError(t, drive(t, clk, done, 40))

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(fetchAt), 2, "at least two items should have been fetched")
	require.Less(t, fetchAt[1], 2*chunkBytes,
		"the second item's fetch must start while the first is still being fed, not at its EOF (%d bytes in)", trackBytes)
}

// A lookahead that fails to PLAN must consume nothing. planNext reports a
// pending next-up consumption alongside its error so that the feed goroutine
// can commit it (see planInline), but a lookahead may still be abandoned — by
// going off air, by a ctx cancellation, by the session ending — and an
// abandoned lookahead that had already thrown the commitment away would leave
// the station having skipped a track nothing ever aired.
//
// This pins startPrefetch calling planNext rather than planInline. Swapping the
// two is exactly the "consistency cleanup" a future maintainer reaches for, and
// nothing else in the suite notices.
func TestPrefetchDoesNotConsumeTheNextUpWhenPlanningFails(t *testing.T) {
	store, lib, reqs := newFixture(t, "t1")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, failingGetLibrary{lib}, reqs, enc, prod, clk, t.TempDir())
	ctx := context.Background()
	require.NoError(t, f.d.Sched.SetNextUp(ctx, schedule.NextUp{YTID: "t1", Title: "T1"}))

	pf := f.startPrefetch(ctx)
	<-pf.done

	require.ErrorIs(t, pf.err, errLibraryGetBroken)
	require.Nil(t, pf.rd, "a plan that failed must not have opened anything")

	got, ok, err := f.d.Sched.GetNextUp(ctx)
	require.NoError(t, err)
	require.True(t, ok, "the lookahead must not consume the next-up — only the feed goroutine may")
	require.Equal(t, "t1", got.YTID)
}

// getFailingAfterLibrary wraps a library.Library so Get succeeds n times and
// then fails forever. A library that fails from its very first Get never gets
// a track on air at all, so it can only ever exercise the FIRST boundary —
// this is what makes the lookahead's error arm reachable from a full
// RunSession, which is where the zero-plan trap lives.
type getFailingAfterLibrary struct {
	library.Library
	mu   sync.Mutex
	left int
}

func (l *getFailingAfterLibrary) Get(ctx context.Context, ytID string) (library.Track, bool, error) {
	l.mu.Lock()
	if l.left <= 0 {
		l.mu.Unlock()
		return library.Track{}, false, errLibraryGetBroken
	}
	l.left--
	l.mu.Unlock()
	return l.Library.Get(ctx, ytID)
}

// A plan that failed must never be aired. planNext returns a ZERO plan
// alongside its errors, and commitNext turns that into an empty airItem with
// neither skip nor stop set — and, worse, overwrites the error with nil. Commit
// it and the feeder airs a track with a blank YTID and no title, air-logs it,
// and publishes it as now-playing, poisoning the sample-clock resume anchor
// with something nobody heard. Only the error survives to end the session.
//
// The library here succeeds exactly twice — the first boundary's shuffle pick
// and commitNextUp's next-up pick — so the very next Get, the lookahead's
// read of that committed next-up, fails, and so does the inline re-plan that
// follows it. That is the only way to reach takePrefetch's error arm from a
// running session.
func TestFailedPlanIsNeverAiredAsATrack(t *testing.T) {
	store, lib, reqs := newFixture(t, "a", "b")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	log := NewMemAirLog()
	f := newTestFeederWith(store, lib, reqs, enc, prod, clk, t.TempDir(),
		func(d *FeederDeps) {
			d.Library = &getFailingAfterLibrary{Library: lib, left: 2}
			d.Log = log
		})

	done := make(chan error, 1)
	go func() { done <- f.RunSession(context.Background()) }()

	runErr := drive(t, clk, done, 40)
	require.ErrorIs(t, runErr, errLibraryGetBroken,
		"a plan that could not be made must end the session with its error")

	latest, ok, err := log.Latest(context.Background())
	require.NoError(t, err)
	require.True(t, ok, "the first track should still have aired before the library broke")
	require.NotEmpty(t, latest.YTID,
		"a failed plan must never reach commitNext: an empty airItem airs as a blank track")
}

// takePrefetch's error arm: a lookahead that failed hands back no reader and a
// freshly planned plan, so the caller opens inline. This is the fallback the
// whole change rests on, and it is the one arm a RunSession-level test cannot
// reach without a library that fails partway through.
func TestTakePrefetchPlansInlineAfterAFailedLookahead(t *testing.T) {
	store, lib, reqs := newFixture(t, "t1")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())
	ctx := context.Background()
	require.NoError(t, f.d.Sched.SetNextUp(ctx, schedule.NextUp{YTID: "t1", Title: "T1"}))

	finished := make(chan struct{})
	close(finished)
	failed := &prefetch{done: finished, err: errLibraryGetBroken}

	p, rd, cleanup, err := f.takePrefetch(ctx, failed)
	require.NoError(t, err, "a failed lookahead is not a session-ending error")
	require.Nil(t, rd, "a failed lookahead must hand back no reader")
	require.Nil(t, cleanup)
	require.Equal(t, "t1", p.item.track.YTID, "the caller must get a freshly planned item to open inline")
	require.True(t, p.consumedNextUp, "the inline re-plan owns the consumption the lookahead left alone")
}

// A lookahead whose OPEN fails (as opposed to its plan — see the two tests
// above) reports "opened nothing" rather than an error, so the boundary commits
// the plan and retries the open on the feed goroutine, where the pre-existing
// mark-failed-and-skip path lives. Either way the session must not end.
func TestPrefetchOpenSkipFallsBackToInlineOpen(t *testing.T) {
	store, lib, reqs := newFixture(t, "a", "b")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	var calls int32
	f := newTestFeederWith(store, lib, reqs, enc, prod, clk, t.TempDir(),
		func(d *FeederDeps) {
			d.Fetch = func(_ context.Context, id, _ string) (string, error) {
				if atomic.AddInt32(&calls, 1) == 2 {
					return "", errors.New("transient minio failure")
				}
				return "/fake/" + id, nil
			}
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()
	for range 12 {
		clk.step(250 * time.Millisecond)
		time.Sleep(5 * time.Millisecond)
	}
	_, err := store.GoOffAir(ctx)
	require.NoError(t, err)
	require.NoError(t, drive(t, clk, done, 40), "a failed prefetch must not end the session with an error")
	require.GreaterOrEqual(t, atomic.LoadInt32(&calls), int32(3),
		"the feeder should have retried the fetch inline after the prefetch failed")
}

// A lookahead the session never gets to air still owns an ffmpeg decode
// process and a temp dir holding a whole audio artifact. Going off air with
// one in flight must close it, or the leak is permanent: FFDecoder binds the
// process to RunSession's ctx, which is Engine.Run's ctx, which is process
// lifetime.
func TestAbandonedPrefetchIsClosed(t *testing.T) {
	store, lib, reqs := newFixture(t, "a", "b")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	dec := &closeRecordingDecoder{bytesPerTrack: chunkBytes * 4}
	f := newTestFeederWith(store, lib, reqs, enc, prod, clk, t.TempDir(),
		func(d *FeederDeps) { d.Decoder = dec })

	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()

	for range 3 {
		clk.step(250 * time.Millisecond)
		time.Sleep(5 * time.Millisecond)
	}
	// Go off air with a prefetch in flight for the next item.
	_, err := store.GoOffAir(ctx)
	require.NoError(t, err)
	require.NoError(t, drive(t, clk, done, 40))

	require.Equal(t, atomic.LoadInt32(&dec.opened), atomic.LoadInt32(&dec.closed),
		"a prefetched reader abandoned by going off air must still be closed")
}

// The session backstop must shed a reader's registration as soon as that
// reader closes, while still closing anything genuinely left open at session
// end. Append-only would pin one dead closure per track for the whole life of
// a session, which on a 24/7 station is weeks.
func TestOpenSetDropsClosedReadersAndClosesTheRest(t *testing.T) {
	var set openSet
	var closed []string

	first := set.own(once(func() { closed = append(closed, "first") }))
	first()
	require.Empty(t, set.open, "a closed reader must leave no registration behind")

	// Readers still open at session end are closed by the backstop, newest first.
	older := set.own(once(func() { closed = append(closed, "older") }))
	newer := set.own(once(func() { closed = append(closed, "newer") }))
	set.closeAll()
	require.Equal(t, []string{"first", "newer", "older"}, closed)

	// A cleanup landing after closeAll (session end racing an item's own
	// cleanup) neither panics nor re-runs anything.
	require.NotPanics(t, first)
	require.NotPanics(t, older)
	require.NotPanics(t, newer)
	set.closeAll()
	require.Equal(t, []string{"first", "newer", "older"}, closed)
}

// --- talk-break fakes and tests (v2) ---

// fakeTalkSource hands out clips in order, but only after at least minFinished
// TrackFinished calls (mimics prepare-ahead: nothing ready at session start).
type fakeTalkSource struct {
	mu          sync.Mutex
	clips       []Clip
	minFinished int
	finished    []Entry
	takes       []Entry
}

func (s *fakeTalkSource) TrackFinished(e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished = append(s.finished, e)
}

func (s *fakeTalkSource) Take(just Entry) (Clip, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.takes = append(s.takes, just)
	if len(s.clips) == 0 || len(s.finished) < s.minFinished {
		return Clip{}, false
	}
	c := s.clips[0]
	s.clips = s.clips[1:]
	return c, true
}

func (s *fakeTalkSource) finishedYTIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.finished {
		out = append(out, e.YTID)
	}
	return out
}

// writeClipFile writes n bytes of silence as a raw PCM clip file.
func writeClipFile(t *testing.T, dir string, n int) string {
	t.Helper()
	p := filepath.Join(dir, "clip.pcm")
	require.NoError(t, os.WriteFile(p, make([]byte, n), 0o644))
	return p
}

func pumpFrames(t *testing.T, clk *fakeClock, prod *fakeProducer, done <-chan error, topic string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(prod.byTopic(topic)) < want {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d frames on %s (have %d)", want, topic, len(prod.byTopic(topic)))
		}
		select {
		case err := <-done:
			t.Fatalf("session ended early: %v", err)
		default:
			clk.step(250 * time.Millisecond)
		}
	}
}

// A ready clip airs between tracks: track frame, dj frame, track frame; the
// sample clock advances by the clip's real PCM length; the file is deleted;
// the clip itself never fires TrackFinished.
func TestTalkClipAirsBetweenTracks(t *testing.T) {
	store, lib, reqs := newFixture(t, "a", "b")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	clipPath := writeClipFile(t, t.TempDir(), chunkBytes*2) // 0.5s of PCM
	talk := &fakeTalkSource{minFinished: 1, clips: []Clip{{
		Path: clipPath, DurationS: 0.5, Kind: ClipStationID,
	}}}
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())
	f.d.Talk = talk

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()
	require.Eventually(t, func() bool { return f.SessionDir() != "" }, time.Second, time.Millisecond)

	pumpFrames(t, clk, prod, done, TopicNowPlaying, 3)
	cancel()
	require.NoError(t, drive(t, clk, done, 100))

	frames := prod.byTopic(TopicNowPlaying)
	require.Contains(t, frames[0], `"kind":"track"`)
	require.Contains(t, frames[1], `"kind":"dj"`)
	require.Contains(t, frames[1], `"title":"Tiểu Dương Dương"`)
	require.Contains(t, frames[1], `"durationSeconds":1`) // 0.5 rounds to 1... see note below
	require.Contains(t, frames[2], `"kind":"track"`)

	// Sample clock: dj started when track a (2 chunks = 0.5s) ended; the next
	// track started 0.5s (the clip's PCM length) after the dj frame.
	var f1, f2 struct {
		StartedAt time.Time `json:"startedAt"`
	}
	require.NoError(t, json.Unmarshal([]byte(frames[1]), &f1))
	require.NoError(t, json.Unmarshal([]byte(frames[2]), &f2))
	require.Equal(t, 500*time.Millisecond, f2.StartedAt.Sub(f1.StartedAt))

	_, err := os.Stat(clipPath)
	require.True(t, os.IsNotExist(err), "clip file must be deleted after airing")
	// Track a finished normally; the dj clip must NOT appear here; track b was
	// cut by the ctx cancel (session stop), which by contract does not fire
	// TrackFinished either.
	require.Equal(t, []string{"a"}, talk.finishedYTIDs())
}

// Take receives the entry that just finished (freshness anchor input) — zero
// Entry before any music aired this session.
func TestTakeReceivesJustFinishedEntry(t *testing.T) {
	store, lib, reqs := newFixture(t, "a")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	talk := &fakeTalkSource{} // never ready
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())
	f.d.Talk = talk

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()
	require.Eventually(t, func() bool { return f.SessionDir() != "" }, time.Second, time.Millisecond)
	pumpFrames(t, clk, prod, done, TopicNowPlaying, 2)
	cancel()
	require.NoError(t, drive(t, clk, done, 100))

	talk.mu.Lock()
	defer talk.mu.Unlock()
	require.GreaterOrEqual(t, len(talk.takes), 2)
	require.Equal(t, "", talk.takes[0].YTID, "first Take: nothing finished yet")
	require.Equal(t, "a", talk.takes[1].YTID, "second Take: track a just finished")
}

// A clip whose file fails to open is skipped silently — no dj frame, music
// continues in the same boundary pass.
func TestTalkClipOpenFailureFallsThroughToMusic(t *testing.T) {
	store, lib, reqs := newFixture(t, "a")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	talk := &fakeTalkSource{clips: []Clip{{Path: "/nonexistent/clip.pcm", DurationS: 1, Kind: ClipStationID}}}
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())
	f.d.Talk = talk

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()
	require.Eventually(t, func() bool { return f.SessionDir() != "" }, time.Second, time.Millisecond)
	pumpFrames(t, clk, prod, done, TopicNowPlaying, 2)
	cancel()
	require.NoError(t, drive(t, clk, done, 100))

	for _, fr := range prod.byTopic(TopicNowPlaying) {
		require.NotContains(t, fr, `"kind":"dj"`)
	}
}

// Encoder crash mid-clip: the session restarts, the clip's remainder is
// skipped (never re-fed), its file is deleted, and music continues.
func TestTalkClipCrashSkipsRemainder(t *testing.T) {
	store, lib, reqs := newFixture(t, "a")
	enc := &crashingEncoder{aliveFrom: 1} // session 0 crashes instantly
	prod, clk := &fakeProducer{}, newFakeClock()
	clipPath := writeClipFile(t, t.TempDir(), chunkBytes*4)
	talk := &fakeTalkSource{clips: []Clip{{Path: clipPath, DurationS: 1, Kind: ClipStationID}}}
	f := newTestFeeder(store, lib, reqs, &enc.fakeEncoder, prod, clk, t.TempDir())
	f.d.Encoder = enc
	f.d.Talk = talk

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()
	require.Eventually(t, func() bool { return f.SessionDir() != "" }, time.Second, time.Millisecond)
	pumpFrames(t, clk, prod, done, TopicNowPlaying, 2) // dj frame + track a on the restarted session
	cancel()
	require.NoError(t, drive(t, clk, done, 100))

	require.GreaterOrEqual(t, enc.count(), 2, "encoder must have been restarted")
	_, err := os.Stat(clipPath)
	require.True(t, os.IsNotExist(err), "clip file must be deleted after a crash")
	frames := prod.byTopic(TopicNowPlaying)
	// RunSession's teardown defer always appends the off-air sentinel last
	// (see TestOperatorOffAirEndsSession et al.), so the frame right before
	// it is the one that proves music continued after the crash.
	require.Contains(t, frames[len(frames)-2], `"kind":"track"`)
}

// TestAtMostOneTalkBreakPerSeam covers the IMPORTANT fix: at most one talk
// break per seam. A talk source with two clips ready up-front (minFinished
// left at its zero value, so nothing gates Take) must NOT air them
// back-to-back — the feeder's awaitMusic guard forces a boundary() pick
// between them, so the now-playing frames strictly alternate dj/track/dj.
func TestAtMostOneTalkBreakPerSeam(t *testing.T) {
	store, lib, reqs := newFixture(t, "a")
	enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
	clip1 := writeClipFile(t, t.TempDir(), chunkBytes*2)
	clip2 := writeClipFile(t, t.TempDir(), chunkBytes*2)
	talk := &fakeTalkSource{clips: []Clip{
		{Path: clip1, DurationS: 0.5, Kind: ClipStationID},
		{Path: clip2, DurationS: 0.5, Kind: ClipStationID},
	}}
	f := newTestFeeder(store, lib, reqs, enc, prod, clk, t.TempDir())
	f.d.Talk = talk

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.RunSession(ctx) }()
	require.Eventually(t, func() bool { return f.SessionDir() != "" }, time.Second, time.Millisecond)

	pumpFrames(t, clk, prod, done, TopicNowPlaying, 3)
	cancel()
	require.NoError(t, drive(t, clk, done, 100))

	frames := prod.byTopic(TopicNowPlaying)
	require.Contains(t, frames[0], `"kind":"dj"`)
	require.Contains(t, frames[1], `"kind":"track"`)
	require.Contains(t, frames[2], `"kind":"dj"`)

	for i := 1; i < len(frames); i++ {
		require.False(t, strings.Contains(frames[i-1], `"kind":"dj"`) && strings.Contains(frames[i], `"kind":"dj"`),
			"two consecutive dj frames at %d,%d: %q, %q", i-1, i, frames[i-1], frames[i])
	}
}
