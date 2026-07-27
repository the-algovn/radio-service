package brain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateDigitLint(t *testing.T) {
	v := Validate("Bây giờ là 21:00 rồi.", 500)
	require.Len(t, v, 1)
	require.Contains(t, v[0], "digit")
	require.Empty(t, Validate("Bây giờ là chín giờ tối.", 500))
}

func TestValidateLength(t *testing.T) {
	v := Validate(strings.Repeat("a", 501), 500)
	require.Len(t, v, 1)
	require.Contains(t, v[0], "length")
}

func TestParseOutputRejectsNonJSON(t *testing.T) {
	_, err := ParseOutput("not json at all")
	require.Error(t, err)
}

func TestParseOutputRejectsEmptyScript(t *testing.T) {
	_, err := ParseOutput(`{"script":"","summary":"s","used_phrases":[]}`)
	require.Error(t, err)
}
