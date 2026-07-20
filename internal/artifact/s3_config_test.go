package artifact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewS3StoreSplitTLS(t *testing.T) {
	// Internal plaintext endpoint, public HTTPS endpoint — must construct without error.
	s, err := NewS3Store(S3Config{
		Endpoint: "minio.minio.svc:9000", UseSSL: false,
		PublicEndpoint: "s3.algovn.com", PublicUseSSL: true,
		AccessKey: "a", SecretKey: "b", Bucket: "radio-lab",
	})
	require.NoError(t, err)
	require.NotNil(t, s)
}
