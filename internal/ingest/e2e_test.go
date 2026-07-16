package ingest

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// Network + binary test: RADIO_LAB_E2E=1 go test ./internal/ingest/ -run E2E -v
func TestE2ESearchDownloadProbe(t *testing.T) {
	if os.Getenv("RADIO_LAB_E2E") == "" {
		t.Skip("set RADIO_LAB_E2E=1")
	}
	r := Runner{}
	cs, err := r.Search(context.Background(), "em của ngày hôm qua sơn tùng", 5)
	require.NoError(t, err)
	require.NotEmpty(t, cs)
	ranked := Rank("em của ngày hôm qua sơn tùng", cs)
	p, err := r.Download(context.Background(), ranked[0].YTID, t.TempDir())
	require.NoError(t, err)
	dur, err := Probe(p)
	require.NoError(t, err)
	require.Greater(t, dur, 60.0)
	i, _, _, err := Loudnorm(p)
	require.NoError(t, err)
	require.Less(t, i, 0.0) // loudness is negative LUFS
}
