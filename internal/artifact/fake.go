package artifact

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// FakeStore keeps artifacts in memory for hermetic tests. PresignGet returns a
// deterministic stub URL; FetchToFile writes the bytes to a temp file.
type FakeStore struct {
	mu   sync.Mutex
	arts map[string]Artifact
	blob map[string][]byte
}

func NewFakeStore() *FakeStore {
	return &FakeStore{arts: map[string]Artifact{}, blob: map[string][]byte{}}
}

func (f *FakeStore) Save(_ context.Context, kind, ext, label string, data []byte, meta map[string]string) (Artifact, error) {
	a := newArtifact(kind, ext, label, int64(len(data)), meta)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.arts[a.ID] = a
	f.blob[a.ID] = append([]byte(nil), data...)
	return a, nil
}

func (f *FakeStore) SaveFile(ctx context.Context, kind, srcPath, label string, meta map[string]string) (Artifact, error) {
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return Artifact{}, err
	}
	a, err := f.Save(ctx, kind, filepath.Ext(srcPath), label, data, meta)
	if err == nil {
		_ = os.Remove(srcPath)
	}
	return a, err
}

func (f *FakeStore) Get(_ context.Context, id string) (Artifact, error) {
	if !idRe.MatchString(id) {
		return Artifact{}, fmt.Errorf("invalid artifact id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.arts[id]
	if !ok {
		return Artifact{}, fmt.Errorf("not found: %s", id)
	}
	return a, nil
}

func (f *FakeStore) List(_ context.Context, kind string) ([]Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Artifact
	for _, a := range f.arts {
		if kind == "" || a.Kind == kind {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (f *FakeStore) Delete(_ context.Context, id string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("invalid artifact id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.arts[id]; !ok {
		return fmt.Errorf("not found: %s", id)
	}
	delete(f.arts, id)
	delete(f.blob, id)
	return nil
}

func (f *FakeStore) PresignGet(_ context.Context, id string) (string, error) {
	if _, err := f.Get(context.Background(), id); err != nil {
		return "", err
	}
	return "https://fake.local/artifacts/" + id, nil
}

func (f *FakeStore) FetchToFile(ctx context.Context, id, dir string) (string, error) {
	a, err := f.Get(ctx, id)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	data := f.blob[id]
	f.mu.Unlock()
	p := filepath.Join(dir, a.ID+"."+a.Ext)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return "", err
	}
	return p, nil
}
