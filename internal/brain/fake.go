package brain

import (
	"context"

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

func (f fake) Generate(context.Context, string, string, *jsonschema.Schema) (string, error) {
	return f.canned, nil
}
