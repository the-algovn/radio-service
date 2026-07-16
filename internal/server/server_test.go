package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	radiolabv1 "github.com/the-algovn/protos/gen/go/algovn/radiolab/v1"
	"github.com/the-algovn/radio-service/internal/spend"
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
