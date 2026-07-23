//go:build integration

package live

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/schedule"
	"github.com/the-algovn/radio-service/internal/station"
)

// makeTone writes an n-second 440Hz sine m4a for the pipeline to chew on.
// The fixture's fake loudness values are fine — linear loudnorm accepts any
// plausible measurements.
func makeTone(t *testing.T, dir, name string, seconds int) string {
	t.Helper()
	p := filepath.Join(dir, name+".m4a")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%d", seconds),
		"-c:a", "aac", p)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return p
}

func TestRealPipelineProducesLiveHLS(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	work := t.TempDir()
	tone := makeTone(t, work, "a", 4)

	lib := library.NewMemLibrary()
	ctx := context.Background()
	require.NoError(t, lib.Add(ctx, library.Track{
		YTID: "tone-a", Title: "Tone A", Channel: "test", DurationS: 4,
		ArtifactID: "art-a", InputI: -20, InputTP: -3, InputLRA: 5,
	}))
	st := station.NewMemStore()
	_, err := st.GoOnAir(ctx)
	require.NoError(t, err)

	f := NewFeeder(FeederDeps{
		Store: st, Requests: request.NewMemStore(), Library: lib, Sched: schedule.NewMemStore(),
		Log: NewMemAirLog(), Listeners: NewMemListeners(time.Now),
		Fetch:   func(_ context.Context, _, _ string) (string, error) { return tone, nil },
		Decoder: NewFFDecoder(), Encoder: NewFFEncoder(),
		Clock: RealClock(), Dir: work,
	})

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- f.RunSession(runCtx) }()

	// within ~15s (real-time pacing) a manifest with PDT + segments appears
	var manifest string
	require.Eventually(t, func() bool {
		dir := f.SessionDir()
		if dir == "" {
			return false
		}
		b, err := os.ReadFile(filepath.Join(dir, "live.m3u8"))
		if err != nil {
			return false
		}
		manifest = string(b)
		return strings.Contains(manifest, "#EXT-X-PROGRAM-DATE-TIME:") &&
			strings.Contains(manifest, "seg-0.ts")
	}, 20*time.Second, 250*time.Millisecond)

	require.Regexp(t, regexp.MustCompile(`#EXT-X-MEDIA-SEQUENCE:\d+`), manifest)
	seg := filepath.Join(f.SessionDir(), "seg-0.ts")
	fi, err := os.Stat(seg)
	require.NoError(t, err)
	require.Greater(t, fi.Size(), int64(1000)) // real encoded audio bytes

	cancel()
	require.NoError(t, <-done)
	require.Empty(t, f.SessionDir()) // dir cleaned up
}
