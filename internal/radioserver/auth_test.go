package radioserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
)

// authCtx forges a gateway-forwarded JWT (the gateway already verified the
// signature; services only decode segment 2 — authnz-conventions.md).
func authCtx(t *testing.T, claims map[string]string) context.Context {
	t.Helper()
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	tok := "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	md := metadata.Pairs("authorization", "Bearer "+tok)
	return metadata.NewIncomingContext(context.Background(), md)
}

func TestIdentityFromContext(t *testing.T) {
	sub, name, err := identityFromContext(authCtx(t, map[string]string{"sub": "u-123456789", "name": "Ngọc"}))
	require.NoError(t, err)
	require.Equal(t, "u-123456789", sub)
	require.Equal(t, "Ngọc", name)

	// name → preferred_username → derived fallback
	_, name, err = identityFromContext(authCtx(t, map[string]string{"sub": "u-1", "preferred_username": "ngoc.98"}))
	require.NoError(t, err)
	require.Equal(t, "ngoc.98", name)
	_, name, err = identityFromContext(authCtx(t, map[string]string{"sub": "abcdefghij"}))
	require.NoError(t, err)
	require.Equal(t, "thính giả abcdef", name)

	// no metadata / no sub → error
	_, _, err = identityFromContext(context.Background())
	require.Error(t, err)
	_, _, err = identityFromContext(authCtx(t, map[string]string{"name": "x"}))
	require.Error(t, err)
}

func TestBucketsRefill(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	b := newBuckets(func() time.Time { return now })
	for i := 0; i < 10; i++ {
		require.True(t, b.allow("u1"), "call %d", i)
	}
	require.False(t, b.allow("u1")) // capacity 10 exhausted
	require.True(t, b.allow("u2")) // per-user isolation

	now = now.Add(6 * time.Second) // one token refilled
	require.True(t, b.allow("u1"))
	require.False(t, b.allow("u1"))

	now = now.Add(10 * time.Minute) // refill clamps at capacity
	for i := 0; i < 10; i++ {
		require.True(t, b.allow("u1"), "post-clamp call %d", i)
	}
	require.False(t, b.allow("u1"))
}
