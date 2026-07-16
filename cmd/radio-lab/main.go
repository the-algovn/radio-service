// radio-lab: Phase-0 component bench for Tần Số 42. Dev-only — reached
// through the LOCAL api-control-plane gateway (role:admin routes); audio
// artifacts are served directly on :9291 (bytes never ride the gateway).
package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	radiolabv1 "github.com/the-algovn/protos/gen/go/algovn/radiolab/v1"
	"github.com/the-algovn/radio-service/internal/artifact"
	"github.com/the-algovn/radio-service/internal/config"
	"github.com/the-algovn/radio-service/internal/server"
	"github.com/the-algovn/radio-service/internal/spend"
	"github.com/the-algovn/radio-service/internal/voice"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	_ = config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	lis, err := net.Listen("tcp", ":9290")
	if err != nil {
		logger.Error("listen failed", "err", err)
		os.Exit(1)
	}
	gs := grpc.NewServer()
	dataDir := config.Get("LAB_DATA_DIR", "lab-data")
	store := &artifact.Store{Dir: filepath.Join(dataDir, "artifacts")}
	var voiceProv voice.Provider = voice.Fake{}
	voiceFake := true
	if k := config.Get("GOOGLE_TTS_API_KEY", ""); k != "" {
		voiceProv, voiceFake = voice.NewGoogle(k), false
	}
	srv := server.New(server.Deps{
		Ledger: spend.NewLedger(filepath.Join(dataDir, "ledger.jsonl")),
		Store:  store, Voice: voiceProv, VoiceFake: voiceFake,
	})
	radiolabv1.RegisterLabServiceServer(gs, srv)
	healthpb.RegisterHealthServer(gs, health.NewServer())
	reflection.Register(gs)

	go func() {
		_ = (&http.Server{Addr: ":9291", Handler: artifact.Handler(*store)}).ListenAndServe()
	}()
	go func() {
		<-ctx.Done()
		gs.GracefulStop()
	}()
	logger.Info("radio-lab listening", "grpc", ":9290", "http", ":9291")
	if err := gs.Serve(lis); err != nil {
		logger.Error("serve failed", "err", err)
		os.Exit(1)
	}
}
