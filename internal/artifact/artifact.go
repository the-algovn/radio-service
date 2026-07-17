// Package artifact stores lab outputs (voice takes, downloaded tracks, renders)
// in object storage. Store is an interface: S3Store (minio) for real use,
// FakeStore for hermetic tests. IDs are generated (kind-millis-hex), never user
// input, but still validated on read.
package artifact

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
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

// Store persists artifacts and hands out presigned GET URLs and local copies.
type Store interface {
	Save(ctx context.Context, kind, ext, label string, data []byte, meta map[string]string) (Artifact, error)
	SaveFile(ctx context.Context, kind, srcPath, label string, meta map[string]string) (Artifact, error)
	Get(ctx context.Context, id string) (Artifact, error)
	List(ctx context.Context, kind string) ([]Artifact, error)       // kind=="" means all
	PresignGet(ctx context.Context, id string) (string, error)       // time-limited URL for the browser
	FetchToFile(ctx context.Context, id, dir string) (string, error) // local copy for ffmpeg
}

var idRe = regexp.MustCompile(`^[a-z0-9-]+$`)

func newID(kind string) string {
	suf := make([]byte, 4)
	_, _ = rand.Read(suf)
	return fmt.Sprintf("%s-%d-%s", kind, time.Now().UnixMilli(), hex.EncodeToString(suf))
}

func newArtifact(kind, ext, label string, size int64, meta map[string]string) Artifact {
	return Artifact{
		ID: newID(kind), Kind: kind, Label: label, Ext: strings.TrimPrefix(ext, "."),
		Bytes: size, CreatedAt: time.Now().UTC(), Meta: meta,
	}
}
