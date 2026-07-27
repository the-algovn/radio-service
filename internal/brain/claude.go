package brain

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
)

const (
	requestTimeout = 25 * time.Second
	maxTokens      = 2048
)

type claudeModel struct {
	cm      *claude.ChatModel
	modelID string
}

// NewClaude builds an Anthropic-backed Model. Structured output uses
// Anthropic's native OutputConfig via claude.WithResponseFormat, which retires
// the old assistant-"{"-prefill hack.
func NewClaude(ctx context.Context, key, modelID string) (Model, error) {
	return newClaudeBase(ctx, key, modelID, "")
}

// newClaudeBase is the constructor the round-trip tests use: baseURL != ""
// points the SDK at an httptest server instead of api.anthropic.com.
func newClaudeBase(ctx context.Context, key, modelID, baseURL string) (Model, error) {
	cfg := &claude.Config{
		APIKey:         key,
		Model:          modelID,
		MaxTokens:      maxTokens,
		RequestTimeout: requestTimeout,
	}
	if baseURL != "" {
		cfg.BaseURL = &baseURL
	}
	cm, err := claude.NewChatModel(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("claude chat model: %w", err)
	}
	return &claudeModel{cm: cm, modelID: modelID}, nil
}

func (m *claudeModel) Name() string     { return m.modelID }
func (m *claudeModel) Provider() string { return "anthropic" }

func (m *claudeModel) Generate(ctx context.Context, system, user string, s *jsonschema.Schema) (string, error) {
	msg, err := m.cm.Generate(ctx,
		[]*schema.Message{schema.SystemMessage(system), schema.UserMessage(user)},
		claude.WithResponseFormat(&claude.ResponseFormat{Schema: s}),
	)
	if err != nil {
		return "", err
	}
	if msg == nil || msg.Content == "" {
		return "", fmt.Errorf("anthropic: empty response")
	}
	return msg.Content, nil
}
