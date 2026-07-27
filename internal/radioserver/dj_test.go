package radioserver

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	radiov1 "github.com/the-algovn/protos/gen/go/algovn/radio/v1"
)

func TestUpdateDJSettings(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	// Defaults surface on GetStation before any update.
	st, err := s.GetStation(ctx, &radiov1.GetStationRequest{})
	require.NoError(t, err)
	require.Equal(t, "vi-VN-Neural2-A", st.GetDj().GetVoiceId())
	require.Equal(t, 1.0, st.GetDj().GetSpeakingRate())
	require.Equal(t, int32(1), st.GetDj().GetBreakEvery())
	require.Equal(t, int32(60), st.GetDj().GetStationIdMin())
	require.Equal(t, int32(1024), st.GetDj().GetMaxChars())

	// Update to a different voice (proves a real change off the default).
	resp, err := s.UpdateDJSettings(ctx, &radiov1.UpdateDJSettingsRequest{
		Settings: &radiov1.DJSettings{VoiceId: "vi-VN-Chirp3-HD-Aoede", SpeakingRate: 1.2,
			BreakEvery: 3, StationIdMin: 0, MaxChars: 300},
	})
	require.NoError(t, err)
	require.Equal(t, "vi-VN-Chirp3-HD-Aoede", resp.GetSettings().GetVoiceId())
	require.Equal(t, int32(0), resp.GetSettings().GetStationIdMin(), "0 = disabled is legal")

	st, err = s.GetStation(ctx, &radiov1.GetStationRequest{})
	require.NoError(t, err)
	require.Equal(t, "vi-VN-Chirp3-HD-Aoede", st.GetDj().GetVoiceId())
	require.Equal(t, 1.2, st.GetDj().GetSpeakingRate())
	require.Equal(t, int32(300), st.GetDj().GetMaxChars())
}

func TestUpdateDJSettingsValidation(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	base := func() *radiov1.DJSettings {
		return &radiov1.DJSettings{VoiceId: "vi-VN-Neural2-A", SpeakingRate: 1.0,
			BreakEvery: 1, StationIdMin: 60, MaxChars: 1024}
	}
	cases := []struct {
		name   string
		mutate func(*radiov1.DJSettings) // nil = omit settings entirely
	}{
		{"missing settings", nil},
		{"unknown voice", func(d *radiov1.DJSettings) { d.VoiceId = "vi-VN-Nope" }},
		{"empty voice", func(d *radiov1.DJSettings) { d.VoiceId = "" }},
		{"fake is preview-only", func(d *radiov1.DJSettings) { d.VoiceId = "fake" }},
		{"rate too low", func(d *radiov1.DJSettings) { d.SpeakingRate = 0.5 }},
		{"rate too high", func(d *radiov1.DJSettings) { d.SpeakingRate = 1.5 }},
		{"rate absent (protojson zero)", func(d *radiov1.DJSettings) { d.SpeakingRate = 0 }},
		{"negative break_every", func(d *radiov1.DJSettings) { d.BreakEvery = -1 }},
		{"negative station_id_min", func(d *radiov1.DJSettings) { d.StationIdMin = -1 }},
		{"max_chars too small", func(d *radiov1.DJSettings) { d.MaxChars = 10 }},
		{"max_chars too large", func(d *radiov1.DJSettings) { d.MaxChars = 5000 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &radiov1.UpdateDJSettingsRequest{}
			if tc.mutate != nil {
				d := base()
				tc.mutate(d)
				req.Settings = d
			}
			_, err := s.UpdateDJSettings(ctx, req)
			require.Equal(t, codes.InvalidArgument, status.Code(err), "got err: %v", err)
		})
	}

	// Rejected updates must not have touched the stored settings.
	st, err := s.GetStation(ctx, &radiov1.GetStationRequest{})
	require.NoError(t, err)
	require.Equal(t, "vi-VN-Neural2-A", st.GetDj().GetVoiceId())
}
