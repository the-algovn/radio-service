package audit_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/audit"
)

type storeFactory func(t *testing.T) audit.Store

func runStoreContract(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	base := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	seed := func(s audit.Store) {
		require.NoError(t, s.Record(ctx, audit.Rec{TS: base, Label: "director:backsell", Model: "gemini-2.5-flash", Provider: "gemini", System: "sys1", User: "u1", Output: "o1", InTokens: 100, OutTokens: 40, CostUSD: 0.001}))
		require.NoError(t, s.Record(ctx, audit.Rec{TS: base.Add(time.Minute), Label: "programmer:pick", Model: "gemini-2.5-flash", Provider: "gemini", System: "sys2", User: "u2", Output: "", InTokens: 200, OutTokens: 0, CostUSD: 0.002, Error: "boom"}))
		require.NoError(t, s.Record(ctx, audit.Rec{TS: base.Add(2 * time.Minute), Label: "director:backsell", Model: "claude-x", Provider: "anthropic", System: "sys3", User: "u3", Output: "o3", InTokens: 300, OutTokens: 60, CostUSD: 0.004}))
	}

	t.Run("list newest-first with paging + total", func(t *testing.T) {
		s := newStore(t)
		seed(s)
		all, err := s.List(ctx, audit.Filter{}, 10, 0)
		require.NoError(t, err)
		require.Len(t, all, 3)
		require.Equal(t, "director:backsell", all[0].Label) // newest (base+2m) first
		require.Equal(t, "o3", all[0].Output)
		require.NotZero(t, all[0].ID)
		total, err := s.Count(ctx, audit.Filter{})
		require.NoError(t, err)
		require.Equal(t, int64(3), total)

		page, err := s.List(ctx, audit.Filter{}, 2, 2)
		require.NoError(t, err)
		require.Len(t, page, 1) // 3 rows, offset 2 → last one
	})

	t.Run("filter by label", func(t *testing.T) {
		s := newStore(t)
		seed(s)
		got, err := s.List(ctx, audit.Filter{Label: "director:backsell"}, 10, 0)
		require.NoError(t, err)
		require.Len(t, got, 2)
		n, err := s.Count(ctx, audit.Filter{Label: "director:backsell"})
		require.NoError(t, err)
		require.Equal(t, int64(2), n)
	})

	t.Run("errors only", func(t *testing.T) {
		s := newStore(t)
		seed(s)
		got, err := s.List(ctx, audit.Filter{ErrorsOnly: true}, 10, 0)
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, "boom", got[0].Error)
	})

	t.Run("script: filter matches all script:* labels (prefix), exact still works", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.Record(ctx, audit.Rec{TS: base, Label: "script:intro", Model: "m", Provider: "fake"}))
		require.NoError(t, s.Record(ctx, audit.Rec{TS: base.Add(time.Minute), Label: "script:backsell", Model: "m", Provider: "fake"}))
		require.NoError(t, s.Record(ctx, audit.Rec{TS: base.Add(2 * time.Minute), Label: "callin", Model: "m", Provider: "fake"}))

		byPrefix, err := s.List(ctx, audit.Filter{Label: "script:"}, 10, 0)
		require.NoError(t, err)
		require.Len(t, byPrefix, 2) // both script:* rows, not callin
		n, err := s.Count(ctx, audit.Filter{Label: "script:"})
		require.NoError(t, err)
		require.Equal(t, int64(2), n)

		exact, err := s.List(ctx, audit.Filter{Label: "script:intro"}, 10, 0)
		require.NoError(t, err)
		require.Len(t, exact, 1)
	})

	t.Run("programmer: filter matches all programmer:* labels (prefix), exact still works", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.Record(ctx, audit.Rec{TS: base, Label: "programmer:intent", Model: "m", Provider: "fake"}))
		require.NoError(t, s.Record(ctx, audit.Rec{TS: base.Add(time.Minute), Label: "programmer:choose", Model: "m", Provider: "fake"}))
		require.NoError(t, s.Record(ctx, audit.Rec{TS: base.Add(2 * time.Minute), Label: "programmer:repair", Model: "m", Provider: "fake"}))
		require.NoError(t, s.Record(ctx, audit.Rec{TS: base.Add(3 * time.Minute), Label: "director:backsell", Model: "m", Provider: "fake"}))

		byPrefix, err := s.List(ctx, audit.Filter{Label: "programmer:"}, 10, 0)
		require.NoError(t, err)
		require.Len(t, byPrefix, 3) // all three programmer:* rows, not director:backsell
		n, err := s.Count(ctx, audit.Filter{Label: "programmer:"})
		require.NoError(t, err)
		require.Equal(t, int64(3), n)

		exact, err := s.List(ctx, audit.Filter{Label: "programmer:choose"}, 10, 0)
		require.NoError(t, err)
		require.Len(t, exact, 1)
	})

	t.Run("stats group by label+model since", func(t *testing.T) {
		s := newStore(t)
		seed(s)
		stats, err := s.Stats(ctx, base.Add(-time.Hour))
		require.NoError(t, err)
		require.Len(t, stats, 3) // (director,gemini),(programmer,gemini),(director,claude-x)
		var total float64
		for _, st := range stats {
			total += st.CostUSD
		}
		require.InDelta(t, 0.007, total, 1e-9)

		recent, err := s.Stats(ctx, base.Add(90*time.Second)) // only the base+2m row
		require.NoError(t, err)
		require.Len(t, recent, 1)
		require.Equal(t, 1, recent[0].Count)
		require.Equal(t, 300, recent[0].InTokens)
	})
}
