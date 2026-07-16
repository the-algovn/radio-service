package voice

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFakeProducesPlayableWav(t *testing.T) {
	data, ext, err := Fake{}.Synthesize(context.Background(), "xin chào", "any", 1.0)
	require.NoError(t, err)
	require.Equal(t, "wav", ext)
	require.Equal(t, "RIFF", string(data[:4]))
	require.Equal(t, "WAVE", string(data[8:12]))
	require.Greater(t, len(data), 44)
}

func TestCostUSD(t *testing.T) {
	require.InDelta(t, 30.0/1e6*1000, CostUSD("vi-VN-Chirp3-HD-Aoede", 1000), 1e-9)
	require.InDelta(t, 16.0/1e6*1000, CostUSD("vi-VN-Neural2-A", 1000), 1e-9)
	require.Equal(t, 0.0, CostUSD("fake", 1000))
}
