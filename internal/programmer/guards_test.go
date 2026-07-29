package programmer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/the-algovn/radio-service/internal/ingest"
	"github.com/the-algovn/radio-service/internal/live"
)

func TestClassifyReasons(t *testing.T) {
	tests := []struct {
		name string
		f    factsOf
		want dropReason
	}{
		{"keeps a normal track",
			factsOf{YTID: "ok", DurationS: 200, DurationKnown: true}, dropNone},
		{"rejects too short",
			factsOf{YTID: "s", DurationS: minTrackSeconds - 1, DurationKnown: true}, dropTooShort},
		{"rejects too long",
			factsOf{YTID: "l", DurationS: maxTrackSeconds + 1, DurationKnown: true}, dropTooLong},
		{"rejects a live stream",
			factsOf{YTID: "v", DurationKnown: false, Live: true}, dropLive},
		{"rejects a short",
			factsOf{YTID: "sh", DurationS: 45, DurationKnown: true, ShortForm: true}, dropShortForm},
		// The regression this whole stage exists for. An unknown duration must
		// NOT be read as zero and rejected as too-short: acquire re-probes with
		// ffprobe and enforces MaxDurationS before the track ever reaches the
		// library, so admitting it costs pool quality, not pool emptiness.
		{"admits unknown duration",
			factsOf{YTID: "u", DurationS: 0, DurationKnown: false}, dropNone},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			g, err := h.prog.buildGuards(h.ctx)
			require.NoError(t, err)

			got, err := h.prog.classify(h.ctx, tc.f, g)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestClassifyRejectsRecentlyAired(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.airlog.Append(h.ctx, live.Entry{YTID: "aired", Title: "A", DurationS: 200}))

	g, err := h.prog.buildGuards(h.ctx)
	require.NoError(t, err)

	got, err := h.prog.classify(h.ctx, factsOf{YTID: "aired", DurationS: 200, DurationKnown: true}, g)
	require.NoError(t, err)
	require.Equal(t, dropRecent, got)
}

// A live stream arrives with an unknown duration. Before this change it was
// rejected by accident, as "too short", with no log line saying so. The reason
// must name what actually disqualified it.
func TestClassifyPrefersLiveOverDurationReason(t *testing.T) {
	h := newHarness(t)
	g, err := h.prog.buildGuards(h.ctx)
	require.NoError(t, err)

	got, err := h.prog.classify(h.ctx, factsOf{YTID: "v", DurationKnown: false, Live: true}, g)
	require.NoError(t, err)
	require.Equal(t, dropLive, got)
}

func TestFactsFromCandidateCarriesEveryFlag(t *testing.T) {
	f := factsFrom(ingest.Candidate{
		YTID: "x", DurationS: 200, DurationKnown: true, Live: true, ShortForm: true,
	})
	require.Equal(t, "x", f.YTID)
	require.Equal(t, int64(200), f.DurationS)
	require.True(t, f.DurationKnown)
	require.True(t, f.Live)
	require.True(t, f.ShortForm)
}
