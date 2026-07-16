package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Anthropic struct {
	key, model, base string
	hc               *http.Client
}

func NewAnthropic(key, model string) *Anthropic {
	return &Anthropic{key: key, model: model, base: "https://api.anthropic.com", hc: &http.Client{Timeout: 25 * time.Second}}
}

func (a *Anthropic) Name() string { return a.model }

func (a *Anthropic) Generate(ctx context.Context, system, user string) (string, Usage, error) {
	body, _ := json.Marshal(map[string]any{
		"model": a.model, "max_tokens": 1024, "system": system,
		"messages": []map[string]string{{"role": "user", "content": user}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.key)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := a.hc.Do(req)
	if err != nil {
		return "", Usage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error struct{ Message string } }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return "", Usage{}, fmt.Errorf("anthropic %d: %s", resp.StatusCode, e.Error.Message)
	}
	var out struct {
		Content []struct{ Text string }
		Usage   struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", Usage{}, err
	}
	if len(out.Content) == 0 {
		return "", Usage{}, fmt.Errorf("anthropic: empty response")
	}
	return out.Content[0].Text, Usage{In: out.Usage.InputTokens, Out: out.Usage.OutputTokens}, nil
}
