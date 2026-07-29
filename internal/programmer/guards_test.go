package programmer

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/the-algovn/radio-service/internal/ingest"
	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/schedule"
	"github.com/the-algovn/radio-service/internal/songkey"
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

// errRequests wraps a real request.Store and fails only HasPendingYTID —
// mirrors errCountLibrary in programmer_test.go, the established pattern for
// making one store call fail without a parallel fake.
type errRequests struct{ request.Store }

func (errRequests) HasPendingYTID(context.Context, string) (bool, error) {
	return false, errors.New("pending read boom")
}

// The guard read itself failing must not be mistaken for its own answer: a
// Postgres outage has to render in the funnel histogram as "read-failed", not
// "queued" — the one label the histogram most needs to get right.
func TestClassifyReturnsReadFailedNotQueuedOnGuardError(t *testing.T) {
	h := newHarness(t)
	h.prog.d.Requests = errRequests{h.requests}
	g, err := h.prog.buildGuards(h.ctx)
	require.NoError(t, err)

	got, err := h.prog.classify(h.ctx, factsOf{YTID: "x", DurationS: 200, DurationKnown: true}, g)
	require.Error(t, err)
	require.Equal(t, dropReadFailed, got)
}

// The committed next-up track is about to play. Queueing it again is the
// highest-frequency real duplication in the system, and no guard saw it.
func TestClassifyRejectsNextUp(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.sched.SetNextUp(h.ctx, schedule.NextUp{
		YTID: "soon", Title: "Soon", Channel: "C",
	}))

	g, err := h.prog.buildGuards(h.ctx)
	require.NoError(t, err)

	got, err := h.prog.classify(h.ctx, factsOf{YTID: "soon", DurationS: 200, DurationKnown: true}, g)
	require.NoError(t, err)
	require.Equal(t, dropNextUp, got)
}

// No next-up is committed (g.nextUpID == "") and the candidate has no id
// either. Comparing the two bare strings would match and count a phantom
// "next-up" drop in the funnel for a candidate that has nothing to do with
// the schedule.
func TestClassifyIgnoresEmptyYTIDWhenNoNextUpCommitted(t *testing.T) {
	h := newHarness(t)

	g, err := h.prog.buildGuards(h.ctx)
	require.NoError(t, err)
	require.Empty(t, g.nextUpID)

	got, err := h.prog.classify(h.ctx, factsOf{YTID: "", DurationS: 200, DurationKnown: true}, g)
	require.NoError(t, err)
	require.Equal(t, dropNone, got)
}

// A failed request is invisible to HasPendingYTID, so without this the
// programmer re-picks a geo-blocked upload every single tick.
func TestClassifyRejectsRecentlyFailed(t *testing.T) {
	h := newHarness(t)
	it, err := h.requests.Create(h.ctx, request.Item{
		Source: request.SourceAI, YTID: "blocked", Title: "B",
		DurationS: 200, Status: request.StatusApproved,
	})
	require.NoError(t, err)
	require.NoError(t, h.requests.MarkFailed(h.ctx, it.ID, "geo"))

	g, err := h.prog.buildGuards(h.ctx)
	require.NoError(t, err)

	got, err := h.prog.classify(h.ctx, factsOf{YTID: "blocked", DurationS: 200, DurationKnown: true}, g)
	require.NoError(t, err)
	require.Equal(t, dropFailed, got)
}

// An aired request must NOT be treated as failed — recency already governs it,
// with its own window.
func TestClassifyIgnoresAiredTerminalRequests(t *testing.T) {
	h := newHarness(t)
	it, err := h.requests.Create(h.ctx, request.Item{
		Source: request.SourceAI, YTID: "played", Title: "P",
		DurationS: 200, Status: request.StatusReady,
	})
	require.NoError(t, err)
	require.NoError(t, h.requests.MarkAired(h.ctx, it.ID, h.clock.Now()))

	g, err := h.prog.buildGuards(h.ctx)
	require.NoError(t, err)
	require.False(t, g.failed["played"])
}

// The duplication hole the whole stage exists for: the same song under a second
// yt_id passes every id-keyed guard.
func TestClassifyRejectsTheSameSongUnderADifferentID(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.lib.Add(h.ctx, library.Track{
		YTID: "aired-id", Title: "Nắng Ấm Xa Dần", Channel: "Sơn Tùng M-TP",
		DurationS: 200, SongKey: songkey.Of("Sơn Tùng M-TP", "Nắng Ấm Xa Dần"),
	}))
	require.NoError(t, h.airlog.Append(h.ctx, live.Entry{
		YTID: "aired-id", Title: "Nắng Ấm Xa Dần", DurationS: 200,
	}))

	g, err := h.prog.buildGuards(h.ctx)
	require.NoError(t, err)

	got, err := h.prog.classify(h.ctx, factsOf{
		YTID: "other-upload", DurationS: 205, DurationKnown: true,
		SongKey: songkey.Of("Sơn Tùng M-TP", "Nắng Ấm Xa Dần (Official MV)"),
	}, g)
	require.NoError(t, err)
	require.Equal(t, dropRecentSong, got)
}

// An empty song key is the "not computed" sentinel and must never match.
func TestClassifyIgnoresEmptySongKey(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.lib.Add(h.ctx, library.Track{
		YTID: "a", Title: "A", DurationS: 200, SongKey: "",
	}))
	require.NoError(t, h.airlog.Append(h.ctx, live.Entry{YTID: "a", Title: "A", DurationS: 200}))

	g, err := h.prog.buildGuards(h.ctx)
	require.NoError(t, err)

	got, err := h.prog.classify(h.ctx, factsOf{
		YTID: "b", DurationS: 200, DurationKnown: true, SongKey: "",
	}, g)
	require.NoError(t, err)
	require.Equal(t, dropNone, got, "the empty sentinel must not collapse everything into one song")
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

// The regression this guard exists for: a search result carries no
// artist/track metadata, only a channel name and a raw video title.
// songkey.Of on that pair mis-keys "Artist - Title" uploads onto the artist,
// so these two unrelated Đen Vâu songs would fold to the same identity —
// "den-vau-official/den" — and once one aired, dropRecentSong would reject
// the other as a duplicate for the whole recency window. factsFrom must
// leave SongKey "" so classify never sees that false identity.
func TestFactsFromLeavesSongKeyEmpty(t *testing.T) {
	f1 := factsFrom(ingest.Candidate{
		YTID: "a", Channel: "Đen Vâu Official", Title: "Đen - Trốn Tìm ft. MTV Band (M/V)",
	})
	f2 := factsFrom(ingest.Candidate{
		YTID: "b", Channel: "Đen Vâu Official", Title: "Đen - Mang Tiền Về Cho Mẹ ft. Nguyên Thảo (M/V)",
	})
	require.Empty(t, f1.SongKey)
	require.Empty(t, f2.SongKey)
}
