package director

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeIDs(t *testing.T, lines string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "station-ids.txt")
	require.NoError(t, os.WriteFile(p, []byte(lines), 0o644))
	return p
}

func TestStationIDsRotateSequentially(t *testing.T) {
	s := loadStationIDs(writeIDs(t, "một câu\nhai câu\n"), 450, nil)
	// NOTE: "một"/"hai" are words, not numerals — digit-lint clean.
	require.True(t, s.available())
	a, ok := s.next()
	require.True(t, ok)
	require.Equal(t, "một câu", a)
	b, _ := s.next()
	require.Equal(t, "hai câu", b)
	c, _ := s.next() // wraps
	require.Equal(t, "một câu", c)
}

func TestStationIDsDropInvalidLines(t *testing.T) {
	long := strings.Repeat("d", 500)
	s := loadStationIDs(writeIDs(t, "có 42 số\n"+long+"\nsạch sẽ\n\n# chú thích\n"), 450, nil)
	// digit line and over-cap line dropped; blank and #-comment ignored
	line, ok := s.next()
	require.True(t, ok)
	require.Equal(t, "sạch sẽ", line)
	line2, _ := s.next()
	require.Equal(t, "sạch sẽ", line2) // only one valid line — keeps rotating it
}

func TestStationIDsMissingFileIneligible(t *testing.T) {
	s := loadStationIDs(filepath.Join(t.TempDir(), "nope.txt"), 450, nil)
	require.False(t, s.available())
	_, ok := s.next()
	require.False(t, ok)
}
