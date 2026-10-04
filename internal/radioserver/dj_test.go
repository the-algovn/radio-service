package radioserver

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	radiov1 "github.com/the-algovn/protos/gen/go/algovn/radio/v1"

	"github.com/the-algovn/radio-service/internal/station"
)

func TestUpdateDJSettings(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	// Defaults surface on GetStation before any update.
	st, err := s.GetStation(ctx, &radiov1.GetStationRequest{})
	require.NoError(t, err)
	require.Equal(t, "", st.GetDj().GetVoiceId())
	require.Equal(t, 1.0, st.GetDj().GetSpeakingRate())
	require.Equal(t, int32(2), st.GetDj().GetBreakEvery())
	require.Equal(t, int32(60), st.GetDj().GetStationIdMin())
	require.Equal(t, int32(10), st.GetDj().GetMusingEveryMin())
	require.Equal(t, int32(1500), st.GetDj().GetMaxChars())

	// Update to a different voice (proves a real change off the default).
	resp, err := s.UpdateDJSettings(ctx, &radiov1.UpdateDJSettingsRequest{
		Settings: &radiov1.DJSettings{VoiceId: "voxcpm:v_bbbbbbbbbbbb", SpeakingRate: 1.2,
			BreakEvery: 3, StationIdMin: 0, MusingEveryMin: 25, MaxChars: 300},
	})
	require.NoError(t, err)
	require.Equal(t, "voxcpm:v_bbbbbbbbbbbb", resp.GetSettings().GetVoiceId())
	require.Equal(t, int32(0), resp.GetSettings().GetStationIdMin(), "0 = disabled is legal")
	require.Equal(t, int32(25), resp.GetSettings().GetMusingEveryMin())

	st, err = s.GetStation(ctx, &radiov1.GetStationRequest{})
	require.NoError(t, err)
	require.Equal(t, "voxcpm:v_bbbbbbbbbbbb", st.GetDj().GetVoiceId())
	require.Equal(t, 1.2, st.GetDj().GetSpeakingRate())
	require.Equal(t, int32(300), st.GetDj().GetMaxChars())
	require.Equal(t, int32(25), st.GetDj().GetMusingEveryMin(), "persisted, not just echoed")
}

func TestUpdateDJSettingsValidation(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	_, err := s.deps.Store.UpdateDJSettings(ctx, station.DJSettings{VoiceID: "voxcpm:v_aaaaaaaaaaaa", Rate: 1.0,
		BreakEvery: 1, StationIDMin: 60, MusingEveryMin: 10, MaxChars: 1024})
	require.NoError(t, err)
	base := func() *radiov1.DJSettings {
		return &radiov1.DJSettings{VoiceId: "voxcpm:v_aaaaaaaaaaaa", SpeakingRate: 1.0,
			BreakEvery: 1, StationIdMin: 60, MusingEveryMin: 10, MaxChars: 1024}
	}
	cases := []struct {
		name   string
		mutate func(*radiov1.DJSettings) // nil = omit settings entirely
	}{
		{"missing settings", nil},
		{"unknown voice", func(d *radiov1.DJSettings) { d.VoiceId = "vi-VN-Nope" }},
		{"fake is preview-only", func(d *radiov1.DJSettings) { d.VoiceId = "fake" }},
		{"rate too low", func(d *radiov1.DJSettings) { d.SpeakingRate = 0.5 }},
		{"rate too high", func(d *radiov1.DJSettings) { d.SpeakingRate = 1.5 }},
		{"rate absent (protojson zero)", func(d *radiov1.DJSettings) { d.SpeakingRate = 0 }},
		{"negative break_every", func(d *radiov1.DJSettings) { d.BreakEvery = -1 }},
		{"negative station_id_min", func(d *radiov1.DJSettings) { d.StationIdMin = -1 }},
		{"negative musing_every_min", func(d *radiov1.DJSettings) { d.MusingEveryMin = -1 }},
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
	require.Equal(t, "voxcpm:v_aaaaaaaaaaaa", st.GetDj().GetVoiceId())
}

func TestVoiceKnownComparesExactly(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	known, err := s.voiceKnown(ctx, "voxcpm:v_aaaaaaaaaaaa")
	require.NoError(t, err)
	require.True(t, known)
	for _, id := range []string{"", "v_aaaaaaaaaaaa", "vi-VN-Neural2-A", "google:vi-VN-Neural2-A"} {
		known, err = s.voiceKnown(ctx, id)
		require.NoError(t, err)
		require.False(t, known, id)
	}
}

// TestUpdateDJSettingsToleratesCatalogUnavailableWhenVoiceUnchanged guards
// against voiceKnown being called unconditionally: a down tts-service must
// not block edits to break_every, station_id_min, or max_chars -- including
// the one action an operator wants when TTS itself is broken (break_every:
// 0, to stop the DJ attempting breaks). Only an actual voice_id change may
// consult (and be blocked by) the catalog.
func TestUpdateDJSettingsToleratesCatalogUnavailableWhenVoiceUnchanged(t *testing.T) {
	s := newTestServerWithTTS(t, erroringTTS{})
	ctx := context.Background()
	_, err := s.deps.Store.UpdateDJSettings(ctx, station.DJSettings{VoiceID: "voxcpm:v_aaaaaaaaaaaa", Rate: 1.0,
		BreakEvery: 1, StationIDMin: 60, MusingEveryMin: 10, MaxChars: 1024})
	require.NoError(t, err)

	resp, err := s.UpdateDJSettings(ctx, &radiov1.UpdateDJSettingsRequest{
		Settings: &radiov1.DJSettings{VoiceId: "voxcpm:v_aaaaaaaaaaaa", SpeakingRate: 1.0,
			BreakEvery: 0, StationIdMin: 60, MaxChars: 1500},
	})
	require.NoError(t, err)
	require.Equal(t, int32(0), resp.GetSettings().GetBreakEvery())

	// Changing voice_id while the catalog is down must still fail Unavailable.
	_, err = s.UpdateDJSettings(ctx, &radiov1.UpdateDJSettingsRequest{
		Settings: &radiov1.DJSettings{VoiceId: "voxcpm:v_bbbbbbbbbbbb", SpeakingRate: 1.0,
			BreakEvery: 0, StationIdMin: 60, MaxChars: 1500},
	})
	require.Equal(t, codes.Unavailable, status.Code(err))
}

func TestUpdateDJSettingsEmptyVoiceMeansNoVoice(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	_, err := s.deps.Store.UpdateDJSettings(ctx, station.DJSettings{VoiceID: "voxcpm:v_aaaaaaaaaaaa", Rate: 1.0,
		BreakEvery: 1, StationIDMin: 60, MusingEveryMin: 10, MaxChars: 1024})
	require.NoError(t, err)
	_, err = s.UpdateDJSettings(ctx, &radiov1.UpdateDJSettingsRequest{Settings: &radiov1.DJSettings{
		SpeakingRate: 1.0, BreakEvery: 1, StationIdMin: 60, MusingEveryMin: 10, MaxChars: 1024}})
	require.NoError(t, err)
	st, err := s.deps.Store.GetStation(ctx)
	require.NoError(t, err)
	require.Equal(t, "", st.DJ.VoiceID)
}
