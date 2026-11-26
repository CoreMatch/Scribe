package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"os"

	"github.com/corematch/scribe/internal/binding"
	"github.com/corematch/scribe/internal/config"
	"github.com/corematch/scribe/internal/gateway"
	"github.com/corematch/scribe/internal/migration"
	"github.com/corematch/scribe/internal/profile"
	"github.com/corematch/scribe/internal/server"
	"github.com/corematch/scribe/internal/texture"
	"github.com/redis/go-redis/v9"
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
	redisClient, err := openRedis(cfg.RedisURL)
	if err != nil {
		return err
	}
	defer func() {
		if err := redisClient.Close(); err != nil {
			log.Error("migration Redis close failed")
		}
	}()
	pingCtx, cancelPing := context.WithTimeout(ctx, cfg.RequestTimeout)
	err = redisClient.Ping(pingCtx).Err()
	cancelPing()
	if err != nil {
		return errors.New("migration Redis is unavailable")
	}
	store, err := binding.OpenSQLite(cfg.SQLitePath, cfg.TokenBindingTTL)
	if err != nil {
		return err
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Error("binding store close failed", "error", err)
		}
	}()

	migrator, migrationPending, recovery, err := newMigrator(cfg, redisClient, store, log)
	if err != nil {
		return err
	}
	if err := recovery.Run(ctx); err != nil {
		return err
	}
	gateway, err := gateway.New(gateway.Options{
		PublicBaseURL:    cfg.PublicBaseURL,
		LegacyBaseURL:    cfg.LegacyBaseURL,
		ArmorialBaseURL:  cfg.ArmorialBaseURL,
		RequestTimeout:   cfg.RequestTimeout,
		MaxRequestBody:   cfg.MaxRequestBody,
		Circuit:          circuitbreakerSettings(cfg),
		Migrator:         migrator,
		MigrationPending: migrationPending,
	}, store, log)
	if err != nil {
		return err
	}
	ready := func() error {
		checkCtx, cancel := context.WithTimeout(context.Background(), cfg.RequestTimeout)
		defer cancel()
		return checkReady(checkCtx, redisClient, store)
	}
	return server.New(cfg.ListenAddr, httpMux(cfg, gateway, ready), cfg.ShutdownTimeout, cfg.WriteTimeout, log).Run(ctx)
}

func checkReady(ctx context.Context, redisClient redis.UniversalClient, store *binding.SQLiteStore) error {
	if err := redisClient.Ping(ctx).Err(); err != nil {
		return errors.New("migration Redis is unavailable")
	}
	if err := store.Ping(ctx); err != nil {
		return errors.New("binding store is unavailable")
	}
	return nil
}

func openRedis(rawURL string) (*redis.Client, error) {
	options, err := redis.ParseURL(rawURL)
	if err != nil || options.TLSConfig == nil || options.TLSConfig.InsecureSkipVerify {
		return nil, errors.New("invalid Redis URL or TLS configuration")
	}
	if options.TLSConfig.MinVersion < tls.VersionTLS12 {
		options.TLSConfig.MinVersion = tls.VersionTLS12
	}
	return redis.NewClient(options), nil
}

func newMigrator(cfg config.Config, redisClient redis.UniversalClient, store *binding.SQLiteStore, log *slog.Logger) (migration.Runner, migration.PendingChecker, *migration.Recovery, error) {
	profiles, err := profile.New(cfg.LegacyBaseURL, profile.Options{Timeout: cfg.RequestTimeout})
	if err != nil {
		return nil, nil, nil, err
	}
	downloader, err := texture.New(texture.Options{Timeout: cfg.RequestTimeout})
	if err != nil {
		return nil, nil, nil, err
	}
	snapshotter, err := migration.New(profiles, downloader, migration.Options{})
	if err != nil {
		return nil, nil, nil, err
	}
	staging, err := migration.NewRedisStore(redisClient, migration.RedisOptions{
		TTL:             cfg.MigrationTTL,
		MaxPackageBytes: cfg.MigrationMaxPackageBytes,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	committer, err := migration.NewClient(cfg.ArmorialBaseURL, cfg.MigrationServiceToken, migration.ClientOptions{
		Timeout: cfg.MigrationTimeout,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	service, err := migration.NewService(snapshotter, staging, committer, log)
	if err != nil {
		return nil, nil, nil, err
	}
	recovery, err := migration.NewRecovery(staging, committer, store, log)
	if err != nil {
		return nil, nil, nil, err
	}
	return service, staging, recovery, nil
}
