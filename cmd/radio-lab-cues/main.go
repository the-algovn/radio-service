// radio-lab-cues backfills tail cue measurements for library tracks ingested
// before migration 00012.
//
// Resumable by construction: it only ever selects rows still carrying the
// unmeasured sentinel, so an interrupted run is simply re-run, never
// repaired. It is NOT a deploy prerequisite — an unmeasured track reads as
// "no overlap budget", which is a butt-join, which is what the station does
// today.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/the-algovn/radio-service/internal/artifact"
	"github.com/the-algovn/radio-service/internal/config"
	"github.com/the-algovn/radio-service/internal/ingest"
	"github.com/the-algovn/radio-service/internal/library"
)

func main() {
	batch := flag.Int("batch", 50, "tracks to claim per round")
	dryRun := flag.Bool("dry-run", false, "list what would be measured and exit without writing")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.Default()

	pgURL := config.Get("PG_URL", "")
	if pgURL == "" {
		logger.ErrorContext(ctx, "config", "err", "PG_URL is required")
		os.Exit(1)
	}
	pool, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		logger.ErrorContext(ctx, "pg connect failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	lib := library.NewPGLibrary(pool)

	// These env keys and defaults intentionally mirror cmd/radio-lab, so the
	// backfill reads the same bucket and database as the service that wrote
	// the artifacts it is measuring.
	store, err := artifact.NewS3Store(artifact.S3Config{
		Endpoint:       config.Get("MINIO_ENDPOINT", "localhost:9000"),
		PublicEndpoint: config.Get("MINIO_PUBLIC_ENDPOINT", config.Get("MINIO_ENDPOINT", "localhost:9000")),
		AccessKey:      config.Get("MINIO_ACCESS_KEY", ""),
		SecretKey:      config.Get("MINIO_SECRET_KEY", ""),
		Bucket:         config.Get("MINIO_BUCKET", "radio-lab"),
		UseSSL:         config.GetBool("MINIO_USE_SSL", false),
		PublicUseSSL:   config.GetBool("MINIO_PUBLIC_USE_SSL", config.GetBool("MINIO_USE_SSL", false)),
	})
	if err != nil {
		logger.ErrorContext(ctx, "minio init failed", "err", err)
		os.Exit(1)
	}

	// alreadyFailed remembers, for the life of this invocation only, which
	// yt_ids have already failed measureOne. A failure leaves the DB
	// sentinel unchanged, so MissingCues keeps re-selecting a broken track
	// on every later round; without this filter one bad artifact gets
	// re-fetched from MinIO and re-decoded by ffmpeg again and again, and
	// the final tally counts the same track's repeats instead of distinct
	// failures. Deliberately not persisted: a transient failure (a MinIO
	// blip, say) deserves another chance on the *next* run, and the DB
	// sentinel already makes that safe.
	alreadyFailed := map[string]bool{}

	var done, failed int
	start := time.Now()
	for {
		if ctx.Err() != nil {
			logger.InfoContext(ctx, "cancelled", "measured", done, "failed", failed)
			break
		}
		tracks, err := lib.MissingCues(ctx, *batch)
		if err != nil {
			logger.ErrorContext(ctx, "missing-cues query failed", "err", err)
			os.Exit(1)
		}
		if len(tracks) == 0 {
			break
		}
		if *dryRun {
			for _, tr := range tracks {
				logger.InfoContext(ctx, "would measure", "yt_id", tr.YTID, "title", tr.Title)
			}
			logger.InfoContext(ctx, "dry run: stopping after one round", "listed", len(tracks))
			break
		}

		pending := make([]library.Track, 0, len(tracks))
		for _, tr := range tracks {
			if !alreadyFailed[tr.YTID] {
				pending = append(pending, tr)
			}
		}
		if len(pending) == 0 {
			// Every track MissingCues just returned already failed earlier
			// in this run. The sentinel can't tell "never attempted" from
			// "failed once" apart, so without this check we would query the
			// same known-bad rows forever.
			logger.WarnContext(ctx, "every track in this round already failed earlier in the run; stopping rather than spinning",
				"measured", done, "failed", failed)
			break
		}

		// A round in which every pending track fails would otherwise cost
		// one more no-op round-trip before the alreadyFailed check above
		// catches it next time; stop here instead.
		progressed := false
		for _, tr := range pending {
			if ctx.Err() != nil {
				break
			}
			if measureOne(ctx, logger, lib, store, tr) {
				done++
				progressed = true
			} else {
				failed++
				alreadyFailed[tr.YTID] = true
			}
		}
		if ctx.Err() != nil {
			// A SIGTERM landing mid-round can leave progressed false even
			// though nothing here was actually a bad artifact — the round
			// just never got the chance to finish. That is a clean
			// shutdown, not a stuck round; label it as such rather than as
			// "a full round failed".
			logger.InfoContext(ctx, "cancelled", "measured", done, "failed", failed)
			break
		}
		if !progressed {
			logger.WarnContext(ctx, "a full round failed; stopping rather than spinning",
				"measured", done, "failed", failed)
			break
		}
	}
	logger.InfoContext(ctx, "backfill finished: measured/failed are distinct tracks, not attempts",
		"measured", done, "failed", failed, "elapsed", time.Since(start).String())
}

// measureOne fetches, measures and records one track. It returns false on any
// failure, having logged it — one unreadable artifact must never strand the
// rest of the library, so this is the only error policy in the loop.
func measureOne(ctx context.Context, logger *slog.Logger, lib library.Library,
	store artifact.Store, tr library.Track) bool {

	// The defer below is scoped to this function, not the outer loop over the
	// whole library: main calls measureOne once per track, so exactly one
	// artifact is on disk at a time instead of every artifact accumulating
	// until main returned.
	dir, err := os.MkdirTemp("", "cues-*")
	if err != nil {
		logger.ErrorContext(ctx, "tmp dir failed", "yt_id", tr.YTID, "err", err)
		return false
	}
	defer os.RemoveAll(dir) // scoped to THIS function, one track at a time

	path, err := store.FetchToFile(ctx, tr.ArtifactID, dir)
	if err != nil {
		logger.ErrorContext(ctx, "fetch failed", "yt_id", tr.YTID, "err", err)
		return false
	}
	sil, dec, err := ingest.Cues(ctx, path)
	if err != nil {
		logger.ErrorContext(ctx, "measure failed", "yt_id", tr.YTID, "err", err)
		return false
	}
	if err := lib.SetCues(ctx, tr.YTID, sil, dec); err != nil {
		logger.ErrorContext(ctx, "write failed", "yt_id", tr.YTID, "err", err)
		return false
	}
	logger.InfoContext(ctx, "measured", "yt_id", tr.YTID,
		"tail_silence_s", sil, "tail_decay_s", dec)
	return true
}
