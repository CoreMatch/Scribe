package main

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/corematch/scribe/internal/binding"
	"github.com/corematch/scribe/internal/config"
	"github.com/redis/go-redis/v9"
)

func TestOpenRedisRequiresVerifiedTLS(t *testing.T) {
	for _, raw := range []string{
		"redis://redis.internal:6379/0",
		"rediss://redis.internal:6379/0?skip_verify=true",
		"not-a-url",
	} {
		if client, err := openRedis(raw); err == nil {
			_ = client.Close()
			t.Fatalf("openRedis(%q) unexpectedly succeeded", raw)
		}
	}
	client, err := openRedis("rediss://scribe:secret@redis.internal:6379/1")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	options := client.Options()
	if options.TLSConfig == nil || options.TLSConfig.InsecureSkipVerify || options.TLSConfig.MinVersion < tls.VersionTLS12 {
		t.Fatalf("unexpected TLS options: %#v", options.TLSConfig)
	}
	if options.DB != 1 || options.Username != "scribe" {
		t.Fatalf("Redis URL options were not applied: %#v", options)
	}
}

func TestNewMigratorConstructsProductionDependenciesAndRecovery(t *testing.T) {
	cfg := config.Config{
		LegacyBaseURL:            "https://legacy.internal",
		ArmorialBaseURL:          "https://armorial.internal",
		MigrationServiceToken:    "service-token",
		MigrationTTL:             24 * time.Hour,
		MigrationMaxPackageBytes: 16 << 20,
		MigrationTimeout:         60 * time.Second,
		RequestTimeout:           15 * time.Second,
	}
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	defer redisClient.Close()
	store, err := binding.OpenSQLite(filepath.Join(t.TempDir(), "scribe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	migrator, pending, recovery, err := newMigrator(cfg, redisClient, store, log)
	if err != nil || migrator == nil || pending == nil || recovery == nil {
		t.Fatalf("newMigrator() = %v, %v, %v, %v", migrator, pending, recovery, err)
	}
	if err := recovery.Run(context.Background()); err != nil {
		t.Fatalf("recovery.Run() error = %v", err)
	}
}
