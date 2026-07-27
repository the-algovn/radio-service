package brain_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/the-algovn/radio-service/internal/brain"
)

// Construction must not dial the network, and must report the identity the
// audit callback and console rely on. No Generate call here — that needs a key.
func TestProviderConstructionReportsIdentity(t *testing.T) {
	ctx := context.Background()

	c, err := brain.NewClaude(ctx, "test-key", "claude-haiku-4-5-20251001")
	require.NoError(t, err)
	require.Equal(t, "claude-haiku-4-5-20251001", c.Name())
	require.Equal(t, "anthropic", c.Provider())

	g, err := brain.NewGemini(ctx, "test-key", "gemini-2.5-flash")
	require.NoError(t, err)
	require.Equal(t, "gemini-2.5-flash", g.Name())
	require.Equal(t, "gemini", g.Provider())
}

// CostUSD keys off the model id, so the ids the constructors report must keep
// matching its prefix switch.
func TestCostUSDStillMatchesProviderModelIDs(t *testing.T) {
	require.Greater(t, brain.CostUSD("claude-haiku-4-5-20251001", brain.Usage{In: 1000, Out: 1000}), 0.0)
	require.Greater(t, brain.CostUSD("gemini-2.5-flash", brain.Usage{In: 1000, Out: 1000}), 0.0)
}
