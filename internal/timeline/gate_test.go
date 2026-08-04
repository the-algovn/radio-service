package timeline_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/station"
	"github.com/the-algovn/radio-service/internal/timeline"
)

func onAir() station.Station { return station.Station{OnAir: true, AIEnabled: true} }

func TestGatePrecedence(t *testing.T) {
	// Several gates fail at once in the COMMON case, not the exotic one — an
	// off-air station also has zero listeners. The order names the thing to
	// fix first, cheapest-to-act-on last.
	for _, tc := range []struct {
		name string
		st   timeline.State
		want string
	}{
		{
			// Off air outranks everything: no air means no breaks, and
			// reporting dj_disabled would send someone to redeploy when the
			// fix is one button.
			name: "off air outranks a disabled dj and zero listeners",
			st:   timeline.State{Station: station.Station{}, Dir: timeline.DirectorSnapshot{Present: false}},
			want: timeline.GateOffAir,
		},
		{
			name: "dj disabled outranks ai paused",
			st: timeline.State{
				Station: station.Station{OnAir: true, AIEnabled: false},
				Dir:     timeline.DirectorSnapshot{Present: false}, Listeners: 3,
			},
			want: timeline.GateDJDisabled,
		},
		{
			name: "ai paused outranks budget",
			st: timeline.State{
				Station:   station.Station{OnAir: true, AIEnabled: false},
				Dir:       timeline.DirectorSnapshot{Present: true},
				Listeners: 3, SpentUSD: 99, BudgetUSD: 1,
			},
			want: timeline.GateAIPaused,
		},
		{
			name: "budget outranks no listeners",
			st: timeline.State{
				Station: onAir(), Dir: timeline.DirectorSnapshot{Present: true},
				Listeners: 0, SpentUSD: 5, BudgetUSD: 5,
			},
			want: timeline.GateBudget,
		},
		{
			name: "no listeners is the last real gate",
			st: timeline.State{
				Station: onAir(), Dir: timeline.DirectorSnapshot{Present: true},
				Listeners: 0, SpentUSD: 0, BudgetUSD: 5,
			},
			want: timeline.GateNoListeners,
		},
		{
			name: "everything satisfied",
			st: timeline.State{
				Station: onAir(), Dir: timeline.DirectorSnapshot{Present: true},
				Listeners: 1, SpentUSD: 0, BudgetUSD: 5,
			},
			want: timeline.GateOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, timeline.Gate(tc.st))
		})
	}
}

func TestGateBudgetIsInclusive(t *testing.T) {
	// The director idles at spent >= budget, not > budget. Off-by-one here
	// would show `ok` on the exact tick the DJ goes quiet.
	st := timeline.State{
		Station: onAir(), Dir: timeline.DirectorSnapshot{Present: true},
		Listeners: 1, SpentUSD: 5.0, BudgetUSD: 5.0,
	}
	require.Equal(t, timeline.GateBudget, timeline.Gate(st))
}
