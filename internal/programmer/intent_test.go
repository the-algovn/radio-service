package programmer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseIntent(t *testing.T) {
	t.Run("full intent", func(t *testing.T) {
		in, err := ParseIntent(`{"searches":["lofi guitar","piano mưa"],"library_query":"acoustic","respins":["abc123"],"note":"đêm dịu"}`)
		require.NoError(t, err)
		require.Equal(t, []string{"lofi guitar", "piano mưa"}, in.Searches)
		require.Equal(t, "acoustic", in.LibraryQuery)
		require.Equal(t, []string{"abc123"}, in.Respins)
		require.Equal(t, "đêm dịu", in.Note)
		require.False(t, in.empty())
	})

	t.Run("truncates over-long lists", func(t *testing.T) {
		in, err := ParseIntent(`{"searches":["a","b","c","d"],"respins":["1","2","3"]}`)
		require.NoError(t, err)
		require.Len(t, in.Searches, maxSearches)
		require.Len(t, in.Respins, maxRespins)
	})

	t.Run("drops blank entries", func(t *testing.T) {
		in, err := ParseIntent(`{"searches":["","  ","real"],"respins":[""]}`)
		require.NoError(t, err)
		require.Equal(t, []string{"real"}, in.Searches)
		require.Empty(t, in.Respins)
	})

	// An all-empty intent is a legitimate "nothing to add", not an error — the
	// caller routes it to the deterministic fallback.
	t.Run("empty intent is valid but reports empty", func(t *testing.T) {
		in, err := ParseIntent(`{"searches":[],"library_query":"","respins":[],"note":"im lặng"}`)
		require.NoError(t, err)
		require.True(t, in.empty())
	})

	t.Run("non-JSON is an error", func(t *testing.T) {
		_, err := ParseIntent(`not json`)
		require.Error(t, err)
	})
}

// The brief is untrusted data and must be delimited as such.
func TestBuildIntentPromptsDelimitsBrief(t *testing.T) {
	system, user := BuildIntentPrompts("PERSONA", `{"local_time":"Monday 21:00"}`)
	require.Contains(t, system, "PERSONA")
	require.Contains(t, user, "<brief>")
	require.Contains(t, user, "</brief>")
	require.Contains(t, user, `"local_time"`)
	require.NotContains(t, system, "reason", "phase 1 must not ask for reasons")
}
