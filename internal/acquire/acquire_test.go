package acquire

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/the-algovn/radio-service/internal/artifact"
	"github.com/the-algovn/radio-service/internal/library"
)

type fakeArtifacts struct{ saved []string }

func (f *fakeArtifacts) Save(context.Context, string, string, string, []byte, map[string]string) (artifact.Artifact, error) {
	return artifact.Artifact{}, errors.New("unused")
}
func (f *fakeArtifacts) SaveFile(_ context.Context, kind, srcPath, label string, _ map[string]string) (artifact.Artifact, error) {
	f.saved = append(f.saved, srcPath)
	_ = os.Remove(srcPath) // mirror S3Store: consumes the file on success
	return artifact.Artifact{ID: "art-" + label, Kind: kind, Label: label}, nil
}
func (f *fakeArtifacts) Get(context.Context, string) (artifact.Artifact, error) {
	return artifact.Artifact{}, errors.New("unused")
}
func (f *fakeArtifacts) List(context.Context, string) ([]artifact.Artifact, error) { return nil, nil }
func (f *fakeArtifacts) Delete(context.Context, string) error                      { return nil }
func (f *fakeArtifacts) PresignGet(context.Context, string) (string, error)        { return "", nil }
func (f *fakeArtifacts) FetchToFile(context.Context, string, string) (string, error) {
	return "", errors.New("unused")
}

func newAcquirer(t *testing.T, lib library.Library, arts artifact.Store, downloadErr error) *Acquirer {
	t.Helper()
	return New(Deps{
		Download: func(_ context.Context, ytID, destDir string) (string, error) {
			if downloadErr != nil {
				return "", downloadErr
			}
			p := filepath.Join(destDir, ytID+".m4a")
			require.NoError(t, os.WriteFile(p, []byte("audio"), 0o644))
			return p, nil
		},
		Probe:    func(context.Context, string) (float64, error) { return 245.5, nil },
		Loudnorm: func(context.Context, string) (float64, float64, float64, error) { return -13.2, -1.1, 6.4, nil },
		Store:    arts, Library: lib, TmpDir: t.TempDir(),
	})
}

func TestAcquireDownloadsAndAddsLibraryRow(t *testing.T) {
	lib := library.NewMemLibrary()
	arts := &fakeArtifacts{}
	a := newAcquirer(t, lib, arts, nil)

	tr, cached, err := a.Acquire(context.Background(), "yt1", "Lạc Trôi", "Sơn Tùng M-TP - Topic")
	require.NoError(t, err)
	require.False(t, cached)
	require.Equal(t, "yt1", tr.YTID)
	require.Equal(t, "Lạc Trôi", tr.Title)
	require.Equal(t, "Sơn Tùng M-TP - Topic", tr.Channel)
	require.Equal(t, 245.5, tr.DurationS)
	require.Equal(t, -13.2, tr.InputI)
	require.Equal(t, "art-Lạc Trôi", tr.ArtifactID)
	require.Len(t, arts.saved, 1)

	got, found, err := lib.Get(context.Background(), "yt1")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, tr.ArtifactID, got.ArtifactID)
}

func TestAcquireCachedSkipsDownload(t *testing.T) {
	lib := library.NewMemLibrary()
	require.NoError(t, lib.Add(context.Background(), library.Track{
		YTID: "yt1", Title: "cached", ArtifactID: "art-old", DurationS: 100,
	}))
	a := newAcquirer(t, lib, &fakeArtifacts{}, errors.New("must not download"))
	tr, cached, err := a.Acquire(context.Background(), "yt1", "ignored", "ignored")
	require.NoError(t, err)
	require.True(t, cached)
	require.Equal(t, "cached", tr.Title)
}

func TestAcquireDownloadFailurePropagates(t *testing.T) {
	a := newAcquirer(t, library.NewMemLibrary(), &fakeArtifacts{}, errors.New("yt-dlp: 403"))
	_, _, err := a.Acquire(context.Background(), "yt1", "x", "y")
	require.ErrorContains(t, err, "yt-dlp: 403")
}

func TestAcquireBlankTitleFallsBackToYTID(t *testing.T) {
	lib := library.NewMemLibrary()
	a := newAcquirer(t, lib, &fakeArtifacts{}, nil)
	tr, _, err := a.Acquire(context.Background(), "yt9", "", "")
	require.NoError(t, err)
	require.Equal(t, "yt9", tr.Title)
}

func TestAcquireCappedRejectsProbedOverCap(t *testing.T) {
	lib := library.NewMemLibrary()
	arts := &fakeArtifacts{}
	a := New(Deps{
		Download: func(_ context.Context, ytID, destDir string) (string, error) {
			p := filepath.Join(destDir, ytID+".m4a")
			require.NoError(t, os.WriteFile(p, []byte("audio"), 0o644))
			return p, nil
		},
		Probe: func(context.Context, string) (float64, error) { return 36000, nil },
		Loudnorm: func(context.Context, string) (float64, float64, float64, error) {
			t.Fatal("loudnorm must not run once the duration cap is exceeded")
			return 0, 0, 0, nil
		},
		Store: arts, Library: lib, TmpDir: t.TempDir(), MaxDurationS: 600,
	})

	_, _, err := a.Acquire(context.Background(), "yt1", "x", "y")
	require.ErrorIs(t, err, ErrTooLong)
	require.Empty(t, arts.saved) // never persisted to the artifact store

	_, found, gerr := lib.Get(context.Background(), "yt1")
	require.NoError(t, gerr)
	require.False(t, found) // never persisted to the library
}

func TestAcquireUncappedAllowsProbedOverSixHundred(t *testing.T) {
	lib := library.NewMemLibrary()
	arts := &fakeArtifacts{}
	a := New(Deps{
		Download: func(_ context.Context, ytID, destDir string) (string, error) {
			p := filepath.Join(destDir, ytID+".m4a")
			require.NoError(t, os.WriteFile(p, []byte("audio"), 0o644))
			return p, nil
		},
		Probe:    func(context.Context, string) (float64, error) { return 36000, nil },
		Loudnorm: func(context.Context, string) (float64, float64, float64, error) { return -13.2, -1.1, 6.4, nil },
		Store:    arts, Library: lib, TmpDir: t.TempDir(), // MaxDurationS unset: uncapped, mirrors the lab bench
	})

	tr, _, err := a.Acquire(context.Background(), "yt1", "x", "y")
	require.NoError(t, err)
	require.Equal(t, 36000.0, tr.DurationS)
	require.Len(t, arts.saved, 1)
}
