//go:build integration

package audit_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/audit"
	"github.com/the-algovn/radio-service/internal/testutil"
)

func newPGPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := testutil.StartPostgres(t)
	testutil.Migrate(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func TestPGStoreContract(t *testing.T) {
	runStoreContract(t, func(t *testing.T) audit.Store {
		// long retention so prune never interferes with contract assertions
		return audit.NewPGStore(newPGPool(t), 100*365*24*time.Hour)
	})
}

func TestPGStorePrunesOnWrite(t *testing.T) {
	ctx := context.Background()
	s := audit.NewPGStore(newPGPool(t), 30*24*time.Hour)
	require.NoError(t, s.Record(ctx, audit.Rec{TS: time.Now().Add(-40 * 24 * time.Hour), Label: "old", Model: "m", Provider: "fake"}))
	require.NoError(t, s.Record(ctx, audit.Rec{TS: time.Now(), Label: "new", Model: "m", Provider: "fake"}))
	got, err := s.List(ctx, audit.Filter{}, 10, 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "new", got[0].Label)
}
