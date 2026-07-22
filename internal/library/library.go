// Package library is radio-lab's persistent, deduped music cache. It mirrors
// internal/spend's layout: an interface, PGLibrary (prod/local) backed by
// Postgres via sqlc, and MemLibrary for hermetic tests. DownloadTrack looks
// here before running yt-dlp/ffprobe/loudnorm again for a yt_id already
// fetched.
package library

import (
	"context"
	"time"
)

type Track struct {
	YTID, Title, Channel, ArtifactID     string
	DurationS, InputI, InputTP, InputLRA float64
	AddedAt                              time.Time
}

// Library stores and looks up cached tracks by yt_id. Implementations:
// PGLibrary (prod/local), MemLibrary (tests).
type Library interface {
	// Get looks up a track by yt_id. found is false (err nil) when absent.
	Get(ctx context.Context, ytID string) (Track, bool, error)
	// Add stores t. A yt_id already present is left untouched (dedup).
	Add(ctx context.Context, t Track) error
	// List returns tracks whose title or channel contains query
	// (case-insensitive substring), newest first. limit<=0 defaults to 50.
	List(ctx context.Context, query string, limit, offset int) ([]Track, error)
	// Count returns the number of tracks matching query (same filter as List).
	Count(ctx context.Context, query string) (int64, error)
	// Delete removes a track by yt_id and returns its artifact_id so the
	// caller can delete the MinIO blob. found is false (err nil) when absent.
	Delete(ctx context.Context, ytID string) (artifactID string, found bool, err error)
	// AllIDs returns every yt_id in the library, sorted ascending — the
	// shuffle fallback and the programmer's brief sample read this.
	AllIDs(ctx context.Context) ([]string, error)
}
