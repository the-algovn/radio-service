//go:build integration

// Requires a running podman machine:
//
//	export DOCKER_HOST="unix://$(podman machine inspect --format '{{.ConnectionInfo.PodmanSocket.Path}}')"
//	export TESTCONTAINERS_RYUK_DISABLED=true
package testutil

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// StartPostgres runs postgres:18-alpine and returns a pgx URL.
func StartPostgres(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	c, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("radio_lab"),
		tcpostgres.WithUsername("radio_lab"),
		tcpostgres.WithPassword("test"),
		tcpostgres.BasicWaitStrategies(),
	)
	testcontainers.CleanupContainer(t, c)
	require.NoError(t, err)
	url, err := c.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	return url
}

// StartMinio runs a MinIO container and returns endpoint (host:port, no scheme),
// access key, secret key. Bucket creation is the caller's job.
func StartMinio(t *testing.T) (endpoint, accessKey, secretKey string) {
	t.Helper()
	ctx := context.Background()
	c, err := tcminio.Run(ctx, "minio/minio:latest")
	testcontainers.CleanupContainer(t, c)
	require.NoError(t, err)
	ep, err := c.ConnectionString(ctx)
	require.NoError(t, err)
	return ep, c.Username, c.Password
}
