// Package ttsclient adapts the shared TTS service to the narrow interface the
// director and lab server need.
package ttsclient

import (
	"context"

	ttsv1 "github.com/the-algovn/protos/gen/go/algovn/tts/v1"
)

// Speaker is what radio needs from text-to-speech. It replaces the deleted
// internal/voice.Provider, adding cost and provider to the return values --
// both now come from the service instead of a duplicated local price table.
type Speaker interface {
	Synthesize(ctx context.Context, text, voiceID string, rate float64) (data []byte, ext string, costUSD float64, provider string, err error)
}

type Client struct{ c ttsv1.TTSServiceClient }

func New(c ttsv1.TTSServiceClient) *Client { return &Client{c: c} }

func (c *Client) Synthesize(ctx context.Context, text, voiceID string, rate float64) ([]byte, string, float64, string, error) {
	resp, err := c.c.Synthesize(ctx, &ttsv1.SynthesizeRequest{
		Text: text, VoiceId: voiceID, SpeakingRate: rate,
		Format: ttsv1.AudioFormat_AUDIO_FORMAT_MP3, Label: "radio",
	})
	if err != nil {
		return nil, "", 0, "", err
	}
	return resp.GetAudio(), "mp3", resp.GetCostUsd(), resp.GetProvider(), nil
}
