// Package acquire downloads and normalizes tracks into the library — the
// one pipeline behind LabService.DownloadTrack and the request ingest
// worker (spec §5). Steps are injected funcs so tests never exec
// yt-dlp/ffmpeg.
package acquire

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/the-algovn/radio-service/internal/artifact"
	"github.com/the-algovn/radio-service/internal/ingest"
	"github.com/the-algovn/radio-service/internal/library"
)

// ErrTooLong wraps the error Acquire returns when the probed duration
// exceeds MaxDurationS.
var ErrTooLong = errors.New("track exceeds the duration cap")

// ErrNotMusic wraps the error Acquire returns when the completed extraction
// shows the track is not a song. The bytes are already downloaded by then —
// that is the price of a signal that costs no extra network request.
var ErrNotMusic = errors.New("track is not music")

type Deps struct {
	Download func(ctx context.Context, ytID, destDir string) (string, ingest.Meta, error)
	Probe    func(ctx context.Context, path string) (float64, error)
	Loudnorm func(ctx context.Context, path string) (i, tp, lra float64, err error)
	// Cues measures the track's overlap budget. Injected like Probe and
	// Loudnorm so tests never exec ffmpeg. A nil Cues, or an error from it,
	// leaves the track UNMEASURED (-1) rather than failing the acquire —
	// a track that cannot be cue-measured must still be airable, because
	// unmeasured simply means the later crossfade butt-joins it.
	Cues    func(ctx context.Context, path string) (tailSilenceS, tailDecayS float64, err error)
	Store   artifact.Store
	Library library.Library
	TmpDir  string
	Logger  *slog.Logger
	// MaxDurationS rejects a probed track longer than this many seconds
	// before it is normalized/stored/added to the library. 0 = uncapped
	// (the lab bench's DownloadTrack stays uncapped by never setting this).
	MaxDurationS float64
}

type Acquirer struct{ d Deps }

func New(d Deps) *Acquirer {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Acquirer{d: d}
}

// Acquire returns the library track for ytID, downloading + normalizing +
// storing it when absent. cached=true when the library already had it.
func (a *Acquirer) Acquire(ctx context.Context, ytID, title, channel string) (library.Track, bool, error) {
	if tr, found, err := a.d.Library.Get(ctx, ytID); err != nil {
		return library.Track{}, false, err
	} else if found {
		return tr, true, nil
	}
	tmp, err := os.MkdirTemp(a.d.TmpDir, "dl-*")
	if err != nil {
		return library.Track{}, false, fmt.Errorf("tmp: %w", err)
	}
	defer os.RemoveAll(tmp)
	p, meta, err := a.d.Download(ctx, ytID, tmp)
	if err != nil {
		return library.Track{}, false, fmt.Errorf("download: %w", err)
	}
	dur, err := a.d.Probe(ctx, p)
	if err != nil {
		return library.Track{}, false, fmt.Errorf("probe: %w", err)
	}
	if a.d.MaxDurationS > 0 && dur > a.d.MaxDurationS {
		return library.Track{}, false, fmt.Errorf("probed %.0fs: %w", dur, ErrTooLong)
	}
	i, tp, lra, err := a.d.Loudnorm(ctx, p)
	if err != nil {
		return library.Track{}, false, fmt.Errorf("loudnorm: %w", err)
	}
	// The extraction that resolved -f bestaudio already told us what this is.
	// Reject before the blob is stored: the bytes are spent either way, but
	// storage and a library row are not.
	if notMusic, why := meta.NotMusic(); notMusic {
		return library.Track{}, false, fmt.Errorf("%s: %w", why, ErrNotMusic)
	}
	// Unlike Probe and Loudnorm above, whose values the feeder cannot air
	// without, a cue is an optimisation: its absence just costs a hard cut
	// instead of a crossfade, so a Cues failure must not fail the acquire.
	tailSilenceS, tailDecayS := -1.0, -1.0
	if a.d.Cues != nil {
		if s, d, cerr := a.d.Cues(ctx, p); cerr != nil {
			a.d.Logger.WarnContext(ctx, "cue measurement failed; track stays unmeasured",
				"yt_id", ytID, "err", cerr)
		} else {
			tailSilenceS, tailDecayS = s, d
		}
	}
	label := title
	if label == "" {
		label = ytID
	}
	art, err := a.d.Store.SaveFile(ctx, "track", p, label, map[string]string{
		"yt_id": ytID, "duration_s": fmt.Sprintf("%.1f", dur), "input_i": fmt.Sprintf("%.1f", i),
	})
	if err != nil {
		return library.Track{}, false, fmt.Errorf("store: %w", err)
	}
	tr := library.Track{
		YTID: ytID, Title: label, Channel: channel, DurationS: dur,
		ArtifactID: art.ID, InputI: i, InputTP: tp, InputLRA: lra,
		TailSilenceS: tailSilenceS, TailDecayS: tailDecayS,
	}
	// Unlike the old lab RPC, an Add failure here is an error: the worker's
	// track MUST reach the library or the request can never air.
	if err := a.d.Library.Add(ctx, tr); err != nil {
		return library.Track{}, false, fmt.Errorf("library add: %w", err)
	}
	return tr, false, nil
}
