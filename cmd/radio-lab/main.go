// radio-lab: Phase-0 component bench for Tần Số 42. Dev-only — reached
// through the LOCAL api-control-plane gateway (role:admin routes); audio
// artifacts live in MinIO and are served via presigned URLs.
package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	radiolabv1 "github.com/the-algovn/protos/gen/go/algovn/radiolab/v1"
	radiov1 "github.com/the-algovn/protos/gen/go/algovn/radio/v1"
	"github.com/the-algovn/radio-service/internal/artifact"
	"github.com/the-algovn/radio-service/internal/brain"
	"github.com/the-algovn/radio-service/internal/config"
	"github.com/the-algovn/radio-service/internal/ingest"
	"github.com/the-algovn/radio-service/internal/playlist"
	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/radioserver"
	"github.com/the-algovn/radio-service/internal/server"
	"github.com/the-algovn/radio-service/internal/spend"
	"github.com/the-algovn/radio-service/internal/voice"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := config.Load(); err != nil {
		logger.Error("config load failed", "err", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	lis, err := net.Listen("tcp", config.Get("LISTEN_ADDR", ":9290"))
	if err != nil {
		logger.Error("listen failed", "err", err)
		os.Exit(1)
	}
	gs := grpc.NewServer()
	dataDir := config.Get("LAB_DATA_DIR", "lab-data")
	tmpDir := filepath.Join(dataDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		logger.Error("mkdir tmp failed", "err", err)
		os.Exit(1)
	}

	// Postgres ledger
	pgURL := config.Get("PG_URL", "")
	if pgURL == "" {
		logger.Error("config", "err", "PG_URL is required")
		os.Exit(1)
	}
	pool, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		logger.Error("pg connect failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	ledger := spend.NewPGLedger(pool)
	lib := library.NewPGLibrary(pool)

	// MinIO artifact store
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
		logger.Error("minio init failed", "err", err)
		os.Exit(1)
	}
	// Ensure the bucket exists, tolerating a brief MinIO startup race.
	for i := 0; ; i++ {
		if err = store.EnsureBucket(ctx); err == nil {
			break
		}
		if i >= 15 {
			logger.Error("minio bucket ensure failed", "err", err)
			os.Exit(1)
		}
		time.Sleep(time.Second)
	}

	var voiceProv voice.Provider = voice.Fake{}
	voiceFake := true
	if k := config.Get("GOOGLE_TTS_API_KEY", ""); k != "" {
		voiceProv, voiceFake = voice.NewGoogle(k), false
	}
	models := map[string]brain.Model{"fake": brain.Fake{}}
	defaultModel := "fake"
	if k := config.Get("GEMINI_API_KEY", ""); k != "" {
		models["gemini"] = brain.NewGemini(k, config.Get("GEMINI_MODEL", "gemini-2.5-flash"))
		defaultModel = "gemini"
	}
	if k := config.Get("ANTHROPIC_API_KEY", ""); k != "" {
		models["anthropic"] = brain.NewAnthropic(k, config.Get("ANTHROPIC_MODEL", "claude-haiku-4-5-20251001"))
		if defaultModel == "fake" {
			defaultModel = "anthropic"
		}
	}
	srv := server.New(server.Deps{
		Ledger: ledger, Library: lib,
		Store: store, Voice: voiceProv, VoiceFake: voiceFake,
		Models: models, DefaultModel: defaultModel, PersonaDir: config.Get("PERSONA_DIR", "persona"),
		PersonaReadonly: config.GetBool("PERSONA_READONLY", false),
		FixturesDir:     config.Get("FIXTURES_DIR", "internal/callin/testdata/fixtures"),
		Ingest:          &ingest.Runner{}, TmpDir: tmpDir,
		Logger: logger,
	})
	radiolabv1.RegisterLabServiceServer(gs, srv)
	radiov1.RegisterRadioServiceServer(gs, radioserver.New(radioserver.Deps{
		Store:  playlist.NewPGStore(pool),
		Logger: logger,
	}))
	healthpb.RegisterHealthServer(gs, health.NewServer())
	reflection.Register(gs)

	go func() {
		<-ctx.Done()
		gs.GracefulStop()
	}()
	logger.Info("radio-lab listening", "grpc", ":9290")
	if err := gs.Serve(lis); err != nil {
		logger.Error("serve failed", "err", err)
		os.Exit(1)
	}
}
