package main

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/corematch/scribe/internal/binding"
	"github.com/corematch/scribe/internal/config"
	"github.com/corematch/scribe/internal/gateway"
	"github.com/corematch/scribe/internal/metrics"
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

func TestCheckReadyRequiresRedisAndSQLite(t *testing.T) {
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	defer redisClient.Close()
	databasePath := filepath.Join(t.TempDir(), "scribe.db")
	store, err := binding.OpenSQLite(databasePath, 15*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := checkReady(ctx, redisClient, store); err != nil {
		t.Fatalf("checkReady() with healthy dependencies: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := checkReady(ctx, redisClient, store); err == nil || !strings.Contains(err.Error(), "binding store") {
		t.Fatalf("checkReady() with closed SQLite = %v", err)
	}
	store, err = binding.OpenSQLite(databasePath, 15*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	redisServer.Close()
	if err := checkReady(ctx, redisClient, store); err == nil || !strings.Contains(err.Error(), "Redis") {
		t.Fatalf("checkReady() with closed Redis = %v", err)
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
	store, err := binding.OpenSQLite(filepath.Join(t.TempDir(), "scribe.db"), 15*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := metrics.New()
	migrator, pending, recovery, err := newMigrator(cfg, redisClient, store, log, registry)
	if err != nil || migrator == nil || pending == nil || recovery == nil {
		t.Fatalf("newMigrator() = %v, %v, %v, %v", migrator, pending, recovery, err)
	}
	if err := recovery.Run(context.Background()); err != nil {
		t.Fatalf("recovery.Run() error = %v", err)
	}
	response := httptest.NewRecorder()
	registry.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(response.Body.String(), `scribe_recovery_scans_total{result="success"} 1`) {
		t.Fatalf("recovery metric was not recorded: %s", response.Body.String())
	}
}

func TestHTTPMuxServesMetrics(t *testing.T) {
	registry := metrics.New()
	registry.Authentication("NEW_WINS", "")
	handler := httpMux(config.Config{
		AuthRateLimit:         1,
		AuthBurst:             1,
		RateLimiterMaxEntries: 16,
		MaxRequestBody:        1024,
	}, &gateway.Gateway{}, func() error { return nil }, registry)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d", response.Code)
	}
	if !strings.Contains(response.Body.String(), `scribe_authentication_decisions_total{outcome="NEW_WINS",reason="none"} 1`) {
		t.Fatalf("GET /metrics did not expose registry: %s", response.Body.String())
	}
	if response.Header().Get("X-Content-Type-Options") != "nosniff" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("metrics security headers missing: %#v", response.Header())
	}
}
