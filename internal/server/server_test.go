package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	radiolabv1 "github.com/the-algovn/protos/gen/go/algovn/radiolab/v1"
	"github.com/the-algovn/radio-service/internal/artifact"
	"github.com/the-algovn/radio-service/internal/brain"
	"github.com/the-algovn/radio-service/internal/persona"
	"github.com/the-algovn/radio-service/internal/spend"
	"github.com/the-algovn/radio-service/internal/voice"
)

func TestGetLedger(t *testing.T) {
	led := spend.NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"))
	require.NoError(t, led.Append(spend.Line{TS: time.Now(), Kind: "tts", Provider: "google", Label: "x", Chars: 10, CostUSD: 0.0003}))
	s := New(Deps{Ledger: led})
	resp, err := s.GetLedger(context.Background(), &radiolabv1.GetLedgerRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetLines(), 1)
	require.InDelta(t, 0.0003, resp.GetTotalUsd(), 1e-9)
	require.Equal(t, int32(10), resp.GetLines()[0].GetChars())
}

func TestSynthesizeVoiceFakeSavesTakeAndLedgerLine(t *testing.T) {
	dir := t.TempDir()
	led := spend.NewLedger(filepath.Join(dir, "ledger.jsonl"))
	store := &artifact.Store{Dir: filepath.Join(dir, "artifacts")}
	s := New(Deps{Ledger: led, Store: store, Voice: voice.Fake{}, VoiceFake: true})
	resp, err := s.SynthesizeVoice(context.Background(), &radiolabv1.SynthesizeVoiceRequest{Text: "xin chào", VoiceId: "fake", Label: "t1"})
	require.NoError(t, err)
	require.True(t, resp.GetFake())
	require.Equal(t, "take", resp.GetArtifact().GetKind())
	lines, _ := led.All()
	require.Len(t, lines, 1)
	require.Equal(t, 0.0, lines[0].CostUSD)
}

func TestGenerateScriptFakeValidatesAndLedgers(t *testing.T) {
	dir := t.TempDir()
	led := spend.NewLedger(filepath.Join(dir, "ledger.jsonl"))
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
	lines, _ := led.All()
	require.Len(t, lines, 1)
	require.Equal(t, "llm", lines[0].Kind)
}
