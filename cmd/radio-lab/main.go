// radio-lab: Phase-0 component bench for Tần Số 42. Dev-only — reached
// through the LOCAL api-control-plane gateway (role:admin routes); audio
// artifacts live in MinIO and are served via presigned URLs.
package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/cloudwego/eino/callbacks"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/the-algovn/gopkg/obs"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	radiov1 "github.com/the-algovn/protos/gen/go/algovn/radio/v1"
	radiolabv1 "github.com/the-algovn/protos/gen/go/algovn/radiolab/v1"
	"github.com/the-algovn/radio-service/internal/acquire"
	"github.com/the-algovn/radio-service/internal/artifact"
	"github.com/the-algovn/radio-service/internal/audit"
	"github.com/the-algovn/radio-service/internal/brain"
	"github.com/the-algovn/radio-service/internal/config"
	"github.com/the-algovn/radio-service/internal/director"
	"github.com/the-algovn/radio-service/internal/ingest"
	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/programmer"
	"github.com/the-algovn/radio-service/internal/radioserver"
	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/schedule"
	"github.com/the-algovn/radio-service/internal/server"
	"github.com/the-algovn/radio-service/internal/spend"
	"github.com/the-algovn/radio-service/internal/station"
	"github.com/the-algovn/radio-service/internal/voice"
)

// version is stamped at build time with -ldflags "-X main.version=<sha>".
var version = "dev"

func main() {
	obsCfg, err := obs.ConfigFromEnv("radio-service", version)
	if err != nil {
		slog.Error("observability config", "err", err)
		os.Exit(1)
	}
	shutdownObs, err := obs.Setup(context.Background(), obsCfg)
	if err != nil {
		slog.Error("observability setup", "err", err)
		os.Exit(1)
	}
	logger := slog.Default()
	defer func() { _ = shutdownObs(context.Background()) }()

	if err := config.Load(); err != nil {
		logger.Error("config load failed", "err", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	lis, err := net.Listen("tcp", config.Get("LISTEN_ADDR", ":9290"))
	if err != nil {
		logger.ErrorContext(ctx, "listen failed", "err", err)
		os.Exit(1)
	}
	gs := grpc.NewServer(
		grpc.StatsHandler(obs.ServerHandler()),
		grpc.ChainUnaryInterceptor(obs.UnaryServerInterceptor()),
	)
	dataDir := config.Get("LAB_DATA_DIR", "lab-data")
	tmpDir := filepath.Join(dataDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		logger.ErrorContext(ctx, "mkdir tmp failed", "err", err)
		os.Exit(1)
	}

	// Postgres ledger
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
	ledger := spend.NewPGLedger(pool)
	lib := library.NewPGLibrary(pool)

	retDays, err := strconv.Atoi(config.Get("LLM_AUDIT_RETENTION_DAYS", "30"))
	if err != nil || retDays <= 0 {
		retDays = 30
	}
	auditStore := audit.NewPGStore(pool, time.Duration(retDays)*24*time.Hour)

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
		logger.ErrorContext(ctx, "minio init failed", "err", err)
		os.Exit(1)
	}
	// Ensure the bucket exists, tolerating a brief MinIO startup race.
	for i := 0; ; i++ {
		if err = store.EnsureBucket(ctx); err == nil {
			break
		}
		if i >= 15 {
			logger.ErrorContext(ctx, "minio bucket ensure failed", "err", err)
			os.Exit(1)
		}
		time.Sleep(time.Second)
	}

	var voiceProv voice.Provider = voice.Fake{}
	voiceFake := true
	if k := config.Get("GOOGLE_TTS_API_KEY", ""); k != "" {
		voiceProv, voiceFake = voice.NewGoogle(k), false
	}
	clk := live.RealClock()

	// One Eino callback records every LLM call to the audit store and appends
	// its cost to the ledger — for every provider and every call site.
	callbacks.AppendGlobalHandlers(audit.NewCallback(auditStore, ledger, clk, logger))

	models := map[string]brain.Model{"fake": brain.NewFake(brain.FakeScript)}
	defaultModel := "fake"
	if k := config.Get("GEMINI_API_KEY", ""); k != "" {
		m, err := brain.NewGemini(ctx, k, config.Get("GEMINI_MODEL", "gemini-2.5-flash"))
		if err != nil {
			logger.ErrorContext(ctx, "config", "err", "gemini model init failed", "detail", err)
			os.Exit(1)
		}
		models["gemini"] = m
		defaultModel = "gemini"
	}
	if k := config.Get("ANTHROPIC_API_KEY", ""); k != "" {
		m, err := brain.NewClaude(ctx, k, config.Get("ANTHROPIC_MODEL", "claude-haiku-4-5-20251001"))
		if err != nil {
			logger.ErrorContext(ctx, "config", "err", "anthropic model init failed", "detail", err)
			os.Exit(1)
		}
		models["anthropic"] = m
		if defaultModel == "fake" {
			defaultModel = "anthropic"
		}
	}
	runner := &ingest.Runner{}
	srv := server.New(server.Deps{
		Ledger: ledger, Audit: auditStore, Library: lib,
		Store: store, Voice: voiceProv, VoiceFake: voiceFake,
		Models: models, DefaultModel: defaultModel, PersonaDir: config.Get("PERSONA_DIR", "persona"),
		PersonaReadonly: config.GetBool("PERSONA_READONLY", false),
		FixturesDir:     config.Get("FIXTURES_DIR", "internal/callin/testdata/fixtures"),
		Ingest:          runner, TmpDir: tmpDir,
		Logger: logger,
	})
	radiolabv1.RegisterLabServiceServer(gs, srv)

	// Broadcast engine (Slice 2): feeder + HLS listener + optional Kafka feeds.
	// stationStore is hoisted so the RadioService registration below and
	// the feeder share the same store.
	stationStore := station.NewPGStore(pool)
	requests := request.NewPGStore(pool)
	sched := schedule.NewPGStore(pool)
	loc, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		logger.ErrorContext(ctx, "load station timezone failed", "err", err)
		os.Exit(1)
	}
	budget, err := strconv.ParseFloat(config.Get("RADIO_DAILY_BUDGET_USD", "1.0"), 64)
	if err != nil {
		logger.ErrorContext(ctx, "config", "err", "RADIO_DAILY_BUDGET_USD is not a number", "raw", config.Get("RADIO_DAILY_BUDGET_USD", "1.0"))
		os.Exit(1)
	}
	hlsDir := config.Get("HLS_DIR", filepath.Join(dataDir, "hls"))
	if err := os.MkdirAll(hlsDir, 0o755); err != nil {
		logger.ErrorContext(ctx, "mkdir hls failed", "err", err)
		os.Exit(1)
	}
	var producer live.Producer
	if brokers := config.Get("KAFKA_BROKERS", ""); brokers != "" {
		kp, err := live.NewKafkaProducer(strings.Split(brokers, ","))
		if err != nil {
			logger.ErrorContext(ctx, "kafka producer init failed", "err", err)
			os.Exit(1)
		}
		defer kp.Close()
		producer = kp
	} else {
		logger.WarnContext(ctx, "KAFKA_BROKERS not set; radio SSE feeds disabled")
	}
	// airLog/listeners are hoisted so the feeder and the RadioService
	// registration below share the same stores.
	airLog := live.NewPGAirLog(pool)
	listeners := live.NewPGListeners(pool)

	// DJ talk breaks (v2): director prepares clips ahead; the feeder airs
	// them via FeederDeps.Talk. Off unless RADIO_DJ_ENABLED=true.
	var talk live.TalkSource
	var dj *director.Director
	if config.GetBool("RADIO_DJ_ENABLED", false) {
		djDir := filepath.Join(dataDir, "dj")
		// Startup sweep: clips are regenerable; orphans from a crash are junk.
		_ = os.RemoveAll(djDir)
		if err := os.MkdirAll(djDir, 0o755); err != nil {
			logger.ErrorContext(ctx, "mkdir dj failed", "err", err)
			os.Exit(1)
		}
		// Voice/rate/cadence live on the station row (spec 2026-07-23) —
		// edited from the console, re-read by the director every tick.
		dj = director.New(director.Deps{
			Model: models[defaultModel], Voice: voiceProv, VoiceFake: voiceFake,
			Ledger: ledger, Station: stationStore, Listeners: listeners,
			AirLog: airLog, Requests: requests,
			PersonaDir:     config.Get("PERSONA_DIR", "persona"),
			StationIDsPath: filepath.Join(config.Get("PERSONA_DIR", "persona"), "station-ids.txt"),
			DataDir:        djDir, BudgetUSD: budget,
			Clock: live.RealClock(), Location: loc, Logger: logger,
		})
		talk = dj
		logger.InfoContext(ctx, "dj talk breaks enabled", "voice_fake", voiceFake, "model", defaultModel)
	}

	feeder := live.NewFeeder(live.FeederDeps{
		Store: stationStore, Requests: requests, Sched: sched, Library: lib,
		Log: airLog, Listeners: listeners,
		Fetch:   store.FetchToFile,
		Decoder: live.NewFFDecoder(), Encoder: live.NewFFEncoder(),
		Producer: producer, Clock: live.RealClock(), Dir: hlsDir, Logger: logger,
		Talk: talk,
	})
	engine := live.NewEngine(feeder, logger)
	go func() {
		if err := engine.Run(ctx); err != nil {
			logger.ErrorContext(ctx, "engine exited", "err", err)
		}
	}()
	hlsSrv := &http.Server{Addr: config.Get("HLS_ADDR", ":9291"), Handler: live.NewHLSHandler(feeder.SessionDir)}
	go func() {
		if err := hlsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.ErrorContext(ctx, "hls listener failed", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		_ = hlsSrv.Close()
	}()

	acquirer := acquire.New(acquire.Deps{
		Download: runner.Download, Probe: ingest.Probe, Loudnorm: ingest.Loudnorm, Cues: ingest.Cues,
		Store: store, Library: lib, TmpDir: tmpDir, Logger: logger, MaxDurationS: 600,
	})
	worker := acquire.NewWorker(acquire.WorkerDeps{
		Requests: requests, Sched: sched, Acquire: acquirer.Acquire, Producer: producer,
		Clock: live.RealClock(), Logger: logger,
	})
	go func() {
		if err := worker.Run(ctx); err != nil {
			logger.ErrorContext(ctx, "ingest worker exited", "err", err)
		}
	}()

	prog := programmer.New(programmer.Deps{
		Model: models[defaultModel], Fake: defaultModel == "fake",
		PersonaDir: config.Get("PERSONA_DIR", "persona"),
		Station:    stationStore, Requests: requests, Sched: sched, Library: lib,
		Log: airLog, Listeners: listeners, Search: runner, Ledger: ledger,
		BudgetUSD: budget, Producer: producer, Clock: live.RealClock(),
		Location: loc, Logger: logger,
	})
	go func() {
		if err := prog.Run(ctx); err != nil {
			logger.ErrorContext(ctx, "programmer exited", "err", err)
		}
	}()

	if dj != nil {
		go func() {
			if err := dj.Run(ctx); err != nil {
				logger.ErrorContext(ctx, "director exited", "err", err)
			}
		}()
	}

	radiov1.RegisterRadioServiceServer(gs, radioserver.New(radioserver.Deps{
		Store:     stationStore,
		Log:       airLog,
		Listeners: listeners,
		Notifier:  engine,
		Logger:    logger,
		Requests:  requests,
		Sched:     sched,
		Library:   lib,
		Producer:  producer,
		Search:    runner,
		Location:  loc,
		Skipper:   feeder,
		Ledger:    ledger,
		BudgetUSD: budget,
	}))
	healthpb.RegisterHealthServer(gs, health.NewServer())
	reflection.Register(gs)

	go func() {
		<-ctx.Done()
		gs.GracefulStop()
	}()
	logger.InfoContext(ctx, "radio-lab listening", "grpc", ":9290")
	if err := gs.Serve(lis); err != nil {
		logger.ErrorContext(ctx, "serve failed", "err", err)
		os.Exit(1)
	}
}
