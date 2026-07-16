// Package voice synthesizes Vietnamese speech. Providers return encoded
// audio bytes + extension; artifact storage and cost accounting live in
// the server layer.
package voice

import (
	"context"
	"strings"
)

type Provider interface {
	Synthesize(ctx context.Context, text, voiceID string, rate float64) (data []byte, ext string, err error)
}

type Info struct{ ID, Label, Tier string }

// Voices is the audition list. VERIFY names against
//
//	curl "https://texttospeech.googleapis.com/v1/voices?languageCode=vi-VN&key=$GOOGLE_TTS_API_KEY"
//
// and edit before first real run — Google renames Chirp voices.
func Voices() []Info {
	return []Info{
		{ID: "vi-VN-Chirp3-HD-Aoede", Label: "Chirp3 HD — Aoede (nữ)", Tier: "chirp3-hd"},
		{ID: "vi-VN-Chirp3-HD-Kore", Label: "Chirp3 HD — Kore (nữ)", Tier: "chirp3-hd"},
		{ID: "vi-VN-Chirp3-HD-Leda", Label: "Chirp3 HD — Leda (nữ)", Tier: "chirp3-hd"},
		{ID: "vi-VN-Chirp3-HD-Zephyr", Label: "Chirp3 HD — Zephyr (nữ)", Tier: "chirp3-hd"},
		{ID: "vi-VN-Neural2-A", Label: "Neural2 A (nữ)", Tier: "neural2"},
		{ID: "vi-VN-Wavenet-A", Label: "Wavenet A (nữ)", Tier: "standard"},
	}
}

// CostUSD prices a synthesis. VERIFY against current Google TTS pricing
// (assumed: Chirp3-HD $30/1M chars, Neural2 $16/1M, Wavenet/Standard $4/1M).
func CostUSD(voiceID string, chars int) float64 {
	per1M := 4.0
	switch {
	case strings.Contains(voiceID, "Chirp3"):
		per1M = 30.0
	case strings.Contains(voiceID, "Neural2"):
		per1M = 16.0
	case voiceID == "fake":
		return 0
	}
	return per1M / 1e6 * float64(chars)
}
