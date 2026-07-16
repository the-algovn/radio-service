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

func TestParseOutputStripsFences(t *testing.T) {
	raw := "```json\n{\"script\":\"xin chào\",\"summary\":\"chào\",\"used_phrases\":[\"bạn nghe đài\"]}\n```"
	out, err := ParseOutput(raw)
	require.NoError(t, err)
	require.Equal(t, "xin chào", out.Script)
	require.Equal(t, []string{"bạn nghe đài"}, out.UsedPhrases)
}
