package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadRequiresUpstreamConfiguration(t *testing.T) {
	t.Setenv("SCRIBE_PUBLIC_BASE_URL", "")
	t.Setenv("SCRIBE_LEGACY_BASE_URL", "")
	t.Setenv("SCRIBE_ARMIORIAL_BASE_URL", "")
	t.Setenv("SCRIBE_REDIS_URL", "")
	t.Setenv("SCRIBE_MIGRATION_SERVICE_TOKEN", "")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "SCRIBE_PUBLIC_BASE_URL") {
		t.Fatalf("Load() error = %v, want missing public URL error", err)
	}
}

func TestLoadReadsOverrides(t *testing.T) {
	setMigrationEnvironment(t)
	t.Setenv("SCRIBE_PUBLIC_BASE_URL", "https://auth.example.com")
	t.Setenv("SCRIBE_LEGACY_BASE_URL", "https://legacy.internal")
	t.Setenv("SCRIBE_ARMIORIAL_BASE_URL", "https://armorial.internal")
	t.Setenv("SCRIBE_UPSTREAM_TIMEOUT", "3s")
	t.Setenv("SCRIBE_SHUTDOWN_TIMEOUT", "2s")
	t.Setenv("SCRIBE_MAX_REQUEST_BODY", "4096")
	t.Setenv("SCRIBE_SQLITE_PATH", "/tmp/scribe-test.db")
	t.Setenv("SCRIBE_AUTH_RATE_LIMIT", "2.5")
	t.Setenv("SCRIBE_AUTH_BURST", "7")
	t.Setenv("SCRIBE_RATE_LIMIT_MAX_ENTRIES", "100")
	t.Setenv("SCRIBE_CIRCUIT_FAILURE_THRESHOLD", "3")
	t.Setenv("SCRIBE_CIRCUIT_OPEN_TIMEOUT", "4s")
	t.Setenv("SCRIBE_CIRCUIT_HALF_OPEN_MAX_REQUESTS", "2")
	t.Setenv("SCRIBE_REDIS_URL", "rediss://scribe:secret@redis.internal:6379/1")
	t.Setenv("SCRIBE_MIGRATION_SERVICE_TOKEN", "migration-service-token")
	t.Setenv("SCRIBE_MIGRATION_TTL", "2h")
	t.Setenv("SCRIBE_MIGRATION_MAX_PACKAGE_BYTES", "8388608")
	t.Setenv("SCRIBE_MIGRATION_TIMEOUT", "45s")
	t.Setenv("SCRIBE_WRITE_TIMEOUT", "90s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.RequestTimeout != 3*time.Second || cfg.ShutdownTimeout != 2*time.Second || cfg.MaxRequestBody != 4096 {
		t.Fatalf("Load() overrides not applied: %+v", cfg)
	}
	if cfg.SQLitePath != "/tmp/scribe-test.db" || cfg.AuthRateLimit != 2.5 || cfg.AuthBurst != 7 || cfg.RateLimiterMaxEntries != 100 {
		t.Fatalf("Load() storage/rate overrides not applied: %+v", cfg)
	}
	if cfg.CircuitFailureThreshold != 3 || cfg.CircuitOpenTimeout != 4*time.Second || cfg.CircuitHalfOpenMaxRequests != 2 {
		t.Fatalf("Load() circuit overrides not applied: %+v", cfg)
	}
	if cfg.RedisURL != "rediss://scribe:secret@redis.internal:6379/1" || cfg.MigrationServiceToken != "migration-service-token" {
		t.Fatalf("Load() migration credentials not applied: %+v", cfg)
	}
	if cfg.MigrationTTL != 2*time.Hour || cfg.MigrationMaxPackageBytes != 8<<20 || cfg.MigrationTimeout != 45*time.Second || cfg.WriteTimeout != 90*time.Second {
		t.Fatalf("Load() migration limits not applied: %+v", cfg)
	}
}

func TestLoadRejectsInvalidNumericOverrides(t *testing.T) {
	setMigrationEnvironment(t)
	t.Setenv("SCRIBE_PUBLIC_BASE_URL", "https://auth.example.com")
	t.Setenv("SCRIBE_LEGACY_BASE_URL", "https://legacy.internal")
	t.Setenv("SCRIBE_ARMIORIAL_BASE_URL", "https://armorial.internal")
	for _, value := range []string{"0", "-1", "NaN", "+Inf", "not-a-number"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("SCRIBE_AUTH_RATE_LIMIT", value)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() accepted invalid rate %q", value)
			}
		})
	}
}

func TestLoadRequiresMigrationConfiguration(t *testing.T) {
	t.Setenv("SCRIBE_PUBLIC_BASE_URL", "https://auth.example.com")
	t.Setenv("SCRIBE_LEGACY_BASE_URL", "https://legacy.internal")
	t.Setenv("SCRIBE_ARMIORIAL_BASE_URL", "https://armorial.internal")
	t.Setenv("SCRIBE_REDIS_URL", "")
	t.Setenv("SCRIBE_MIGRATION_SERVICE_TOKEN", "service-token")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SCRIBE_REDIS_URL") {
		t.Fatalf("Load() error = %v, want missing Redis URL", err)
	}
	t.Setenv("SCRIBE_REDIS_URL", "rediss://redis.internal:6379/0")
	t.Setenv("SCRIBE_MIGRATION_SERVICE_TOKEN", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SCRIBE_MIGRATION_SERVICE_TOKEN") {
		t.Fatalf("Load() error = %v, want missing service token", err)
	}
}

func TestValidateRedisURL(t *testing.T) {
	for _, raw := range []string{
		"",
		"redis://redis.internal:6379/0",
		"rediss:///0",
		"rediss://redis.internal:6379/0#fragment",
		"rediss://redis.internal:6379/0?skip_verify=true",
		"rediss://redis.internal:6379/0?skip_verify=1",
	} {
		if err := validateRedisURL(raw); err == nil {
			t.Fatalf("validateRedisURL(%q) unexpectedly succeeded", raw)
		}
	}
	if err := validateRedisURL("rediss://scribe:secret@redis.internal:6379/1"); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsInvalidMigrationToken(t *testing.T) {
	setMigrationEnvironment(t)
	t.Setenv("SCRIBE_PUBLIC_BASE_URL", "https://auth.example.com")
	t.Setenv("SCRIBE_LEGACY_BASE_URL", "https://legacy.internal")
	t.Setenv("SCRIBE_ARMIORIAL_BASE_URL", "https://armorial.internal")
	for _, token := range []string{"has space", "has\ttab", "has\nnewline"} {
		t.Run(token, func(t *testing.T) {
			t.Setenv("SCRIBE_MIGRATION_SERVICE_TOKEN", token)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() accepted token %q", token)
			}
		})
	}
}

func TestLoadRejectsWriteTimeoutShorterThanMigration(t *testing.T) {
	setMigrationEnvironment(t)
	t.Setenv("SCRIBE_PUBLIC_BASE_URL", "https://auth.example.com")
	t.Setenv("SCRIBE_LEGACY_BASE_URL", "https://legacy.internal")
	t.Setenv("SCRIBE_ARMIORIAL_BASE_URL", "https://armorial.internal")
	t.Setenv("SCRIBE_MIGRATION_TIMEOUT", "60s")
	t.Setenv("SCRIBE_WRITE_TIMEOUT", "30s")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SCRIBE_WRITE_TIMEOUT") {
		t.Fatalf("Load() error = %v, want write timeout error", err)
	}
}

func setMigrationEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("SCRIBE_REDIS_URL", "rediss://redis.internal:6379/0")
	t.Setenv("SCRIBE_MIGRATION_SERVICE_TOKEN", "service-token")
}

func TestValidateRejectsInsecureOrInvalidBaseURLs(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "relative", url: "/api"},
		{name: "credentials", url: "https://user:pass@example.com"},
		{name: "query", url: "https://example.com?token=secret"},
		{name: "fragment", url: "https://example.com#fragment"},
		{name: "path", url: "https://example.com/prefix"},
		{name: "scheme", url: "ftp://example.com"},
		{name: "insecure http", url: "http://example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateBaseURL("TEST_URL", tt.url); err == nil {
				t.Fatalf("validateBaseURL(%q) unexpectedly succeeded", tt.url)
			}
		})
	}
}
