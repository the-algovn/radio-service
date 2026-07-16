package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadFilePopulatesUnsetVars(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "lab.env")
	require.NoError(t, os.WriteFile(f, []byte("# comment\nFOO_A=from-file\nFOO_B=file-loses\n\n"), 0o600))
	t.Setenv("FOO_B", "env-wins")
	require.NoError(t, loadFile(f))
	require.Equal(t, "from-file", os.Getenv("FOO_A"))
	require.Equal(t, "env-wins", os.Getenv("FOO_B"))
	t.Setenv("FOO_A", "") // cleanup semantics via t.Setenv snapshot
}

func TestGetDefault(t *testing.T) {
	require.Equal(t, "fallback", Get("NO_SUCH_KEY_XYZ", "fallback"))
	t.Setenv("SOME_KEY_XYZ", "set")
	require.Equal(t, "set", Get("SOME_KEY_XYZ", "fallback"))
}
