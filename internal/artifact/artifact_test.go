package artifact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSaveListPath(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	a, err := s.Save("take", "wav", "thử giọng", []byte("RIFFdata"), map[string]string{"voice": "x"})
	require.NoError(t, err)
	require.Regexp(t, `^take-\d+-[0-9a-f]{8}$`, a.ID)
	require.Equal(t, int64(8), a.Bytes)

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "thử giọng", list[0].Label)

	p, err := s.Path(a.ID)
	require.NoError(t, err)
	require.FileExists(t, p)
}

func TestPathRejectsTraversal(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	_, err := s.Path("../etc/passwd")
	require.Error(t, err)
	_, err = s.Path("no-such-id")
	require.Error(t, err)
}
