// Package artifact stores lab outputs (voice takes, downloaded tracks,
// renders) as <id>.<ext> + <id>.json sidecar under one directory. IDs are
// generated, never user input — but Path still validates against traversal.
package artifact

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type Artifact struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind"`
	Label     string            `json:"label"`
	Ext       string            `json:"ext"`
	Bytes     int64             `json:"bytes"`
	CreatedAt time.Time         `json:"created_at"`
	Meta      map[string]string `json:"meta,omitempty"`
}

type Store struct{ Dir string }

var idRe = regexp.MustCompile(`^[a-z0-9-]+$`)

func (s Store) Save(kind, ext, label string, data []byte, meta map[string]string) (Artifact, error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return Artifact{}, err
	}
	suf := make([]byte, 4)
	_, _ = rand.Read(suf)
	a := Artifact{
		ID:   fmt.Sprintf("%s-%d-%s", kind, time.Now().UnixMilli(), hex.EncodeToString(suf)),
		Kind: kind, Label: label, Ext: strings.TrimPrefix(ext, "."),
		Bytes: int64(len(data)), CreatedAt: time.Now().UTC(), Meta: meta,
	}
	if err := os.WriteFile(filepath.Join(s.Dir, a.ID+"."+a.Ext), data, 0o644); err != nil {
		return Artifact{}, err
	}
	side, _ := json.MarshalIndent(a, "", "  ")
	if err := os.WriteFile(filepath.Join(s.Dir, a.ID+".json"), side, 0o644); err != nil {
		return Artifact{}, err
	}
	return a, nil
}

// SaveFile ingests an existing file (e.g. a yt-dlp download) by rename.
func (s Store) SaveFile(kind, srcPath, label string, meta map[string]string) (Artifact, error) {
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return Artifact{}, err
	}
	ext := strings.TrimPrefix(filepath.Ext(srcPath), ".")
	a, err := s.Save(kind, ext, label, data, meta)
	if err == nil {
		_ = os.Remove(srcPath)
	}
	return a, err
}

func (s Store) List() ([]Artifact, error) {
	entries, err := filepath.Glob(filepath.Join(s.Dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []Artifact
	for _, p := range entries {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var a Artifact
		if json.Unmarshal(b, &a) == nil && a.ID != "" {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s Store) Get(id string) (Artifact, error) {
	if !idRe.MatchString(id) {
		return Artifact{}, fmt.Errorf("invalid artifact id")
	}
	b, err := os.ReadFile(filepath.Join(s.Dir, id+".json"))
	if err != nil {
		return Artifact{}, err
	}
	var a Artifact
	return a, json.Unmarshal(b, &a)
}

func (s Store) Path(id string) (string, error) {
	a, err := s.Get(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.Dir, a.ID+"."+a.Ext), nil
}
