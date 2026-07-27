package brain_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/the-algovn/radio-service/internal/brain"
)

func TestFakeReturnsCannedAndIdentifiesItself(t *testing.T) {
	m := brain.NewFake(`{"script":"xin chào","summary":"s","used_phrases":[]}`)

	require.Equal(t, "fake", m.Name())
	require.Equal(t, "fake", m.Provider())

	raw, err := m.Generate(context.Background(), "sys", "usr", brain.ScriptSchema)
	require.NoError(t, err)

	out, err := brain.ParseOutput(raw)
	require.NoError(t, err)
	require.Equal(t, "xin chào", out.Script)
}

// FakeScript is what main.go wires in keyless mode; it must satisfy the
// director's parser so talk breaks still work for $0.
func TestFakeScriptConstantParses(t *testing.T) {
	out, err := brain.ParseOutput(brain.FakeScript)
	require.NoError(t, err)
	require.NotEmpty(t, out.Script)
}
