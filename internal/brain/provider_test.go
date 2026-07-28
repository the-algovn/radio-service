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

// CostUSD feeds the programmer's daily budget gate, so under-pricing is the
// dangerous direction: too low a number means `spent >= BudgetUSD` never trips.
// Before this table existed, every claude* model priced at haiku's $1/$5 — so a
// single ANTHROPIC_MODEL env change to opus would have under-reported cost 5x
// and let 5x the intended spend through the cap.
func TestCostUSDIsModelSpecificAndNeverUnderprices(t *testing.T) {
	const million = 1_000_000
	oneMIn := brain.Usage{In: million}
	oneMOut := brain.Usage{Out: million}

	for _, tc := range []struct{ model string; in, out float64 }{
		{"claude-haiku-4-5-20251001", 1.00, 5.00},
		{"claude-sonnet-5", 3.00, 15.00},
		{"claude-opus-5", 5.00, 25.00},
		{"claude-fable-5", 10.00, 50.00},
		{"gemini-2.5-flash", 0.30, 2.50},
	} {
		t.Run(tc.model, func(t *testing.T) {
			require.InDelta(t, tc.in, brain.CostUSD(tc.model, oneMIn), 1e-9)
			require.InDelta(t, tc.out, brain.CostUSD(tc.model, oneMOut), 1e-9)
		})
	}

	t.Run("fake is free", func(t *testing.T) {
		require.Zero(t, brain.CostUSD("fake", brain.Usage{In: million, Out: million}))
	})

	// The critical property: an unrecognised model must NOT price at zero (which
	// would disable the budget cap outright) and must not price below the most
	// expensive tier we know about.
	t.Run("unknown model prices pessimistically, never zero", func(t *testing.T) {
		unknown := brain.CostUSD("claude-something-unreleased", oneMIn)
		require.Positive(t, unknown, "an unknown model priced at 0 would disable the budget cap")
		require.GreaterOrEqual(t, unknown, brain.CostUSD("claude-fable-5", oneMIn),
			"unknown must price at least as high as the most expensive known model")
	})
}
