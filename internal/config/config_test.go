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

func TestGetBool(t *testing.T) {
	t.Setenv("RL_FLAG_TRUE", "true")
	t.Setenv("RL_FLAG_OTHER", "yes")
	if !GetBool("RL_FLAG_TRUE", false) {
		t.Fatal(`"true" should parse as true`)
	}
	if GetBool("RL_FLAG_OTHER", false) {
		t.Fatal(`only literal "true" is true; "yes" must be false`)
	}
	if !GetBool("RL_FLAG_UNSET", true) {
		t.Fatal("unset must return the default")
	}
}
