package director

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/brain"
	"github.com/the-algovn/radio-service/internal/live"
)

func TestSpecForKnowsEveryKindAndNothingElse(t *testing.T) {
	for _, k := range []string{live.ClipSeam, live.ClipStationID, live.ClipMusing,
		live.ClipDaypartTransition, live.ClipWakeGreeting} {
		_, ok := specFor(k)
		require.True(t, ok, k)
	}
	for _, k := range []string{"", "dj", "backsell", "dedication_read"} {
		_, ok := specFor(k)
		require.False(t, ok, k)
	}
}

// The anchor and the pin belong to the seam ALONE. The other three talk about
// the night, so they need no AirLog.Latest, no peek, and they cannot go stale.
func TestOnlyTheSeamIsAnchoredAndPromises(t *testing.T) {
	for _, k := range []string{live.ClipStationID, live.ClipMusing,
		live.ClipDaypartTransition, live.ClipWakeGreeting} {
		sp, _ := specFor(k)
		require.False(t, sp.Anchored, k)
		require.False(t, sp.Promises, k)
	}
	sp, _ := specFor(live.ClipSeam)
	require.True(t, sp.Anchored)
	require.True(t, sp.Promises)
}

// Generated is what decides whether prepare makes a model call at all, so it
// must agree exactly with the set of kinds the brain has rules for.
func TestGeneratedKindsAreExactlyTheOnesTheBrainWrites(t *testing.T) {
	for _, k := range []string{live.ClipSeam, live.ClipStationID, live.ClipMusing,
		live.ClipDaypartTransition, live.ClipWakeGreeting} {
		sp, _ := specFor(k)
		_, hasRules := brain.RulesFor(k)
		require.Equal(t, sp.Generated, hasRules, k)
	}
}
