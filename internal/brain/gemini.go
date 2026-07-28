package brain

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino-ext/components/model/gemini"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
	"google.golang.org/genai"
)

type geminiModel struct {
	cm      *gemini.ChatModel
	modelID string
}

// NewGemini builds a Gemini-backed Model. Structured output uses Gemini's
// native response schema via gemini.WithResponseJSONSchema.
func NewGemini(ctx context.Context, key, modelID string) (Model, error) {
	return newGeminiBase(ctx, key, modelID, "")
}

// newGeminiBase is the constructor the round-trip tests use: baseURL != ""
// points the SDK at an httptest server instead of the Gemini API.
func newGeminiBase(ctx context.Context, key, modelID, baseURL string) (Model, error) {
	cc := &genai.ClientConfig{APIKey: key, Backend: genai.BackendGeminiAPI}
	if baseURL != "" {
		cc.HTTPOptions.BaseURL = baseURL
	}
	cli, err := genai.NewClient(ctx, cc)
	if err != nil {
		return nil, fmt.Errorf("genai client: %w", err)
	}
	cm, err := gemini.NewChatModel(ctx, &gemini.Config{Client: cli, Model: modelID})
	if err != nil {
		return nil, fmt.Errorf("gemini chat model: %w", err)
	}
	return &geminiModel{cm: cm, modelID: modelID}, nil
}

func (m *geminiModel) Name() string     { return m.modelID }
func (m *geminiModel) Provider() string { return "gemini" }

func (m *geminiModel) Generate(ctx context.Context, system, user string, s *jsonschema.Schema) (string, error) {
	msg, err := m.cm.Generate(ctx,
		[]*schema.Message{schema.SystemMessage(system), schema.UserMessage(user)},
		gemini.WithResponseJSONSchema(s),
	)
	if err != nil {
		return "", err
	}
	if msg == nil || msg.Content == "" {
		return "", fmt.Errorf("gemini: empty response")
	}
	if err := errIfTruncated("gemini", msg); err != nil {
		return "", err
	}
	return msg.Content, nil
}
