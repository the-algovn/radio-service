// radio-lab-migrate applies the embedded goose migrations to PG_URL and exits.
// It runs as an Argo PreSync hook Job — never in the service process, never by
// hand. See the-algovn/specs ARCHITECTURE.md (Data → Schema) and the
// 2026-07-17 sqlc+goose migrations spec.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/the-algovn/gopkg/obs"
	"github.com/the-algovn/radio-service/internal/migrate"
)

// version is stamped at build time with -ldflags "-X main.version=<sha>".
var version = "dev"

func main() {
	down := flag.Bool("down", false, "reverse the most recently applied migration instead of applying pending ones")
	forceDestructive := flag.Bool("force-destructive", false, "with -down, bypass the guard that refuses to reverse migration 001 (DROPS ledger_line); only for a throwaway/dev database")
	flag.Parse()

	obsCfg, err := obs.ConfigFromEnv("radio-lab-migrate", version)
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

	url := os.Getenv("PG_URL")
	if url == "" {
		logger.Error("config", "err", "PG_URL is required")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *down {
		reversed, err := migrate.Down(ctx, url, *forceDestructive)
		if err != nil {
			logger.ErrorContext(ctx, "migrate down failed", "err", err)
			os.Exit(1)
		}
		logger.InfoContext(ctx, "migration reversed", "detail", reversed)
		return
	}

	applied, err := migrate.Up(ctx, url)
	if err != nil {
		logger.ErrorContext(ctx, "migrate failed", "err", err)
		os.Exit(1)
	}
	if len(applied) == 0 {
		logger.InfoContext(ctx, "no pending migrations")
		return
	}
	for _, line := range applied {
		logger.InfoContext(ctx, "migration applied", "detail", line)
	}
}
