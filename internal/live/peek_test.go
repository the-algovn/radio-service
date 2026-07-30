package live

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/schedule"
)

func TestPeekNextCommittedShuffle(t *testing.T) {
	ctx := context.Background()
	sched, reqs, lib := schedule.NewMemStore(), request.NewMemStore(), library.NewMemLibrary()
	require.NoError(t, lib.Add(ctx, library.Track{YTID: "yt1", Title: "T", Channel: "C"}))
	require.NoError(t, sched.SetNextUp(ctx, schedule.NextUp{YTID: "yt1", Title: "T", Channel: "C"}))

	up, found, err := PeekNext(ctx, sched, reqs, lib)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "yt1", up.Track.YTID)
	require.True(t, up.Committed, "an existing commitment is already binding")
	require.Equal(t, "", up.RequestID)
}

func TestPeekNextCommittedRequestCarriesProvenance(t *testing.T) {
	ctx := context.Background()
	sched, reqs, lib := schedule.NewMemStore(), request.NewMemStore(), library.NewMemLibrary()
	require.NoError(t, lib.Add(ctx, library.Track{YTID: "yt2", Title: "T", Channel: "C"}))
	req, err := reqs.Create(ctx, request.Item{Source: request.SourceListener,
		DisplayName: "Ngọc", YTID: "yt2", Reason: "vì trời mưa", Status: request.StatusReady})
	require.NoError(t, err)
	require.NoError(t, sched.SetNextUp(ctx, schedule.NextUp{
		YTID: "yt2", Title: "T", Channel: "C", RequestID: req.ID}))

	up, found, err := PeekNext(ctx, sched, reqs, lib)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, up.Committed)
	require.Equal(t, req.ID, up.RequestID)
	require.Equal(t, request.SourceListener, up.Source)
	require.Equal(t, "Ngọc", up.RequestedByName)
	require.Equal(t, "vì trời mưa", up.Reason)
}

func TestPeekNextHeadOfReadyQueue(t *testing.T) {
	ctx := context.Background()
	sched, reqs, lib := schedule.NewMemStore(), request.NewMemStore(), library.NewMemLibrary()
	require.NoError(t, lib.Add(ctx, library.Track{YTID: "yt3", Title: "T", Channel: "C"}))
	req, err := reqs.Create(ctx, request.Item{Source: request.SourceAI,
		DisplayName: "Ngọc", YTID: "yt3", Reason: "hợp với đêm nay", Status: request.StatusReady})
	require.NoError(t, err)

	up, found, err := PeekNext(ctx, sched, reqs, lib)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, req.ID, up.RequestID)
	require.False(t, up.Committed, "nothing is committed yet — this needs a pin")
	require.Equal(t, request.SourceAI, up.Source)
	require.Equal(t, "Ngọc", up.RequestedByName)
	require.Equal(t, "hợp với đêm nay", up.Reason)
}

func TestPeekNextNothingKnowable(t *testing.T) {
	ctx := context.Background()
	sched, reqs, lib := schedule.NewMemStore(), request.NewMemStore(), library.NewMemLibrary()
	require.NoError(t, lib.Add(ctx, library.Track{YTID: "yt4", Title: "T", Channel: "C"}))

	// Nothing committed and nothing READY (an approved-but-downloading request
	// does not count) — planNext will roll a lazy shuffle at the boundary,
	// which is unknowable ahead of time.
	_, err := reqs.Create(ctx, request.Item{Source: request.SourceListener,
		YTID: "yt4", Status: request.StatusApproved})
	require.NoError(t, err)

	_, found, err := PeekNext(ctx, sched, reqs, lib)
	require.NoError(t, err)
	require.False(t, found)
}

func TestPeekNextCommitmentWhoseTrackVanished(t *testing.T) {
	ctx := context.Background()
	sched, reqs, lib := schedule.NewMemStore(), request.NewMemStore(), library.NewMemLibrary()
	require.NoError(t, sched.SetNextUp(ctx, schedule.NextUp{YTID: "gone", Title: "T"}))

	_, found, err := PeekNext(ctx, sched, reqs, lib)
	require.NoError(t, err)
	require.False(t, found, "promise nothing when the commitment is unusable")
}

// PARITY: PeekNext must choose what planNext chooses. These two are separate
// code paths over the same stores, and a drift between them is a DJ who names
// one song while another plays — so this is a CI gate, not a nicety.
func TestPeekNextMatchesPlanNext(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		setup func(t *testing.T, sched schedule.Store, reqs request.Store, lib library.Library)
	}{
		{"committed shuffle", func(t *testing.T, sched schedule.Store, reqs request.Store, lib library.Library) {
			require.NoError(t, lib.Add(ctx, library.Track{YTID: "a", Title: "A", Channel: "C"}))
			require.NoError(t, sched.SetNextUp(ctx, schedule.NextUp{YTID: "a", Title: "A", Channel: "C"}))
		}},
		{"committed request", func(t *testing.T, sched schedule.Store, reqs request.Store, lib library.Library) {
			require.NoError(t, lib.Add(ctx, library.Track{YTID: "b", Title: "B", Channel: "C"}))
			r, err := reqs.Create(ctx, request.Item{Source: request.SourceListener,
				DisplayName: "Ngọc", YTID: "b", Reason: "vì trời mưa", Status: request.StatusReady})
			require.NoError(t, err)
			require.NoError(t, sched.SetNextUp(ctx, schedule.NextUp{
				YTID: "b", Title: "B", Channel: "C", RequestID: r.ID}))
		}},
		{"listener request outranks an older AI pick", func(t *testing.T, sched schedule.Store, reqs request.Store, lib library.Library) {
			require.NoError(t, lib.Add(ctx, library.Track{YTID: "ai", Title: "AI", Channel: "C"}))
			require.NoError(t, lib.Add(ctx, library.Track{YTID: "li", Title: "LI", Channel: "C"}))
			_, err := reqs.Create(ctx, request.Item{Source: request.SourceAI,
				YTID: "ai", Status: request.StatusReady})
			require.NoError(t, err)
			_, err = reqs.Create(ctx, request.Item{Source: request.SourceListener,
				YTID: "li", Status: request.StatusReady})
			require.NoError(t, err)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// newFixture with no yt_ids: empty library, station on air. Each
			// case seeds the tracks it needs. Sched must be injected through
			// the opts hook — newTestFeeder builds its own internally
			// (feeder_test.go:337) and a test cannot reach that one.
			store, lib, reqs := newFixture(t)
			enc, prod, clk := &fakeEncoder{}, &fakeProducer{}, newFakeClock()
			sched := schedule.NewMemStore()
			f := newTestFeederWith(store, lib, reqs, enc, prod, clk, t.TempDir(),
				func(d *FeederDeps) { d.Sched = sched })
			tc.setup(t, sched, reqs, lib)

			up, found, err := PeekNext(ctx, sched, reqs, lib)
			require.NoError(t, err)
			require.True(t, found)

			p, err := f.planNext(ctx)
			require.NoError(t, err)
			require.Equal(t, p.item.track.YTID, up.Track.YTID,
				"PeekNext and planNext disagree — the DJ would name the wrong song")
			require.Equal(t, p.item.requestID, up.RequestID)
			require.Equal(t, p.item.source, up.Source)
			require.Equal(t, p.item.requestedByName, up.RequestedByName)
			require.Equal(t, p.item.reason, up.Reason)
		})
	}
}
