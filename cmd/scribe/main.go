package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/corematch/scribe/internal/binding"
	"github.com/corematch/scribe/internal/config"
	"github.com/corematch/scribe/internal/gateway"
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
	store, err := binding.OpenSQLite(cfg.SQLitePath)
	if err != nil {
		return err
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Error("binding store close failed", "error", err)
		}
	}()

	gateway, err := gateway.New(gateway.Options{
		PublicBaseURL:   cfg.PublicBaseURL,
		LegacyBaseURL:   cfg.LegacyBaseURL,
		ArmorialBaseURL: cfg.ArmorialBaseURL,
		RequestTimeout:  cfg.RequestTimeout,
		MaxRequestBody:  cfg.MaxRequestBody,
		Circuit:         circuitbreakerSettings(cfg),
	}, store, log)
	if err != nil {
		return err
	}
	return server.New(cfg.ListenAddr, httpMux(cfg, gateway), cfg.ShutdownTimeout, log).Run(ctx)
}
