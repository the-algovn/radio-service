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
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	radiolabv1 "github.com/the-algovn/protos/gen/go/algovn/radiolab/v1"
	"github.com/the-algovn/radio-service/internal/config"
)

type labServer struct {
	radiolabv1.UnimplementedLabServiceServer
}

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
	radiolabv1.RegisterLabServiceServer(gs, &labServer{})
	healthpb.RegisterHealthServer(gs, health.NewServer())
	reflection.Register(gs)

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("ok"))
		})
		_ = (&http.Server{Addr: ":9291", Handler: mux}).ListenAndServe()
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
