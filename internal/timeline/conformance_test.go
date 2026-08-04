package timeline_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/schedule"
	"github.com/the-algovn/radio-service/internal/station"
	"github.com/the-algovn/radio-service/internal/timeline"
)

// TestProjectMatchesPeekNext chains the timeline projector to live.PeekNext.
//
// live.PeekNext is already pinned to planNext by
// internal/live/peek_test.go's TestPeekNextMatchesPlanNext. Chaining to it
// means the projector inherits that guarantee transitively: if planNext
// changes, PeekNext's own test fails first, and this one fails right after.
//
// A pinned request that already AIRED is the branch whose regression is
// invisible downstream — next_up would replay a track back-to-back.
func TestProjectMatchesPeekNext(t *testing.T) {
	var (
		ctx  = context.Background()
		base = time.Date(2026, 8, 4, 21, 0, 0, 0, time.UTC)
	)

	// baseState is set up so the seam arm cannot fire and mask the music choice:
	// no prepared clip, cadence not due (BreakEvery=0, StationIDMin=0), and
	// all gate preconditions pass. Do not "simplify" these zeros — every one is
	// load-bearing.
	baseState := func() timeline.State {
		return timeline.State{
			Now:     base,
			Station: station.Station{OnAir: true, AIEnabled: true, DJ: station.DJSettings{BreakEvery: 0, StationIDMin: 0}},
			Dir:     timeline.DirectorSnapshot{Present: true},
			// SpentUSD=0, BudgetUSD=1 keeps GateOK (not GateBudget).
			// Listeners=1 keeps GateOK (not GateNoListeners).
			Listeners:    1,
			BudgetUSD:    1,
			MedianTrackS: 200,
		}
	}

	cases := []struct {
		name string
		// setupLive builds the PeekNext fixtures: sched, reqs, lib.
		// Returns the track title expected from PeekNext when found=true.
		setupLive func(t *testing.T, sched schedule.Store, reqs request.Store, lib library.Library)
		// setupState builds the projector State from the same scenario data.
		setupState func(t *testing.T) timeline.State
		// wantFound is what PeekNext must report.
		wantFound bool
		// wantCertainty is the projector's certainty when wantFound is true.
		wantCertainty string
	}{
		{
			// 1. Pin set, no RequestID (a shuffle pin).
			name: "shuffle pin",
			setupLive: func(t *testing.T, sched schedule.Store, reqs request.Store, lib library.Library) {
				require.NoError(t, lib.Add(ctx, library.Track{YTID: "yt1", Title: "Shuffle Track", Channel: "Artist 1", DurationS: 200}))
				require.NoError(t, sched.SetNextUp(ctx, schedule.NextUp{YTID: "yt1", Title: "Shuffle Track", Channel: "Artist 1"}))
			},
			setupState: func(t *testing.T) timeline.State {
				s := baseState()
				s.NextUp = &schedule.NextUp{YTID: "yt1", Title: "Shuffle Track", Channel: "Artist 1"}
				s.Durations = map[string]int{"yt1": 200}
				return s
			},
			wantFound:     true,
			wantCertainty: timeline.CertaintyCommitted,
		},
		{
			// 2. Pin set with a RequestID whose request IS ready.
			name: "committed request ready",
			setupLive: func(t *testing.T, sched schedule.Store, reqs request.Store, lib library.Library) {
				require.NoError(t, lib.Add(ctx, library.Track{YTID: "yt2", Title: "Requested Track", Channel: "Artist 2", DurationS: 250}))
				r, err := reqs.Create(ctx, request.Item{
					Source: request.SourceListener, DisplayName: "Ngoc", YTID: "yt2",
					Title: "Requested Track", Channel: "Artist 2", Reason: "vi troi mua", Status: request.StatusReady,
					DurationS: 250,
				})
				require.NoError(t, err)
				require.NoError(t, sched.SetNextUp(ctx, schedule.NextUp{
					YTID: "yt2", Title: "Requested Track", Channel: "Artist 2", RequestID: r.ID,
				}))
			},
			setupState: func(t *testing.T) timeline.State {
				s := baseState()
				s.NextUp = &schedule.NextUp{YTID: "yt2", Title: "Requested Track", Channel: "Artist 2", RequestID: "r2"}
				s.Pending = []request.Item{
					{ID: "r2", YTID: "yt2", Title: "Requested Track", Channel: "Artist 2",
						Source: request.SourceListener, DisplayName: "Ngoc", Reason: "vi troi mua",
						Status: request.StatusReady, DurationS: 250},
				}
				s.Durations = map[string]int{"yt2": 250}
				return s
			},
			wantFound:     true,
			wantCertainty: timeline.CertaintyCommitted,
		},
		{
			// 3. Pin set with a RequestID whose request is NOT ready (already aired).
			//    This is the unusable-commitment branch — the one regression whose
			//    downstream symptom is a track replaying back-to-back.
			name: "committed request already aired",
			setupLive: func(t *testing.T, sched schedule.Store, reqs request.Store, lib library.Library) {
				require.NoError(t, lib.Add(ctx, library.Track{YTID: "yt3", Title: "Aired Track", Channel: "Artist 3", DurationS: 180}))
				r, err := reqs.Create(ctx, request.Item{
					Source: request.SourceListener, DisplayName: "Ngoc", YTID: "yt3",
					Title: "Aired Track", Channel: "Artist 3", Status: request.StatusReady,
				})
				require.NoError(t, err)
				require.NoError(t, reqs.MarkAired(ctx, r.ID, base.Add(-time.Hour)))
				require.NoError(t, sched.SetNextUp(ctx, schedule.NextUp{
					YTID: "yt3", Title: "Aired Track", Channel: "Artist 3", RequestID: r.ID,
				}))
			},
			setupState: func(t *testing.T) timeline.State {
				s := baseState()
				// The pin exists but the request is aired — not in Pending.
				// The projector consumes the pin, finds no ready request, and
				// falls through to KindUnknown when the ready queue is also empty.
				s.NextUp = &schedule.NextUp{YTID: "yt3", Title: "Aired Track", Channel: "Artist 3", RequestID: "r3"}
				s.Durations = map[string]int{"yt3": 180}
				return s
			},
			wantFound: false,
		},
		{
			// 4. Pin whose track is missing from the library.
			name: "pin track missing from library",
			setupLive: func(t *testing.T, sched schedule.Store, reqs request.Store, lib library.Library) {
				// No lib.Add — the track that the pin references is absent.
				require.NoError(t, sched.SetNextUp(ctx, schedule.NextUp{YTID: "gone", Title: "Vanished"}))
			},
			setupState: func(t *testing.T) timeline.State {
				s := baseState()
				// The schedule pin exists. The projector's musicArm does NOT
				// verify library presence — it trusts NextUp blindly, so it
				// would emit KindTrack/Committed while PeekNext returns false.
				// This is the divergence this scenario is designed to catch.
				s.NextUp = &schedule.NextUp{YTID: "gone", Title: "Vanished"}
				return s
			},
			wantFound: false,
		},
		{
			// 5. No pin, ready queue non-empty.
			name: "head of ready queue",
			setupLive: func(t *testing.T, sched schedule.Store, reqs request.Store, lib library.Library) {
				require.NoError(t, lib.Add(ctx, library.Track{YTID: "yt5", Title: "Queued Track", Channel: "Artist 5", DurationS: 220}))
				_, err := reqs.Create(ctx, request.Item{
					Source: request.SourceAI, DisplayName: "DJ", YTID: "yt5",
					Title: "Queued Track", Channel: "Artist 5", Reason: "hop voi dem nay", Status: request.StatusReady,
					DurationS: 220,
				})
				require.NoError(t, err)
				// No SetNextUp — no pin.
			},
			setupState: func(t *testing.T) timeline.State {
				s := baseState()
				s.Pending = []request.Item{
					{ID: "r5", YTID: "yt5", Title: "Queued Track", Channel: "Artist 5",
						Source: request.SourceAI, DisplayName: "DJ", Reason: "hop voi dem nay",
						Status: request.StatusReady, DurationS: 220},
				}
				s.Durations = map[string]int{"yt5": 220}
				return s
			},
			wantFound:     true,
			wantCertainty: timeline.CertaintyProjected,
		},
		{
			// 6. No pin, queue empty (the lazy-shuffle arm — unknowable).
			name: "empty queue no pin",
			setupLive: func(t *testing.T, sched schedule.Store, reqs request.Store, lib library.Library) {
				// Library has tracks (so the station could run), but nothing is
				// committed and nothing is ready. planNext would roll a lazy
				// shuffle — unknowable ahead of time.
				require.NoError(t, lib.Add(ctx, library.Track{YTID: "yt6", Title: "Back Catalog", Channel: "Artist 6", DurationS: 210}))
			},
			setupState: func(t *testing.T) timeline.State {
				s := baseState()
				// No NextUp, no Pending → projector falls to KindUnknown.
				return s
			},
			wantFound: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sched, reqs, lib := schedule.NewMemStore(), request.NewMemStore(), library.NewMemLibrary()
			tc.setupLive(t, sched, reqs, lib)

			up, found, err := live.PeekNext(ctx, sched, reqs, lib)
			require.NoError(t, err)
			require.Equal(t, tc.wantFound, found, "PeekNext found mismatch")

			s := tc.setupState(t)
			upcoming, _, _ := timeline.Project(s)

			if !tc.wantFound {
				// found=false means UNKNOWABLE. The projector must NOT name a
				// track it cannot promise — the console would show a confident,
				// plausible, wrong running order.
				first := firstOfKind(t, upcoming, timeline.KindTrack, timeline.KindUnknown)
				require.Equal(t, timeline.KindUnknown, first.Kind,
					"PeekNext returned found=false but projector emitted a music segment — drift")
				require.Empty(t, first.Title,
					"an unknown block must never carry a title")
				return
			}

			// found=true: the projector's first music segment must match
			// PeekNext on title and carry the expected certainty.
			first := firstOfKind(t, upcoming, timeline.KindTrack)
			require.Equal(t, tc.wantCertainty, first.Certainty,
				"certainty mismatch at %q", first.Title)
			require.Equal(t, up.Track.Title, first.Title,
				"title mismatch: PeekNext=%q projector=%q", up.Track.Title, first.Title)
		})
	}
}
