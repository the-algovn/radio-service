//go:build integration

package artifact_test

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/artifact"
	"github.com/the-algovn/radio-service/internal/testutil"
)

func TestS3StoreRoundTrip(t *testing.T) {
	endpoint, ak, sk := testutil.StartMinio(t)
	ctx := context.Background()

	// create the bucket
	mc, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(ak, sk, ""), Secure: false})
	require.NoError(t, err)
	require.NoError(t, mc.MakeBucket(ctx, "radio-lab", minio.MakeBucketOptions{}))

	s, err := artifact.NewS3Store(artifact.S3Config{
		Endpoint: endpoint, PublicEndpoint: endpoint, AccessKey: ak, SecretKey: sk, Bucket: "radio-lab", UseSSL: false,
	})
	require.NoError(t, err)

	a, err := s.Save(ctx, "take", "wav", "hi", []byte("RIFFdata"), nil)
	require.NoError(t, err)

	got, err := s.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "hi", got.Label)

	list, err := s.List(ctx, "take")
	require.NoError(t, err)
	require.Len(t, list, 1)

	// presign → HTTP GET the bytes back
	u, err := s.PresignGet(ctx, a.ID)
	require.NoError(t, err)
	resp, err := http.Get(u)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, "RIFFdata", string(b))

	// FetchToFile
	p, err := s.FetchToFile(ctx, a.ID, t.TempDir())
	require.NoError(t, err)
	require.FileExists(t, p)
}
