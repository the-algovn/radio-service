package brain

import "context"

type Fake struct{}

func (Fake) Name() string { return "fake" }

func (Fake) Generate(context.Context, string, string) (string, Usage, error) {
	return `{"script":"Đêm nay trời dịu, bạn nghe đài thân mến… mình cùng nghe tiếp nhé.",` +
		`"summary":"filler đêm khuya (fake model)","used_phrases":["bạn nghe đài"]}`, Usage{In: 0, Out: 0}, nil
}
