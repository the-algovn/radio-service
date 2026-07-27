package director

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/eino-contrib/jsonschema"
	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/persona"
	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/spend"
	"github.com/the-algovn/radio-service/internal/station"
	"github.com/the-algovn/radio-service/internal/voice"
)

// seqModel returns scripted raw outputs in sequence.
type seqModel struct {
	raws  []string
	calls int
}

func (m *seqModel) Name() string     { return "claude-test" }
func (m *seqModel) Provider() string { return "fake" }
func (m *seqModel) Generate(context.Context, string, string, *jsonschema.Schema) (string, error) {
	raw := m.raws[min(m.calls, len(m.raws)-1)]
	m.calls++
	return raw, nil
}

type errVoice struct{}

func (errVoice) Synthesize(context.Context, string, string, float64) ([]byte, string, error) {
	return nil, "", errors.New("tts down")
}

const goodRaw = `{"script":"Vừa rồi là một bản nhạc đêm dịu dàng, cảm ơn bạn đã cùng nghe.","summary":"backsell đêm","used_phrases":["cùng nghe"]}`
const digitRaw = `{"script":"Bài này ra năm 2020 đó nha.","summary":"x","used_phrases":[]}`

// testDJ mirrors the old fixture Deps knobs for direct prepare() calls.
var testDJ = station.DJSettings{VoiceID: "fake", Rate: 1.0, BreakEvery: 2, StationIDMin: 60, MaxChars: 450}

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
	reqs   *request.MemStore
	model  *seqModel
}

func newPrepFixture(t *testing.T, model *seqModel) *prepFixture {
	t.Helper()
	clk := newDirClock()
	ledger := spend.NewMemLedger()
	airLog := live.NewMemAirLog()
	reqs := request.NewMemStore()
	personaDir := t.TempDir()
	require.NoError(t, persona.Save(personaDir, "# Tiểu Dương Dương\nGiọng ấm."))
	store := station.NewMemStore()
	_, err := store.UpdateDJSettings(context.Background(), station.DJSettings{
		VoiceID: "fake", Rate: 1.0, BreakEvery: 2, StationIDMin: 60, MaxChars: 450})
	require.NoError(t, err)
	dr := New(Deps{
		Model: model, Voice: voice.Fake{}, VoiceFake: true, Ledger: ledger,
		Station: store, Listeners: live.NewMemListeners(time.Now),
		AirLog: airLog, Requests: reqs,
		PersonaDir: personaDir, StationIDsPath: writeIDs(t, "đài thân mến\n"),
		DataDir: t.TempDir(), BudgetUSD: 1.0,
		Render: fakeRender, Clock: clk, Location: time.UTC,
	})
	return &prepFixture{dr: dr, clk: clk, ledger: ledger, log: airLog, reqs: reqs, model: model}
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

func TestPrepareBacksellHappyPath(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	anchor := live.Entry{YTID: "a", Title: "Bài A", Artist: "Ca sĩ",
		StartedAt: time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC), Source: "ai", Reason: "hợp đêm"}
	require.NoError(t, f.log.Append(context.Background(), anchor))

	clip, ok := f.dr.prepare(context.Background(), live.ClipBacksell, testDJ)
	require.True(t, ok)
	require.Equal(t, live.ClipBacksell, clip.Kind)
	require.Equal(t, "a", clip.AnchorYTID)
	require.Equal(t, anchor.StartedAt, clip.AnchorStartedAt)
	require.InDelta(t, 0.5, clip.DurationS, 0.001)
	_, err := os.Stat(clip.Path)
	require.NoError(t, err, "rendered clip file exists")
	require.Equal(t, []string{"tts:director:backsell"}, ledgerLabels(t, f.ledger), "LLM spend is now priced by the Eino callback, not the director")
	lines, _ := f.ledger.All(context.Background())
	require.Zero(t, lines[0].CostUSD, "VoiceFake zeroes tts cost")
	f.dr.mu.Lock()
	require.Len(t, f.dr.ring, 1, "backsell summary recorded")
	f.dr.mu.Unlock()
}

func TestPrepareBacksellRetriesOnceOnViolations(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{digitRaw, goodRaw}})
	require.NoError(t, f.log.Append(context.Background(), live.Entry{YTID: "a", Title: "A", StartedAt: time.Now()}))
	_, ok := f.dr.prepare(context.Background(), live.ClipBacksell, testDJ)
	require.True(t, ok)
	require.Equal(t, 2, f.model.calls)
	require.Equal(t, []string{"tts:director:backsell"}, ledgerLabels(t, f.ledger),
		"no llm line: the director no longer prices either attempt itself")
}

func TestPrepareBacksellAbortsAfterSecondViolation(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{digitRaw, digitRaw}})
	require.NoError(t, f.log.Append(context.Background(), live.Entry{YTID: "a", Title: "A", StartedAt: time.Now()}))
	_, ok := f.dr.prepare(context.Background(), live.ClipBacksell, testDJ)
	require.False(t, ok)
	require.Equal(t, 2, f.model.calls)
	require.Empty(t, ledgerLabels(t, f.ledger), "no llm spend recorded; the director no longer prices its own calls")
	f.dr.mu.Lock()
	require.Empty(t, f.dr.ring)
	f.dr.mu.Unlock()
}

func TestPrepareBacksellNoAirLogEntryQuietSkip(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	_, ok := f.dr.prepare(context.Background(), live.ClipBacksell, testDJ)
	require.False(t, ok, "nothing airing → nothing to talk about")
	require.Zero(t, f.model.calls)
}

func TestPrepareStationIDSkipsLLM(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	clip, ok := f.dr.prepare(context.Background(), live.ClipStationID, testDJ)
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
	_, ok := f.dr.prepare(context.Background(), live.ClipBacksell, testDJ)
	require.False(t, ok)
	require.Empty(t, ledgerLabels(t, f.ledger), "no llm line: the director no longer prices the call; tts never ran")
}

func TestPrepareRenderFailureCleansUp(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	f.dr.d.Render = func(_ context.Context, _, _ string) (float64, error) { return 0, errors.New("boom") }
	require.NoError(t, f.log.Append(context.Background(), live.Entry{YTID: "a", Title: "A", StartedAt: time.Now()}))
	_, ok := f.dr.prepare(context.Background(), live.ClipBacksell, testDJ)
	require.False(t, ok)
	entries, err := os.ReadDir(f.dr.d.DataDir)
	require.NoError(t, err)
	require.Empty(t, entries, "no temp files left behind")
}

func TestBuildBriefContents(t *testing.T) {
	f := newPrepFixture(t, &seqModel{raws: []string{goodRaw}})
	for _, title := range []string{"C1", "C2", "C3", "C4"} {
		_, err := f.reqs.Create(context.Background(), request.Item{
			Source: request.SourceListener, YTID: "q-" + title, Title: title, Status: request.StatusReady})
		require.NoError(t, err)
	}
	f.dr.pushRing("chuyện mưa", []string{"bạn nghe đài"})
	just := live.Entry{Title: "Bài A", Artist: "Ca sĩ", Source: "listener", RequestedByName: "Minh"}
	b := f.dr.buildBrief(context.Background(), just, 450)
	require.Equal(t, "backsell", b.Type)
	require.Equal(t, "Bài A", b.JustPlayed.Title)
	require.Equal(t, "Minh", b.JustPlayed.RequestedByName)
	require.Len(t, b.QueueTeasers, 3, "teasers capped at 3")
	require.Equal(t, []string{"chuyện mưa"}, b.MemorySummaries)
	require.Equal(t, []string{"bạn nghe đài"}, b.RecentPhrases)
	require.Equal(t, 450, b.MaxChars)
	require.NotEmpty(t, b.Daypart)
}

// recVoice records the last Synthesize arguments.
type recVoice struct {
	mu      sync.Mutex
	voiceID string
	rate    float64
}

func (r *recVoice) Synthesize(_ context.Context, _ string, voiceID string, rate float64) ([]byte, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.voiceID, r.rate = voiceID, rate
	return []byte("mp3"), "mp3", nil
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
