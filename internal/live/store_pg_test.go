//go:build integration

package live_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/testutil"
)

func TestPGStoresContract(t *testing.T) {
	runStoreContract(t, func(t *testing.T) (live.AirLog, live.Listeners) {
		t.Helper()
		url := testutil.StartPostgres(t)
		testutil.Migrate(t, url)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		t.Cleanup(cancel)
		pool, err := pgxpool.New(ctx, url)
		require.NoError(t, err)
		t.Cleanup(pool.Close)
		return live.NewPGAirLog(pool), live.NewPGListeners(pool)
	})
}
