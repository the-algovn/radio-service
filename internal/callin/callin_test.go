package callin

import (
	"context"
	"testing"

	"github.com/eino-contrib/jsonschema"
	"github.com/stretchr/testify/require"
)

type cannedModel struct{ raw string }

func (c cannedModel) Name() string     { return "fake" }
func (c cannedModel) Provider() string { return "fake" }
func (c cannedModel) Generate(context.Context, string, string, *jsonschema.Schema) (string, error) {
	return c.raw, nil
}

func TestParseHappyPath(t *testing.T) {
	m := cannedModel{raw: `{"song_query":"Em Của Ngày Hôm Qua Sơn Tùng","recipient":"Ngọc","message":"chúc ngủ ngon",` +
		`"verdict":"allow","reject_reason":"","digest":"Đức chúc Ngọc ngủ ngon","weight":"warm"}`}
	r, err := Parse(context.Background(), m, "cho mình xin bài...")
	require.NoError(t, err)
	require.Equal(t, "allow", r.Verdict)
	require.Equal(t, "warm", r.Weight)
}

func TestNormalizeRejectsBadEnums(t *testing.T) {
	_, err := Normalize(Result{Verdict: "maybe", Weight: "warm"})
	require.Error(t, err)
	_, err = Normalize(Result{Verdict: "allow", Weight: "spicy"})
	require.Error(t, err)
	r, err := Normalize(Result{Verdict: "reject", RejectReason: "spam", Weight: ""})
	require.NoError(t, err)
	require.Equal(t, "casual", r.Weight) // weight defaults when absent
}

func TestSaveFixture(t *testing.T) {
	dir := t.TempDir()
	p, err := SaveFixture(dir, "happy-dedication", "raw text", `{"verdict":"allow"}`)
	require.NoError(t, err)
	require.FileExists(t, p)
	_, err = SaveFixture(dir, "../evil", "x", "{}")
	require.Error(t, err)
}
