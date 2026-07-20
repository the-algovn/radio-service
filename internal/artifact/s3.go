package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Store stores each artifact as a blob (blobs/<id>) plus a JSON metadata
// object (meta/<id>.json). Two clients: one for internal PUT/GET/List against
// Endpoint, one whose signed URLs resolve against PublicEndpoint (the host the
// browser reaches). Locally the two endpoints are identical.
type S3Store struct {
	c       *minio.Client
	presign *minio.Client
	bucket  string
}

type S3Config struct {
	Endpoint, PublicEndpoint, AccessKey, SecretKey, Bucket string
	UseSSL, PublicUseSSL                                   bool
}

func NewS3Store(cfg S3Config) (*S3Store, error) {
	mk := func(ep string, secure bool) (*minio.Client, error) {
		return minio.New(ep, &minio.Options{
			Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
			Secure: secure,
		})
	}
	c, err := mk(cfg.Endpoint, cfg.UseSSL)
	if err != nil {
		return nil, err
	}
	pub := cfg.PublicEndpoint
	if pub == "" {
		pub = cfg.Endpoint
	}
	pc, err := mk(pub, cfg.PublicUseSSL)
	if err != nil {
		return nil, err
	}
	return &S3Store{c: c, presign: pc, bucket: cfg.Bucket}, nil
}

var s3ContentTypes = map[string]string{
	"mp3": "audio/mpeg", "wav": "audio/wav", "m4a": "audio/mp4",
	"webm": "audio/webm", "opus": "audio/ogg",
}

func blobKey(id string) string { return "blobs/" + id }
func metaKey(id string) string { return "meta/" + id + ".json" }

func (s *S3Store) putMeta(ctx context.Context, a Artifact) error {
	b, _ := json.Marshal(a)
	_, err := s.c.PutObject(ctx, s.bucket, metaKey(a.ID), bytes.NewReader(b), int64(len(b)),
		minio.PutObjectOptions{ContentType: "application/json"})
	return err
}

func (s *S3Store) Save(ctx context.Context, kind, ext, label string, data []byte, meta map[string]string) (Artifact, error) {
	a := newArtifact(kind, ext, label, int64(len(data)), meta)
	ct := s3ContentTypes[a.Ext]
	if _, err := s.c.PutObject(ctx, s.bucket, blobKey(a.ID), bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: ct}); err != nil {
		return Artifact{}, err
	}
	if err := s.putMeta(ctx, a); err != nil {
		return Artifact{}, err
	}
	return a, nil
}

func (s *S3Store) SaveFile(ctx context.Context, kind, srcPath, label string, meta map[string]string) (Artifact, error) {
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return Artifact{}, err
	}
	a, err := s.Save(ctx, kind, filepath.Ext(srcPath), label, data, meta)
	if err == nil {
		_ = os.Remove(srcPath)
	}
	return a, err
}

func (s *S3Store) Get(ctx context.Context, id string) (Artifact, error) {
	if !idRe.MatchString(id) {
		return Artifact{}, fmt.Errorf("invalid artifact id")
	}
	obj, err := s.c.GetObject(ctx, s.bucket, metaKey(id), minio.GetObjectOptions{})
	if err != nil {
		return Artifact{}, err
	}
	defer obj.Close()
	var a Artifact
	if err := json.NewDecoder(obj).Decode(&a); err != nil {
		return Artifact{}, fmt.Errorf("read meta %s: %w", id, err)
	}
	return a, nil
}

func (s *S3Store) List(ctx context.Context, kind string) ([]Artifact, error) {
	var out []Artifact
	for o := range s.c.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: "meta/", Recursive: true}) {
		if o.Err != nil {
			return nil, o.Err
		}
		id := strings.TrimSuffix(strings.TrimPrefix(o.Key, "meta/"), ".json")
		a, err := s.Get(ctx, id)
		if err != nil {
			continue
		}
		if kind == "" || a.Kind == kind {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s *S3Store) Delete(ctx context.Context, id string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("invalid artifact id")
	}
	if err := s.c.RemoveObject(ctx, s.bucket, blobKey(id), minio.RemoveObjectOptions{}); err != nil {
		return err
	}
	return s.c.RemoveObject(ctx, s.bucket, metaKey(id), minio.RemoveObjectOptions{})
}

func (s *S3Store) PresignGet(ctx context.Context, id string) (string, error) {
	a, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}
	reqParams := url.Values{}
	if ct := s3ContentTypes[a.Ext]; ct != "" {
		reqParams.Set("response-content-type", ct)
	}
	u, err := s.presign.PresignedGetObject(ctx, s.bucket, blobKey(id), time.Hour, reqParams)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (s *S3Store) FetchToFile(ctx context.Context, id, dir string) (string, error) {
	a, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, a.ID+"."+a.Ext)
	if err := s.c.FGetObject(ctx, s.bucket, blobKey(id), p, minio.GetObjectOptions{}); err != nil {
		return "", err
	}
	return p, nil
}

// EnsureBucket creates the bucket if absent. Called once at startup — not part
// of the Store interface (the fake has no bucket). Idempotent.
func (s *S3Store) EnsureBucket(ctx context.Context) error {
	ok, err := s.c.BucketExists(ctx, s.bucket)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	return s.c.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{})
}
