package director

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eino-contrib/jsonschema"
	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/persona"
	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/schedule"
	"github.com/the-algovn/radio-service/internal/spend"
	"github.com/the-algovn/radio-service/internal/station"
	"github.com/the-algovn/radio-service/internal/talkmem"
)

// fakeSpeaker returns 1s of 8kHz silence as a valid WAV -- keyless dev mode.
// The byte layout is copied verbatim from the deleted internal/voice.Fake:
// render.go's silenceFloorLUFS path measures -inf LUFS on exactly these
// bytes, so an equivalent-looking WAV with a different header would not
// exercise the same plain-decode branch.
type fakeSpeaker struct{}

func (fakeSpeaker) Synthesize(_ context.Context, _, _ string, _ float64) ([]byte, string, float64, string, error) {
	const sampleRate, seconds = 8000, 1
	n := sampleRate * seconds * 2 // 16-bit mono
	buf := make([]byte, 44+n)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+n))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(buf[22:24], 1) // mono
	binary.LittleEndian.PutUint32(buf[24:28], sampleRate)
	binary.LittleEndian.PutUint32(buf[28:32], sampleRate*2)
	binary.LittleEndian.PutUint16(buf[32:34], 2)
	binary.LittleEndian.PutUint16(buf[34:36], 16)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(n))
	return buf, "wav", 0, "fake", nil
}

// seqModel returns scripted raw outputs in sequence, recording the prompts it
// was given and optionally failing outright.
type seqModel struct {
	raws                 []string
	calls                int
	err                  error  // non-nil → Generate fails
	lastSystem, lastUser string // the prompts of the most recent call
}

func (m *seqModel) Name() string     { return "claude-test" }
func (m *seqModel) Provider() string { return "fake" }
func (m *seqModel) Generate(_ context.Context, system, user string, _ *jsonschema.Schema) (string, error) {
	m.lastSystem, m.lastUser = system, user
	m.calls++
	if m.err != nil {
		return "", m.err
	}
	return m.raws[min(m.calls-1, len(m.raws)-1)], nil
}

type errVoice struct{}

func (errVoice) Synthesize(context.Context, string, string, float64) ([]byte, string, float64, string, error) {
	return nil, "", 0, "", errors.New("tts down")
}

// errListeners fails Count, to exercise buildBrief's degradation of the
// listener field.
type errListeners struct{}

func (errListeners) Beat(context.Context, string) error { return nil }
func (errListeners) Count(context.Context) (int, error) {
	return 0, errors.New("listeners down")
}

const goodRaw = `{"script":"Vừa rồi là một bản nhạc đêm dịu dàng, cảm ơn bạn đã cùng nghe.","summary":"backsell đêm","used_phrases":["cùng nghe"]}`
const digitRaw = `{"script":"Bài này ra năm 2020 đó nha.","summary":"x","used_phrases":[]}`

// memoryRaw carries a distinctive summary and phrase so the show-memory
// assertions cannot pass by accident. The script reuses goodRaw's text, which
// is already digit-free and inside testDJ's 450-rune cap.
const memoryRaw = `{"script":"Vừa rồi là một bản nhạc đêm dịu dàng, cảm ơn bạn đã cùng nghe.","summary":"kể chuyện cơn mưa Sài Gòn","used_phrases":["bạn nghe đài thân mến"]}`

// testDJ mirrors the old fixture Deps knobs for direct prepare() calls.
var testDJ = station.DJSettings{VoiceID: "fake", Rate: 1.0, BreakEvery: 2, StationIDMin: 60, MaxChars: 450}

// testStation carries testDJ for prepare()'s station.Station parameter.
var testStation = station.Station{OnAir: true, AIEnabled: true, DJ: testDJ}

// fakeRender writes 96000 bytes (0.5s) to outPath.
func fakeRender(_ context.Context, _, outPath string) (float64, error) {
	if err := os.WriteFile(outPath, make([]byte, 96000), 0o644); err != nil {
		return 0, err
	}
	return 0.5, nil
}

type prepFixture struct {
	dr     *Director
	clk    *dirClock
	ledger *spend.MemLedger
	log    *live.MemAirLog
	mem    *talkmem.MemStore
	model  *seqModel
}

func newPrepFixture(t *testing.T, model *seqModel) *prepFixture {
	t.Helper()
	clk := newDirClock()
	ledger := spend.NewMemLedger()
	airLog := live.NewMemAirLog()
	mem := talkmem.NewMemStore()
	personaDir := t.TempDir()
	require.NoError(t, persona.Save(personaDir, "# Tiểu Dương Dương\nGiọng ấm."))
	store := station.NewMemStore()
	_, err := store.UpdateDJSettings(context.Background(), station.DJSettings{
		VoiceID: "fake", Rate: 1.0, BreakEvery: 2, StationIDMin: 60, MaxChars: 450})
	require.NoError(t, err)
	dr := New(Deps{
		Model: model, Voice: fakeSpeaker{}, Ledger: ledger,
		Station: store, Listeners: live.NewMemListeners(time.Now),
		AirLog: airLog, TalkMem: mem,
		PersonaDir: personaDir, StationIDsPath: writeIDs(t, "đài thân mến\n"),
		DataDir: t.TempDir(), BudgetUSD: 1.0,
		Render: fakeRender, Clock: clk, Location: time.UTC,
	})
	return &prepFixture{dr: dr, clk: clk, ledger: ledger, log: airLog, mem: mem, model: model}
}

func ledgerLabels(t *testing.T, l *spend.MemLedger) []string {
	t.Helper()
	lines, err := l.All(context.Background())
	require.NoError(t, err)
	var out []string
	for _, ln := range lines {
		out = append(out, ln.Kind+":"+ln.Label)
	}
	return out
}

func TestPrepareSeamHappyPath(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	anchor := live.Entry{YTID: "a", Title: "Bài A", Artist: "Ca sĩ",
		StartedAt: time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC), Source: "ai", Reason: "hợp đêm"}
	require.NoError(t, f.log.Append(context.Background(), anchor))

	clip, ok := f.dr.prepare(context.Background(), live.ClipSeam, testStation)
	require.True(t, ok)
	require.Equal(t, live.ClipSeam, clip.Kind)
	require.Equal(t, "a", clip.AnchorYTID)
	require.Equal(t, anchor.StartedAt, clip.AnchorStartedAt)
	require.InDelta(t, 0.5, clip.DurationS, 0.001)
	_, err := os.Stat(clip.Path)
	require.NoError(t, err, "rendered clip file exists")
	require.Equal(t, []string{"tts:director:seam"}, ledgerLabels(t, f.ledger), "LLM spend is now priced by the Eino callback, not the director")
	lines, _ := f.ledger.All(context.Background())
	require.Zero(t, lines[0].CostUSD, "the fake speaker reports zero cost directly")
	got, err := f.dr.d.TalkMem.Recent(context.Background(), time.Time{}, 8)
	require.NoError(t, err)
	require.Len(t, got, 1, "seam summary recorded")
}

func TestPrepareSeamRetriesOnceOnViolations(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{digitRaw, goodRaw}})
	require.NoError(t, f.log.Append(context.Background(), live.Entry{YTID: "a", Title: "A", StartedAt: time.Now()}))
	_, ok := f.dr.prepare(context.Background(), live.ClipSeam, testStation)
	require.True(t, ok)
	require.Equal(t, 2, f.model.calls)
	require.Equal(t, []string{"tts:director:seam"}, ledgerLabels(t, f.ledger),
		"no llm line: the director no longer prices either attempt itself")
}

func TestPrepareSeamAbortsAfterSecondViolation(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{digitRaw, digitRaw}})
	require.NoError(t, f.log.Append(context.Background(), live.Entry{YTID: "a", Title: "A", StartedAt: time.Now()}))
	_, ok := f.dr.prepare(context.Background(), live.ClipSeam, testStation)
	require.False(t, ok)
	require.Equal(t, 2, f.model.calls)
	require.Empty(t, ledgerLabels(t, f.ledger), "no llm spend recorded; the director no longer prices its own calls")
	got, err := f.dr.d.TalkMem.Recent(context.Background(), time.Time{}, 8)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestPrepareSeamNoAirLogEntryQuietSkip(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	_, ok := f.dr.prepare(context.Background(), live.ClipSeam, testStation)
	require.False(t, ok, "nothing airing → nothing to talk about")
	require.Zero(t, f.model.calls)
}

func TestPrepareStationIDSkipsLLM(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	clip, ok := f.dr.prepare(context.Background(), live.ClipStationID, testStation)
	require.True(t, ok)
	require.Equal(t, live.ClipStationID, clip.Kind)
	require.Equal(t, "", clip.AnchorYTID, "station_id has a zero anchor")
	require.Zero(t, f.model.calls, "no LLM call")
	require.Equal(t, []string{"tts:director:station_id"}, ledgerLabels(t, f.ledger))
	require.Equal(t, "đài thân mến", clip.Script)
}

func TestPrepareTTSFailure(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	f.dr.d.Voice = errVoice{}
	require.NoError(t, f.log.Append(context.Background(), live.Entry{YTID: "a", Title: "A", StartedAt: time.Now()}))
	_, ok := f.dr.prepare(context.Background(), live.ClipSeam, testStation)
	require.False(t, ok)
	require.Empty(t, ledgerLabels(t, f.ledger), "no llm line: the director no longer prices the call; tts never ran")
}

func TestPrepareRenderFailureCleansUp(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	f.dr.d.Render = func(_ context.Context, _, _ string) (float64, error) { return 0, errors.New("boom") }
	require.NoError(t, f.log.Append(context.Background(), live.Entry{YTID: "a", Title: "A", StartedAt: time.Now()}))
	_, ok := f.dr.prepare(context.Background(), live.ClipSeam, testStation)
	require.False(t, ok)
	entries, err := os.ReadDir(f.dr.d.DataDir)
	require.NoError(t, err)
	require.Empty(t, entries, "no temp files left behind")
}

func TestBuildBriefContents(t *testing.T) {
	ctx := context.Background()
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	require.NoError(t, f.dr.d.Listeners.Beat(ctx, "s1"))
	onAir := f.clk.Now().Add(-90 * time.Minute)
	st := station.Station{OnAir: true, AIEnabled: true, OnAirSince: &onAir, DJ: testDJ}
	just := live.Entry{Title: "Bài A", Artist: "Ca sĩ", Source: "listener", RequestedByName: "Minh"}

	b := f.dr.buildBrief(ctx, st, just, nil, 450)
	require.Equal(t, "seam", b.Type)
	require.Equal(t, "Bài A", b.JustPlayed.Title)
	require.Equal(t, "Minh", b.JustPlayed.RequestedByName)
	require.Equal(t, 90, b.OnAirForMin)
	require.Equal(t, 1, b.Listeners)
	require.Equal(t, 450, b.MaxChars)
	require.NotEmpty(t, b.Daypart)
	require.Nil(t, b.ComingUp, "up was nil — nothing to promise")
}

// tonight must be scoped to THIS broadcast session. The truth rail tells her
// to state brief facts plainly, so an unscoped read would have her calling a
// song from a broadcast two days ago part of tonight.
func TestBuildBriefScopesTonightToTheSession(t *testing.T) {
	ctx := context.Background()
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	onAir := f.clk.Now().Add(-2 * time.Hour)

	for _, e := range []live.Entry{
		{YTID: "old", Title: "đêm trước", Artist: "C",
			StartedAt: f.clk.Now().Add(-40 * time.Hour), DurationS: 60},
		{YTID: "early", Title: "đêm nay sớm", Artist: "B",
			StartedAt: f.clk.Now().Add(-90 * time.Minute), DurationS: 60},
		{YTID: "late", Title: "đêm nay muộn", Artist: "A",
			StartedAt: f.clk.Now().Add(-10 * time.Minute), DurationS: 60},
	} {
		require.NoError(t, f.log.Append(ctx, e))
	}

	st := station.Station{OnAir: true, AIEnabled: true, OnAirSince: &onAir, DJ: testDJ}
	b := f.dr.buildBrief(ctx, st, live.Entry{Title: "vừa xong"}, nil, 1500)

	var titles []string
	for _, tr := range b.Tonight {
		titles = append(titles, tr.Title)
	}
	require.Equal(t, []string{"đêm nay sớm", "đêm nay muộn"}, titles,
		"oldest first, and the previous broadcast excluded")
	require.Equal(t, 120, b.OnAirForMin)
	require.Nil(t, b.ComingUp)
}

func TestBuildBriefIncludesTheThreadOldestFirst(t *testing.T) {
	ctx := context.Background()
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	onAir := f.clk.Now().Add(-time.Hour)

	require.NoError(t, f.mem.Append(ctx, talkmem.Entry{Kind: live.ClipSeam,
		Summary: "kể về mưa", Phrases: []string{"khuya rồi"}}))
	require.NoError(t, f.mem.Append(ctx, talkmem.Entry{Kind: live.ClipSeam,
		Summary: "nhắc bạn Ngọc"}))

	st := station.Station{OnAir: true, AIEnabled: true, OnAirSince: &onAir, DJ: testDJ}
	b := f.dr.buildBrief(ctx, st, live.Entry{Title: "vừa xong"}, nil, 1500)

	require.Equal(t, []string{"kể về mưa", "nhắc bạn Ngọc"}, b.Thread)
	require.Equal(t, []string{"khuya rồi"}, b.RecentPhrases)
}

func TestBuildBriefCarriesComingUpProvenance(t *testing.T) {
	ctx := context.Background()
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	onAir := f.clk.Now().Add(-time.Hour)

	st := station.Station{OnAir: true, AIEnabled: true, OnAirSince: &onAir, DJ: testDJ}
	up := &live.Upcoming{
		Track:           library.Track{Title: "Em Của Ngày Hôm Qua", Channel: "Sơn Tùng M-TP"},
		RequestID:       "req-1",
		Source:          request.SourceListener,
		RequestedByName: "Ngọc",
		Reason:          "vì trời mưa",
	}

	b := f.dr.buildBrief(ctx, st, live.Entry{Title: "vừa xong"}, up, 1500)

	require.NotNil(t, b.ComingUp)
	require.Equal(t, "Em Của Ngày Hôm Qua", b.ComingUp.Title)
	require.Equal(t, "Sơn Tùng M-TP", b.ComingUp.Artist)
	require.Equal(t, request.SourceListener, b.ComingUp.Source)
	require.Equal(t, "Ngọc", b.ComingUp.RequestedByName)
	require.Equal(t, "vì trời mưa", b.ComingUp.Reason)
}

// RunOnce only reaches prepare when the listener count read SUCCEEDED and was
// greater than zero, so by the time buildBrief runs, a zero can only mean the
// SECOND read failed — there is no legitimate zero to preserve. A failed read
// must therefore leave listeners out of the marshalled brief entirely, not
// ship a false "listeners": 0 into a brief whose contract is "these facts are
// true, state them plainly".
func TestBuildBriefOmitsListenersOnReadFailure(t *testing.T) {
	ctx := context.Background()
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	f.dr.d.Listeners = errListeners{}
	onAir := f.clk.Now().Add(-time.Hour)
	st := station.Station{OnAir: true, AIEnabled: true, OnAirSince: &onAir, DJ: testDJ}

	b := f.dr.buildBrief(ctx, st, live.Entry{Title: "vừa xong"}, nil, 1500)
	require.Zero(t, b.Listeners)

	j, err := json.Marshal(b)
	require.NoError(t, err)
	require.NotContains(t, string(j), "listeners",
		"a failed listener read must not ship a false zero as fact")
}

// recVoice records the last Synthesize arguments.
type recVoice struct {
	mu      sync.Mutex
	voiceID string
	rate    float64
}

func (r *recVoice) Synthesize(_ context.Context, _ string, voiceID string, rate float64) ([]byte, string, float64, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.voiceID, r.rate = voiceID, rate
	return []byte("mp3"), "mp3", 0, "google", nil
}

// The whole point of the DB move: a settings change applies to the NEXT
// prepared clip without a restart (spec §4). Initial settings are set
// explicitly via the store so the test is independent of the migration
// default cadence (which is break_every 1).
func TestRunOnceSettingsChangeAffectsNextPrepare(t *testing.T) {
	ctx := context.Background()
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw, goodRaw}})
	rec := &recVoice{}
	f.dr.d.Voice = rec
	onAir(t, f)
	withListener(t, f)

	_, err := f.dr.d.Station.UpdateDJSettings(ctx, station.DJSettings{
		VoiceID: "vi-VN-Neural2-A", Rate: 1.0, BreakEvery: 2, StationIDMin: 60, MaxChars: 1024})
	require.NoError(t, err)

	start := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	require.NoError(t, f.log.Append(ctx, live.Entry{YTID: "a", Title: "A", StartedAt: start}))
	f.dr.RunOnce(ctx) // observes the on-air transition; 0 + current = 1 < 2 → nothing due
	require.False(t, slotFilled(f.dr))
	f.dr.TrackFinished(live.Entry{YTID: "a"})
	f.dr.RunOnce(ctx) // 1 finished + current = 2 >= 2 → prepares
	require.True(t, slotFilled(f.dr))
	require.Equal(t, "vi-VN-Neural2-A", rec.voiceID, "first prepare uses the current settings")

	_, ok := f.dr.Take(live.Entry{YTID: "a", StartedAt: start}) // air it; counter resets
	require.True(t, ok)

	_, err = f.dr.d.Station.UpdateDJSettings(ctx, station.DJSettings{
		VoiceID: "vi-VN-Chirp3-HD-Aoede", Rate: 1.2, BreakEvery: 2, StationIDMin: 60, MaxChars: 1024})
	require.NoError(t, err)

	require.NoError(t, f.log.Append(ctx, live.Entry{YTID: "b", Title: "B", StartedAt: start.Add(3 * time.Minute)}))
	f.dr.TrackFinished(live.Entry{YTID: "b"})
	f.dr.RunOnce(ctx)
	require.True(t, slotFilled(f.dr))
	require.Equal(t, "vi-VN-Chirp3-HD-Aoede", rec.voiceID, "next prepare uses the updated settings")
	require.Equal(t, 1.2, rec.rate)
}

type fakePinner struct {
	calls int
	last  schedule.NextUp
	err   error
}

func (p *fakePinner) SetNextUp(_ context.Context, n schedule.NextUp) error {
	p.calls++
	p.last = n
	return p.err
}

// seamFixture is newPrepFixture wired for the pin: a recording pinner, a peek
// each test sets after construction, and an anchor already in the air log so
// prepare() gets past its Latest() read.
type seamFixture struct {
	*prepFixture
	pin  *fakePinner
	peek func(context.Context) (live.Upcoming, bool, error)
}

func newSeamFixture(t *testing.T, model *seqModel) *seamFixture {
	t.Helper()
	f := newPrepFixture(t, model)
	sf := &seamFixture{prepFixture: f, pin: &fakePinner{}}
	f.dr.d.Sched = sf.pin
	f.dr.d.Peek = func(ctx context.Context) (live.Upcoming, bool, error) {
		if sf.peek == nil {
			return live.Upcoming{}, false, nil
		}
		return sf.peek(ctx)
	}
	require.NoError(t, f.log.Append(context.Background(), live.Entry{
		YTID: "anchor", Title: "Bài vừa xong", Artist: "Ca sĩ",
		StartedAt: f.clk.Now().Add(-4 * time.Minute), DurationS: 200}))
	return sf
}

// Case 1: an ALREADY committed next-up is binding on its own — planNext reads
// next-up first — so the director must not write. A redundant write is not
// merely wasteful: it would be the only code path that could clobber a
// commitment the feeder just made.
func TestPreparePinSkippedWhenAlreadyCommitted(t *testing.T) {
	sf := newSeamFixture(t, &seqModel{raws: []string{goodRaw}})
	sf.peek = func(context.Context) (live.Upcoming, bool, error) {
		return live.Upcoming{Track: library.Track{YTID: "yt1", Title: "T", Channel: "C"},
			Committed: true}, true, nil
	}

	clip, ok := sf.dr.prepare(context.Background(), live.ClipSeam, testStation)
	require.True(t, ok)
	require.NotEmpty(t, clip.Path)
	require.Equal(t, 0, sf.pin.calls, "an existing commitment needs no pin")
}

// Case 2: the head of the ready queue is NOT stable (a listener request can
// outrank a waiting AI pick, and Reorder can jump anything to the front), so
// promising it requires a pin that carries the request id.
func TestPreparePinsTheReadyQueueHeadWithItsRequestID(t *testing.T) {
	sf := newSeamFixture(t, &seqModel{raws: []string{goodRaw}})
	sf.peek = func(context.Context) (live.Upcoming, bool, error) {
		return live.Upcoming{
			Track:     library.Track{YTID: "yt2", Title: "Em Của Ngày Hôm Qua", Channel: "Sơn Tùng M-TP"},
			RequestID: "req-7", Source: request.SourceListener, RequestedByName: "Ngọc",
		}, true, nil
	}

	_, ok := sf.dr.prepare(context.Background(), live.ClipSeam, testStation)
	require.True(t, ok)
	require.Equal(t, 1, sf.pin.calls)
	require.Equal(t, schedule.NextUp{YTID: "yt2", Title: "Em Của Ngày Hôm Qua",
		Channel: "Sơn Tùng M-TP", RequestID: "req-7"}, sf.pin.last)
	// The promise must reach the PROMPT, not just the schedule. Without this,
	// every test here still passes if prepare kept handing buildBrief a nil
	// `up` — the whole point of the peek would be unasserted.
	require.Contains(t, sf.model.lastUser, "Em Của Ngày Hôm Qua",
		"coming_up must reach the model")
}

// Case 3: nothing knowable — promise nothing, write nothing. Deliberately NOT
// a self-rolled shuffle pin: the feeder's lookahead has already picked its own
// random track, and a different pin would force it to discard an opened reader
// and re-open inline at the seam — an audible stall after every break.
func TestPrepareNoPromiseWhenPeekFindsNothing(t *testing.T) {
	sf := newSeamFixture(t, &seqModel{raws: []string{goodRaw}})
	sf.peek = func(context.Context) (live.Upcoming, bool, error) {
		return live.Upcoming{}, false, nil
	}

	_, ok := sf.dr.prepare(context.Background(), live.ClipSeam, testStation)
	require.True(t, ok, "a backsell-only break still airs")
	require.Equal(t, 0, sf.pin.calls)
	require.NotContains(t, sf.model.lastUser, "coming_up")
}

// The pin must be the LAST step. A generation failure that had already
// reordered the queue would move a listener's song for a break that never airs.
func TestPrepareDoesNotPinWhenGenerationFails(t *testing.T) {
	sf := newSeamFixture(t, &seqModel{raws: []string{goodRaw}})
	sf.peek = func(context.Context) (live.Upcoming, bool, error) {
		return live.Upcoming{Track: library.Track{YTID: "yt3"}, RequestID: "req-1"}, true, nil
	}
	sf.model.err = errors.New("boom")

	_, ok := sf.dr.prepare(context.Background(), live.ClipSeam, testStation)
	require.False(t, ok)
	require.Equal(t, 0, sf.pin.calls)
}

// An unbacked promise is the one thing §3 exists to prevent, so a pin failure
// discards the clip rather than airing it.
func TestPreparePinFailureDiscardsTheClip(t *testing.T) {
	sf := newSeamFixture(t, &seqModel{raws: []string{goodRaw}})
	sf.peek = func(context.Context) (live.Upcoming, bool, error) {
		return live.Upcoming{Track: library.Track{YTID: "yt4"}, RequestID: "req-1"}, true, nil
	}
	sf.pin.err = errors.New("db down")

	clip, ok := sf.dr.prepare(context.Background(), live.ClipSeam, testStation)
	require.False(t, ok)
	require.Empty(t, clip.Path)
	// and no orphaned pcm left behind
	entries, err := os.ReadDir(sf.dr.d.DataDir)
	require.NoError(t, err)
	for _, e := range entries {
		require.False(t, strings.HasSuffix(e.Name(), ".pcm"), "clip %s was left on disk", e.Name())
	}
}

// A peek error is not fatal: she backsells.
func TestPreparePeekErrorFallsBackToBacksellOnly(t *testing.T) {
	sf := newSeamFixture(t, &seqModel{raws: []string{goodRaw}})
	sf.peek = func(context.Context) (live.Upcoming, bool, error) {
		return live.Upcoming{}, false, errors.New("read failed")
	}

	_, ok := sf.dr.prepare(context.Background(), live.ClipSeam, testStation)
	require.True(t, ok)
	require.Equal(t, 0, sf.pin.calls)
}

// A station_id never promises anything and never pins.
func TestPrepareStationIDDoesNotPin(t *testing.T) {
	sf := newSeamFixture(t, &seqModel{raws: []string{goodRaw}})
	sf.peek = func(context.Context) (live.Upcoming, bool, error) {
		return live.Upcoming{Track: library.Track{YTID: "yt5"}, RequestID: "r"}, true, nil
	}

	_, ok := sf.dr.prepare(context.Background(), live.ClipStationID, testStation)
	require.True(t, ok)
	require.Equal(t, 0, sf.pin.calls)
}

func TestPrepareRecordsShowMemory(t *testing.T) {
	ctx := context.Background()
	sf := newSeamFixture(t, &seqModel{raws: []string{memoryRaw}})
	sf.peek = func(context.Context) (live.Upcoming, bool, error) { return live.Upcoming{}, false, nil }

	_, ok := sf.dr.prepare(ctx, live.ClipSeam, testStation)
	require.True(t, ok)

	got, err := sf.dr.d.TalkMem.Recent(ctx, time.Time{}, 8)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, live.ClipSeam, got[0].Kind)
	require.Equal(t, "kể chuyện cơn mưa Sài Gòn", got[0].Summary)
	require.Equal(t, []string{"bạn nghe đài thân mến"}, got[0].Phrases)
}

// A station_id is pre-written, not authored — it must not enter show memory.
func TestPrepareStationIDDoesNotRecordShowMemory(t *testing.T) {
	ctx := context.Background()
	sf := newSeamFixture(t, &seqModel{raws: []string{memoryRaw}})
	sf.peek = func(context.Context) (live.Upcoming, bool, error) { return live.Upcoming{}, false, nil }

	_, ok := sf.dr.prepare(ctx, live.ClipStationID, testStation)
	require.True(t, ok)

	got, err := sf.dr.d.TalkMem.Recent(ctx, time.Time{}, 8)
	require.NoError(t, err)
	require.Empty(t, got)
}

// Memory enriches; it never gates. A write failure must not lose the break —
// it has already been rendered and paid for.
func TestPrepareSurvivesShowMemoryWriteFailure(t *testing.T) {
	sf := newSeamFixture(t, &seqModel{raws: []string{memoryRaw}})
	sf.peek = func(context.Context) (live.Upcoming, bool, error) { return live.Upcoming{}, false, nil }
	sf.dr.d.TalkMem = failingTalkMem{}

	_, ok := sf.dr.prepare(context.Background(), live.ClipSeam, testStation)
	require.True(t, ok)
}

func TestPrepareStampsCorrelationAndTitles(t *testing.T) {
	// The correlation id is what later joins the aired segment to the LLM
	// call that scripted it. It is minted here and nowhere else, so if this
	// silently returns "", every provenance join in the console misses.
	sf := newSeamFixture(t, &seqModel{raws: []string{goodRaw}})
	sf.peek = func(context.Context) (live.Upcoming, bool, error) {
		return live.Upcoming{
			Track: library.Track{YTID: "yt2", Title: "Chạy Ngay Đi", Channel: "Sơn Tùng M-TP"},
		}, true, nil
	}

	clip, ok := sf.dr.prepare(context.Background(), live.ClipSeam, testStation)
	require.True(t, ok)
	require.NotEmpty(t, clip.CorrelationID, "a seam clip must carry a correlation id")
	require.Equal(t, "Bài vừa xong", clip.BacksellTitle,
		"the backsell names the track that just played")
	require.Equal(t, "Chạy Ngay Đi", clip.PromiseTitle,
		"the promise names the track she was allowed to announce")
}

func TestPrepareStationIDCarriesNoCorrelation(t *testing.T) {
	// A station ID reads a pre-written line — no LLM call is made, so an id
	// here would join to nothing and imply provenance that does not exist.
	f := newPrepFixture(t, &seqModel{})
	clip, ok := f.dr.prepare(context.Background(), live.ClipStationID, testStation)
	require.True(t, ok)
	require.Empty(t, clip.CorrelationID)
	require.Empty(t, clip.BacksellTitle)
}

type failingTalkMem struct{}

func (failingTalkMem) Append(context.Context, talkmem.Entry) error { return errors.New("db down") }
func (failingTalkMem) Recent(context.Context, time.Time, int) ([]talkmem.Entry, error) {
	return nil, errors.New("db down")
}
