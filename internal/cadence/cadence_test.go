package cadence_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/cadence"
	"github.com/the-algovn/radio-service/internal/station"
)

var now = time.Date(2026, 9, 8, 21, 30, 0, 0, time.UTC)

// live is a state where music has aired and no timer is elapsed: nothing is
// due unless a case says so.
func live() cadence.State {
	return cadence.State{
		Now:                 now,
		SessionHasMusic:     true,
		LastStationID:       now,
		LastMusing:          now,
		StationIDsAvailable: true,
	}
}

func TestDueKind(t *testing.T) {
	dj := station.DJSettings{BreakEvery: 2, StationIDMin: 60, MusingEveryMin: 10}

	cases := []struct {
		name string
		mut  func(s *cadence.State)
		dj   *station.DJSettings
		want string
	}{
		{name: "nothing due", mut: func(s *cadence.State) {}, want: ""},

		{
			name: "seam when the counter is owed",
			mut:  func(s *cadence.State) { s.FinishedSinceSeam = 1 },
			want: cadence.KindSeam,
		},
		{
			name: "seam when forced with the counter nowhere near owed",
			mut:  func(s *cadence.State) { s.Forced = true },
			want: cadence.KindSeam,
		},
		{
			name: "no seam when BreakEvery is zero",
			mut:  func(s *cadence.State) { s.FinishedSinceSeam = 9 },
			dj:   &station.DJSettings{BreakEvery: 0, StationIDMin: 60},
			want: "",
		},

		{
			name: "station id when its timer is elapsed",
			mut:  func(s *cadence.State) { s.LastStationID = now.Add(-61 * time.Minute) },
			want: cadence.KindStationID,
		},
		{
			name: "station id outranks an owed seam",
			mut: func(s *cadence.State) {
				s.LastStationID = now.Add(-61 * time.Minute)
				s.FinishedSinceSeam = 5
			},
			want: cadence.KindStationID,
		},
		{
			name: "no station id without usable lines",
			mut: func(s *cadence.State) {
				s.LastStationID = now.Add(-61 * time.Minute)
				s.StationIDsAvailable = false
			},
			want: "",
		},
		{
			name: "a zero LastStationID is not overdue since the epoch",
			mut:  func(s *cadence.State) { s.LastStationID = time.Time{} },
			want: "",
		},

		{
			name: "musing when its timer is elapsed",
			mut:  func(s *cadence.State) { s.LastMusing = now.Add(-11 * time.Minute) },
			want: cadence.KindMusing,
		},
		{
			name: "a zero LastMusing is not overdue since the epoch",
			mut:  func(s *cadence.State) { s.LastMusing = time.Time{} },
			want: "",
		},
		{
			name: "no musing when MusingEveryMin is zero",
			mut:  func(s *cadence.State) { s.LastMusing = now.Add(-11 * time.Minute) },
			dj:   &station.DJSettings{BreakEvery: 2, StationIDMin: 60, MusingEveryMin: 0},
			want: "",
		},
		{
			name: "musing outranks an owed seam",
			mut: func(s *cadence.State) {
				s.LastMusing = now.Add(-11 * time.Minute)
				s.FinishedSinceSeam = 5
			},
			want: cadence.KindMusing,
		},

		{
			name: "daypart transition inside the window",
			mut:  func(s *cadence.State) { s.PendingDaypart = now.Add(-5 * time.Minute) },
			want: cadence.KindDaypartTransition,
		},
		{
			name: "daypart transition at the window edge still airs",
			mut:  func(s *cadence.State) { s.PendingDaypart = now.Add(-cadence.DaypartWindow) },
			want: cadence.KindDaypartTransition,
		},
		{
			name: "daypart transition past the window is gone",
			mut: func(s *cadence.State) {
				s.PendingDaypart = now.Add(-cadence.DaypartWindow - time.Second)
			},
			want: "",
		},
		{
			name: "a daypart armed in the future is not due",
			mut:  func(s *cadence.State) { s.PendingDaypart = now.Add(time.Minute) },
			want: "",
		},
		{
			name: "daypart outranks a due musing",
			mut: func(s *cadence.State) {
				s.PendingDaypart = now.Add(-time.Minute)
				s.LastMusing = now.Add(-11 * time.Minute)
			},
			want: cadence.KindDaypartTransition,
		},

		{
			name: "wake greeting when armed",
			mut:  func(s *cadence.State) { s.PendingWake = true },
			want: cadence.KindWakeGreeting,
		},
		{
			name: "wake outranks daypart, musing and an owed seam",
			mut: func(s *cadence.State) {
				s.PendingWake = true
				s.PendingDaypart = now.Add(-time.Minute)
				s.LastMusing = now.Add(-11 * time.Minute)
				s.FinishedSinceSeam = 5
			},
			want: cadence.KindWakeGreeting,
		},
		{
			name: "station id outranks even a wake greeting",
			mut: func(s *cadence.State) {
				s.PendingWake = true
				s.LastStationID = now.Add(-61 * time.Minute)
			},
			want: cadence.KindStationID,
		},

		{
			name: "no session music suppresses every kind but the station id",
			mut: func(s *cadence.State) {
				s.SessionHasMusic = false
				s.PendingWake = true
				s.FinishedSinceSeam = 5
			},
			want: "",
		},
		{
			name: "a station id still airs before any music this session",
			mut: func(s *cadence.State) {
				s.SessionHasMusic = false
				s.LastStationID = now.Add(-61 * time.Minute)
			},
			want: cadence.KindStationID,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := live()
			tc.mut(&s)
			settings := dj
			if tc.dj != nil {
				settings = *tc.dj
			}
			require.Equal(t, tc.want, cadence.DueKind(s, settings))
		})
	}
}

func TestAdvance(t *testing.T) {
	later := now.Add(time.Hour)

	t.Run("a station id stamps only its own timer", func(t *testing.T) {
		s := live()
		s.FinishedSinceSeam = 3
		s.Forced = true
		got := cadence.Advance(s, cadence.KindStationID, later)
		require.Equal(t, later, got.LastStationID)
		require.Equal(t, 3, got.FinishedSinceSeam, "a station id is not a break she wrote")
		require.True(t, got.Forced, "a forced seam is still owed after a station id airs")
	})

	t.Run("a seam resets the counter and consumes the arming", func(t *testing.T) {
		s := live()
		s.FinishedSinceSeam = 3
		s.Forced = true
		got := cadence.Advance(s, cadence.KindSeam, later)
		require.Equal(t, 0, got.FinishedSinceSeam)
		require.False(t, got.Forced)
	})

	t.Run("a musing resets the counter, stamps its timer, and leaves the arming", func(t *testing.T) {
		s := live()
		s.FinishedSinceSeam = 3
		s.Forced = true
		got := cadence.Advance(s, cadence.KindMusing, later)
		require.Equal(t, 0, got.FinishedSinceSeam)
		require.Equal(t, later, got.LastMusing)
		require.True(t, got.Forced,
			"Forced only ever makes a seam due, so only a seam may consume it")
	})

	t.Run("a daypart transition clears its pending and leaves the arming", func(t *testing.T) {
		s := live()
		s.FinishedSinceSeam = 3
		s.Forced = true
		s.PendingDaypart = now.Add(-time.Minute)
		got := cadence.Advance(s, cadence.KindDaypartTransition, later)
		require.Equal(t, 0, got.FinishedSinceSeam)
		require.True(t, got.PendingDaypart.IsZero())
		require.True(t, got.Forced)
	})

	t.Run("a wake greeting clears its flag and leaves the arming", func(t *testing.T) {
		s := live()
		s.FinishedSinceSeam = 3
		s.Forced = true
		s.PendingWake = true
		got := cadence.Advance(s, cadence.KindWakeGreeting, later)
		require.Equal(t, 0, got.FinishedSinceSeam)
		require.False(t, got.PendingWake)
		require.True(t, got.Forced)
	})

	t.Run("an unknown kind changes nothing", func(t *testing.T) {
		s := live()
		s.FinishedSinceSeam = 3
		require.Equal(t, s, cadence.Advance(s, "not_a_kind", later))
	})

	t.Run("the argument is not mutated", func(t *testing.T) {
		s := live()
		s.FinishedSinceSeam = 3
		_ = cadence.Advance(s, cadence.KindSeam, later)
		require.Equal(t, 3, s.FinishedSinceSeam)
	})
}

func TestAdvanceMusic(t *testing.T) {
	s := live()
	s.SessionHasMusic = false
	s.FinishedSinceSeam = 1

	got := cadence.AdvanceMusic(s)
	require.Equal(t, 2, got.FinishedSinceSeam)
	require.True(t, got.SessionHasMusic)
	require.Equal(t, 1, s.FinishedSinceSeam, "the argument is not mutated")
}
