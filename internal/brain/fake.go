package brain

import (
	"context"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
)

// FakeScript is the canned talk-break reply used in keyless mode, so dev and
// degraded prod still get spoken segments for $0.
const FakeScript = `{"script":"Đêm nay trời dịu, bạn nghe đài thân mến… mình cùng nghe tiếp nhé.",` +
	`"summary":"filler đêm khuya (fake model)","used_phrases":["bạn nghe đài"]}`

type fake struct{ canned string }

// NewFake returns a Model that always replies with canned and spends nothing.
// It is a real Model (unlike the programmer's keyless path, which bypasses the
// model entirely) because the director and callin need one in keyless mode.
func NewFake(canned string) Model { return fake{canned: canned} }

func (fake) Name() string     { return "fake" }
func (fake) Provider() string { return "fake" }

// Generate emits the same OnStart/OnEnd callback sequence a real Eino
// component would, so audit.NewCallback records fake calls exactly like the
// old audit.Wrap(brain.Fake{}, …) did (Fake: true, cost 0) instead of silently
// losing the audit trail in keyless mode. Real components emit these from
// inside their own Generate; fake has no framework to do it for it, so it
// does so itself.
func (f fake) Generate(ctx context.Context, system, user string, _ *jsonschema.Schema) (string, error) {
	ctx = callbacks.EnsureRunInfo(ctx, "fake", components.ComponentOfChatModel)
	cfg := &einomodel.Config{Model: "fake"}
	ctx = callbacks.OnStart(ctx, &einomodel.CallbackInput{
		Messages: []*schema.Message{schema.SystemMessage(system), schema.UserMessage(user)},
		Config:   cfg,
	})
	callbacks.OnEnd(ctx, &einomodel.CallbackOutput{
		Message:    schema.AssistantMessage(f.canned, nil),
		Config:     cfg,
		TokenUsage: &einomodel.TokenUsage{},
	})
	return f.canned, nil
}
