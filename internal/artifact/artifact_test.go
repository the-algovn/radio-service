package artifact

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFakeStoreSaveListFetch(t *testing.T) {
	ctx := context.Background()
	s := NewFakeStore()
	a, err := s.Save(ctx, "take", "wav", "thử giọng", []byte("RIFFdata"), map[string]string{"voice": "x"})
	require.NoError(t, err)
	require.Regexp(t, `^take-\d+-[0-9a-f]{8}$`, a.ID)
	require.Equal(t, int64(8), a.Bytes)

	list, err := s.List(ctx, "take")
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "thử giọng", list[0].Label)
	require.Empty(t, mustList(t, s, "render")) // kind filter works

	u, err := s.PresignGet(ctx, a.ID)
	require.NoError(t, err)
	require.Contains(t, u, a.ID)

	p, err := s.FetchToFile(ctx, a.ID, t.TempDir())
	require.NoError(t, err)
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	require.Equal(t, "RIFFdata", string(b))
	require.Equal(t, ".wav", filepath.Ext(p))
}

func TestFakeStoreDelete(t *testing.T) {
	ctx := context.Background()
	s := NewFakeStore()
	a, err := s.Save(ctx, "take", "wav", "hi", []byte("RIFFdata"), nil)
	require.NoError(t, err)

	require.NoError(t, s.Delete(ctx, a.ID))

	_, err = s.Get(ctx, a.ID)
	require.Error(t, err)                    // meta gone
	require.Empty(t, mustList(t, s, "take")) // blob/meta both gone

	err = s.Delete(ctx, a.ID)
	require.Error(t, err) // already gone, mirrors Get's not-found

	err = s.Delete(ctx, "../etc/passwd")
	require.Error(t, err) // idRe guard rejects invalid ids
}

func mustList(t *testing.T, s *FakeStore, kind string) []Artifact {
	l, err := s.List(context.Background(), kind)
	require.NoError(t, err)
	return l
}
