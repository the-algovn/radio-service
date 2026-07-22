//go:build integration

package station_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/station"
	"github.com/the-algovn/radio-service/internal/testutil"
)

func TestPGStoreContract(t *testing.T) {
	runStoreContract(t, func(t *testing.T) station.Store {
		t.Helper()
		url := testutil.StartPostgres(t)
		testutil.Migrate(t, url)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		t.Cleanup(cancel)
		pool, err := pgxpool.New(ctx, url)
		require.NoError(t, err)
		t.Cleanup(pool.Close)
		return station.NewPGStore(pool)
	})
}
