package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	radiolabv1 "github.com/the-algovn/protos/gen/go/algovn/radiolab/v1"
	"github.com/the-algovn/radio-service/internal/artifact"
	"github.com/the-algovn/radio-service/internal/brain"
	"github.com/the-algovn/radio-service/internal/ingest"
	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/persona"
	"github.com/the-algovn/radio-service/internal/spend"
	"github.com/the-algovn/radio-service/internal/voice"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGetLedger(t *testing.T) {
	led := spend.NewMemLedger()
	require.NoError(t, led.Append(context.Background(), spend.Line{TS: time.Now(), Kind: "tts", Provider: "google", Label: "x", Chars: 10, CostUSD: 0.0003}))
	s := New(Deps{Ledger: led})
	resp, err := s.GetLedger(context.Background(), &radiolabv1.GetLedgerRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetLines(), 1)
	require.InDelta(t, 0.0003, resp.GetTotalUsd(), 1e-9)
	require.Equal(t, int32(10), resp.GetLines()[0].GetChars())
}

func TestSynthesizeVoiceFakeSavesTakeAndLedgerLine(t *testing.T) {
	led := spend.NewMemLedger()
	store := artifact.NewFakeStore()
	s := New(Deps{Ledger: led, Store: store, Voice: voice.Fake{}, VoiceFake: true})
	resp, err := s.SynthesizeVoice(context.Background(), &radiolabv1.SynthesizeVoiceRequest{Text: "xin chào", VoiceId: "fake", Label: "t1"})
	require.NoError(t, err)
	require.True(t, resp.GetFake())
	require.Equal(t, "take", resp.GetArtifact().GetKind())
	require.NotEmpty(t, resp.GetArtifact().GetUrl())
	lines, _ := led.All(context.Background())
	require.Len(t, lines, 1)
	require.Equal(t, 0.0, lines[0].CostUSD)
}

func TestGenerateScriptFakeValidatesAndLedgers(t *testing.T) {
	dir := t.TempDir()
	led := spend.NewMemLedger()
	s := New(Deps{
		Ledger: led, PersonaDir: dir,
		Models: map[string]brain.Model{"fake": brain.Fake{}}, DefaultModel: "fake",
	})
	require.NoError(t, persona.Save(dir, "# test persona"))
	resp, err := s.GenerateScript(context.Background(), &radiolabv1.GenerateScriptRequest{
		Brief: &radiolabv1.Brief{Type: "musing", Clock: "hai mươi ba giờ", MaxChars: 500},
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.GetScript())
	require.Empty(t, resp.GetViolations())
	require.True(t, resp.GetFake())
	lines, _ := led.All(context.Background())
	require.Len(t, lines, 1)
	require.Equal(t, "llm", lines[0].Kind)
}

func TestParseCallInFakeModelShortCircuits(t *testing.T) {
	led := spend.NewMemLedger()
	s := New(Deps{
		Ledger: led,
		Models: map[string]brain.Model{"fake": brain.Fake{}}, DefaultModel: "fake",
	})
	resp, err := s.ParseCallIn(context.Background(), &radiolabv1.ParseCallInRequest{
		Text: "cho mình xin bài Em Của Ngày Hôm Qua, tặng Ngọc",
	})
	require.NoError(t, err)
	require.Equal(t, "allow", resp.GetVerdict())
	require.True(t, resp.GetFake())
	require.Equal(t, 0.0, resp.GetCostUsd())
	lines, _ := led.All(context.Background())
	require.Len(t, lines, 1)
	require.Equal(t, "llm", lines[0].Kind)
	require.Equal(t, "fake", lines[0].Provider)
	require.Equal(t, 0.0, lines[0].CostUSD)
}

func TestSavePersonaReadonlyRefuses(t *testing.T) {
	s := New(Deps{PersonaReadonly: true, PersonaDir: t.TempDir()})
	_, err := s.SavePersona(context.Background(), &radiolabv1.SavePersonaRequest{Content: "x"})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestSaveFixtureCanonicalizesCamelCaseAndDropsVolatileFields(t *testing.T) {
	dir := t.TempDir()
	s := New(Deps{FixturesDir: dir})
	// Shape the console actually sends: protojson camelCase + volatile cost/fake.
	camel := `{"songQuery":"Em Của Ngày Hôm Qua","recipient":"Ngọc","message":"chúc ngủ ngon",` +
		`"verdict":"allow","rejectReason":"","digest":"Đức chúc Ngọc ngủ ngon","weight":"warm",` +
		`"costUsd":0.0021,"fake":false}`
	resp, err := s.SaveFixture(context.Background(), &radiolabv1.SaveFixtureRequest{
		Name: "happy-dedication", RawText: "raw text here", ExpectedJson: camel,
	})
	require.NoError(t, err)

	b, err := os.ReadFile(resp.GetPath())
	require.NoError(t, err)
	var doc struct {
		Expected map[string]any `json:"expected"`
	}
	require.NoError(t, json.Unmarshal(b, &doc))
	require.Equal(t, "Em Của Ngày Hôm Qua", doc.Expected["song_query"])
	require.Contains(t, doc.Expected, "reject_reason")
	require.Equal(t, "warm", doc.Expected["weight"])
	require.NotContains(t, doc.Expected, "costUsd")
	require.NotContains(t, doc.Expected, "cost_usd")
	require.NotContains(t, doc.Expected, "fake")
}

// fakeIngestBinDir writes hermetic stand-ins for yt-dlp, ffprobe, and ffmpeg
// to a temp dir and returns its path. yt-dlp is wired via ingest.Runner.Bin;
// ffprobe/ffmpeg have no such override (ingest.Probe/Loudnorm call them by
// bare name), so callers must also prepend this dir to PATH.
func fakeIngestBinDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	scripts := map[string]string{
		"yt-dlp": `#!/bin/sh
set -e
url="$1"
shift
outtpl=""
while [ "$#" -gt 0 ]; do
	if [ "$1" = "-o" ]; then
		outtpl="$2"
		shift 2
	else
		shift
	fi
done
ytid="${url##*v=}"
printf 'fake-audio' > "$(dirname "$outtpl")/$ytid.m4a"
`,
		"ffprobe": `#!/bin/sh
echo '{"format":{"duration":"217.5"}}'
`,
		"ffmpeg": `#!/bin/sh
echo '{"input_i" : "-14.20", "input_tp" : "-1.50", "input_lra" : "7.10"}' >&2
`,
	}
	for name, body := range scripts {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755))
	}
	return dir
}

func TestDownloadTrackCacheMissRunsFullPipelineAndAddsToLibrary(t *testing.T) {
	bin := fakeIngestBinDir(t)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	lib := library.NewMemLibrary()
	store := artifact.NewFakeStore()
	s := New(Deps{
		Store: store, Library: lib,
		Ingest: &ingest.Runner{Bin: filepath.Join(bin, "yt-dlp")},
		TmpDir: t.TempDir(),
	})

	resp, err := s.DownloadTrack(context.Background(), &radiolabv1.DownloadTrackRequest{
		YtId: "abc123", Title: "Em Của Ngày Hôm Qua", Channel: "Sơn Tùng M-TP - Topic",
	})
	require.NoError(t, err)
	require.False(t, resp.GetCached())
	require.InDelta(t, 217.5, resp.GetDurationS(), 0.01)
	require.InDelta(t, -14.20, resp.GetInputI(), 0.01)
	require.NotEmpty(t, resp.GetArtifact().GetId())
	require.Equal(t, "track", resp.GetArtifact().GetKind())

	tr, found, err := lib.Get(context.Background(), "abc123")
	require.NoError(t, err)
	require.True(t, found, "DownloadTrack must add the track to the library on a cache miss")
	require.Equal(t, resp.GetArtifact().GetId(), tr.ArtifactID)
	require.Equal(t, "Sơn Tùng M-TP - Topic", tr.Channel)
	require.InDelta(t, 217.5, tr.DurationS, 0.01)
}

func TestDownloadTrackCacheHitSkipsIngestAndReturnsCachedTrue(t *testing.T) {
	bin := fakeIngestBinDir(t)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	lib := library.NewMemLibrary()
	store := artifact.NewFakeStore()
	s := New(Deps{
		Store: store, Library: lib,
		Ingest: &ingest.Runner{Bin: filepath.Join(bin, "yt-dlp")},
		TmpDir: t.TempDir(),
	})

	first, err := s.DownloadTrack(context.Background(), &radiolabv1.DownloadTrackRequest{
		YtId: "abc123", Title: "Em Của Ngày Hôm Qua", Channel: "Sơn Tùng M-TP - Topic",
	})
	require.NoError(t, err)
	require.False(t, first.GetCached())

	// Break yt-dlp so a second Ingest.Download call would fail the RPC —
	// proves the cache-hit path never reaches it.
	require.NoError(t, os.WriteFile(filepath.Join(bin, "yt-dlp"), []byte("#!/bin/sh\nexit 1\n"), 0o755))

	second, err := s.DownloadTrack(context.Background(), &radiolabv1.DownloadTrackRequest{YtId: "abc123"})
	require.NoError(t, err)
	require.True(t, second.GetCached())
	require.Equal(t, first.GetArtifact().GetId(), second.GetArtifact().GetId())
	require.InDelta(t, first.GetDurationS(), second.GetDurationS(), 0.001)
	require.InDelta(t, first.GetInputI(), second.GetInputI(), 0.001)
	require.InDelta(t, first.GetInputTp(), second.GetInputTp(), 0.001)
	require.NotEmpty(t, second.GetArtifact().GetUrl())
}

func TestListTracksFiltersByQuery(t *testing.T) {
	lib := library.NewMemLibrary()
	ctx := context.Background()
	require.NoError(t, lib.Add(ctx, library.Track{YTID: "a", Title: "Em Của Ngày Hôm Qua", Channel: "Sơn Tùng M-TP - Topic", DurationS: 217, ArtifactID: "track-1"}))
	require.NoError(t, lib.Add(ctx, library.Track{YTID: "b", Title: "Lạc Trôi", Channel: "Sơn Tùng M-TP - Topic", DurationS: 240, ArtifactID: "track-2"}))
	s := New(Deps{Library: lib})

	resp, err := s.ListTracks(ctx, &radiolabv1.ListTracksRequest{Query: "lạc"})
	require.NoError(t, err)
	require.Len(t, resp.GetTracks(), 1)
	require.Equal(t, "b", resp.GetTracks()[0].GetYtId())
	require.Equal(t, "track-2", resp.GetTracks()[0].GetArtifactId())
}

func TestDeleteTrackRemovesRowAndBestEffortDeletesBlob(t *testing.T) {
	lib := library.NewMemLibrary()
	store := artifact.NewFakeStore()
	ctx := context.Background()
	a, err := store.Save(ctx, "track", "m4a", "Lạc Trôi", []byte("audio"), nil)
	require.NoError(t, err)
	require.NoError(t, lib.Add(ctx, library.Track{YTID: "b", Title: "Lạc Trôi", ArtifactID: a.ID}))
	s := New(Deps{Library: lib, Store: store})

	_, err = s.DeleteTrack(ctx, &radiolabv1.DeleteTrackRequest{YtId: "b"})
	require.NoError(t, err)

	_, found, err := lib.Get(ctx, "b")
	require.NoError(t, err)
	require.False(t, found)
	_, err = store.Get(ctx, a.ID)
	require.Error(t, err) // blob deleted too
}

func TestDeleteTrackNotFound(t *testing.T) {
	s := New(Deps{Library: library.NewMemLibrary()})
	_, err := s.DeleteTrack(context.Background(), &radiolabv1.DeleteTrackRequest{YtId: "missing"})
	require.Equal(t, codes.NotFound, status.Code(err))
}
