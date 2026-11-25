package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/corematch/scribe/internal/config"
	"github.com/corematch/scribe/internal/server"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(context.Background(), log); err != nil {
		log.Error("scribe stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	mux := httpMux()
	return server.New(cfg.ListenAddr, mux, cfg.ShutdownTimeout, log).Run(ctx)
}
