package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Gemini struct {
	key, model, base string
	hc               *http.Client
}

func NewGemini(key, model string) *Gemini {
	return &Gemini{key: key, model: model, base: "https://generativelanguage.googleapis.com", hc: &http.Client{Timeout: 25 * time.Second}}
}

func (g *Gemini) Name() string { return g.model }

func (g *Gemini) Generate(ctx context.Context, system, user string) (string, Usage, error) {
	body, _ := json.Marshal(map[string]any{
		"system_instruction": map[string]any{"parts": []map[string]string{{"text": system}}},
		"contents":           []map[string]any{{"role": "user", "parts": []map[string]string{{"text": user}}}},
		"generationConfig":   map[string]any{"responseMimeType": "application/json"},
	})
	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent?key=%s", g.base, g.model, g.key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.hc.Do(req)
	if err != nil {
		return "", Usage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error struct{ Message string } }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return "", Usage{}, fmt.Errorf("gemini %d: %s", resp.StatusCode, e.Error.Message)
	}
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct{ Text string }
			}
		}
		UsageMetadata struct {
			PromptTokenCount     int
			CandidatesTokenCount int
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", Usage{}, err
	}
	if len(out.Candidates) == 0 || len(out.Candidates[0].Content.Parts) == 0 {
		return "", Usage{}, fmt.Errorf("gemini: empty response")
	}
	return out.Candidates[0].Content.Parts[0].Text,
		Usage{In: out.UsageMetadata.PromptTokenCount, Out: out.UsageMetadata.CandidatesTokenCount}, nil
}
