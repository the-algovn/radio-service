package programmer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func pool() []Candidate {
	return []Candidate{
		{YTID: "a1", Title: "A", DurationS: 200, Source: sourceLibrary, Cached: true},
		{YTID: "b2", Title: "B", DurationS: 210, Source: sourceYouTube},
	}
}

func TestParseChoice(t *testing.T) {
	t.Run("valid single pick resolves the whole candidate", func(t *testing.T) {
		got, err := ParseChoice(`{"picks":[{"yt_id":"a1","reason":"đêm dịu"}]}`, pool(), 2)
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, "a1", got[0].Candidate.YTID)
		require.Equal(t, "A", got[0].Candidate.Title, "the resolved candidate travels with the choice")
		require.True(t, got[0].Candidate.Cached)
		require.Equal(t, "đêm dịu", got[0].Reason)
	})

	// Membership is the whole point: a yt_id the pool never offered means the
	// model invented a track, and its reason would describe nothing real.
	t.Run("drops yt_id outside the pool", func(t *testing.T) {
		got, err := ParseChoice(`{"picks":[{"yt_id":"ghost","reason":"x"},{"yt_id":"b2","reason":"y"}]}`, pool(), 2)
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, "b2", got[0].Candidate.YTID)
	})

	t.Run("all picks invalid is an error", func(t *testing.T) {
		_, err := ParseChoice(`{"picks":[{"yt_id":"ghost","reason":"x"}]}`, pool(), 2)
		require.Error(t, err)
	})

	t.Run("empty reason is rejected", func(t *testing.T) {
		_, err := ParseChoice(`{"picks":[{"yt_id":"a1","reason":"  "}]}`, pool(), 2)
		require.Error(t, err, "a pick with no reason gives the DJ nothing to say")
	})

	t.Run("duplicate yt_ids collapse", func(t *testing.T) {
		got, err := ParseChoice(`{"picks":[{"yt_id":"a1","reason":"x"},{"yt_id":"a1","reason":"y"}]}`, pool(), 2)
		require.NoError(t, err)
		require.Len(t, got, 1)
	})

	t.Run("truncates to want", func(t *testing.T) {
		got, err := ParseChoice(`{"picks":[{"yt_id":"a1","reason":"x"},{"yt_id":"b2","reason":"y"}]}`, pool(), 1)
		require.NoError(t, err)
		require.Len(t, got, 1, "an over-eager model must not overfill the queue")
	})

	t.Run("caps the reason length", func(t *testing.T) {
		long := ""
		for i := 0; i < maxReasonRunes+50; i++ {
			long += "ă"
		}
		got, err := ParseChoice(`{"picks":[{"yt_id":"a1","reason":"`+long+`"}]}`, pool(), 2)
		require.NoError(t, err)
		require.Len(t, []rune(got[0].Reason), maxReasonRunes)
	})

	t.Run("non-JSON is an error", func(t *testing.T) {
		_, err := ParseChoice(`nope`, pool(), 2)
		require.Error(t, err)
	})
}

// want is derived from queue depth so a decision fills toward the target
// without overshooting it.
func TestWantPicks(t *testing.T) {
	require.Equal(t, maxPicks, wantPicks(0))
	require.Equal(t, maxPicks, wantPicks(1))
	require.Equal(t, 1, wantPicks(queueDepthTarget-1))
}

func TestBuildChoosePromptsDelimitsData(t *testing.T) {
	system, user := BuildChoosePrompts("PERSONA", `{"local_time":"Monday 21:00"}`, `[{"yt_id":"a1"}]`, 2)
	require.Contains(t, system, "PERSONA")
	require.Contains(t, user, "<brief>")
	require.Contains(t, user, "<candidates>")
	require.Contains(t, user, "</candidates>")
	require.Contains(t, user, "a1")
}

func TestRepairUserNamesTheViolation(t *testing.T) {
	got := RepairUser("original", `{"picks":[]}`, "yt_id ghost is not in <candidates>")
	require.Contains(t, got, "original")
	require.Contains(t, got, "ghost")
}
