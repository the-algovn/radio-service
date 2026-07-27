package brain_test

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	einomodel "github.com/cloudwego/eino/components/model"
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

// fake is not a real Eino component, so it must emit OnStart/OnEnd itself or
// the audit callback handler never fires for keyless mode. This proves the
// sequence fires and Config.Model is populated (RunInfo.Name is never set by
// this plan's plain-Go orchestration, so Config is the only source audit has).
func TestFakeEmitsCallbacksForAuditing(t *testing.T) {
	var started, ended int
	var startModel, endModel string
	h := callbacks.NewHandlerBuilder().
		OnStartFn(func(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
			started++
			if in, ok := input.(*einomodel.CallbackInput); ok && in.Config != nil {
				startModel = in.Config.Model
			}
			return ctx
		}).
		OnEndFn(func(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
			ended++
			if out, ok := output.(*einomodel.CallbackOutput); ok && out.Config != nil {
				endModel = out.Config.Model
			}
			return ctx
		}).
		Build()

	ctx := callbacks.InitCallbacks(context.Background(),
		&callbacks.RunInfo{Type: "fake", Component: components.ComponentOfChatModel}, h)

	m := brain.NewFake("canned")
	raw, err := m.Generate(ctx, "sys", "usr", nil)
	require.NoError(t, err)
	require.Equal(t, "canned", raw)

	require.Equal(t, 1, started)
	require.Equal(t, 1, ended)
	require.Equal(t, "fake", startModel)
	require.Equal(t, "fake", endModel)
}
