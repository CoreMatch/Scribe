package main

import (
	"crypto/tls"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/corematch/scribe/internal/config"
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

func TestNewMigratorConstructsProductionDependencies(t *testing.T) {
	cfg := config.Config{
		LegacyBaseURL:            "https://legacy.internal",
		ArmorialBaseURL:          "https://armorial.internal",
		RedisURL:                 "rediss://redis.internal:6379/0",
		MigrationServiceToken:    "service-token",
		MigrationTTL:             24 * time.Hour,
		MigrationMaxPackageBytes: 16 << 20,
		MigrationTimeout:         60 * time.Second,
		RequestTimeout:           15 * time.Second,
	}
	redisClient, err := openRedis(cfg.RedisURL)
	if err != nil {
		t.Fatal(err)
	}
	defer redisClient.Close()
	migrator, pending, err := newMigrator(cfg, redisClient, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || migrator == nil || pending == nil {
		t.Fatalf("newMigrator() = %v, %v, %v", migrator, pending, err)
	}
}
