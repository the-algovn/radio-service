package persona

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Save(dir, "# persona v-test"))
	got, err := Load(dir)
	require.NoError(t, err)
	require.Equal(t, "# persona v-test", got)
}

func TestLoadMissingIsError(t *testing.T) {
	_, err := Load(t.TempDir())
	require.Error(t, err)
}
