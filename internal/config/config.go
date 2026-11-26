package config

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type Config struct {
	ListenAddr                 string
	PublicBaseURL              string
	LegacyBaseURL              string
	ArmorialBaseURL            string
	RedisURL                   string
	MigrationServiceToken      string
	MigrationTTL               time.Duration
	MigrationMaxPackageBytes   int64
	MigrationTimeout           time.Duration
	WriteTimeout               time.Duration
	SQLitePath                 string
	TokenBindingTTL            time.Duration
	RequestTimeout             time.Duration
	ShutdownTimeout            time.Duration
	MaxRequestBody             int64
	AuthRateLimit              float64
	AuthBurst                  int
	RateLimiterMaxEntries      int
	CircuitFailureThreshold    uint32
	CircuitOpenTimeout         time.Duration
	CircuitHalfOpenMaxRequests uint32
}

func Load() (Config, error) {
	cfg := Config{
		ListenAddr:                 env("SCRIBE_LISTEN_ADDR", ":8080"),
		PublicBaseURL:              os.Getenv("SCRIBE_PUBLIC_BASE_URL"),
		LegacyBaseURL:              os.Getenv("SCRIBE_LEGACY_BASE_URL"),
		ArmorialBaseURL:            os.Getenv("SCRIBE_ARMIORIAL_BASE_URL"),
		RedisURL:                   os.Getenv("SCRIBE_REDIS_URL"),
		MigrationServiceToken:      os.Getenv("SCRIBE_MIGRATION_SERVICE_TOKEN"),
		MigrationTTL:               24 * time.Hour,
		MigrationMaxPackageBytes:   16 << 20,
		MigrationTimeout:           60 * time.Second,
		WriteTimeout:               150 * time.Second,
		SQLitePath:                 env("SCRIBE_SQLITE_PATH", "data/scribe.db"),
		TokenBindingTTL:            15 * 24 * time.Hour,
		RequestTimeout:             15 * time.Second,
		ShutdownTimeout:            15 * time.Second,
		MaxRequestBody:             1 << 20,
		AuthRateLimit:              5,
		AuthBurst:                  10,
		RateLimiterMaxEntries:      65536,
		CircuitFailureThreshold:    5,
		CircuitOpenTimeout:         10 * time.Second,
		CircuitHalfOpenMaxRequests: 1,
	}

	var err error
	if cfg.TokenBindingTTL, err = durationEnv("SCRIBE_TOKEN_BINDING_TTL", cfg.TokenBindingTTL); err != nil {
		return Config{}, err
	}
	if cfg.RequestTimeout, err = durationEnv("SCRIBE_UPSTREAM_TIMEOUT", cfg.RequestTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = durationEnv("SCRIBE_SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if cfg.WriteTimeout, err = durationEnv("SCRIBE_WRITE_TIMEOUT", cfg.WriteTimeout); err != nil {
		return Config{}, err
	}
	if cfg.MigrationTTL, err = durationEnv("SCRIBE_MIGRATION_TTL", cfg.MigrationTTL); err != nil {
		return Config{}, err
	}
	if cfg.MigrationMaxPackageBytes, err = int64Env("SCRIBE_MIGRATION_MAX_PACKAGE_BYTES", cfg.MigrationMaxPackageBytes); err != nil {
		return Config{}, err
	}
	if cfg.MigrationTimeout, err = durationEnv("SCRIBE_MIGRATION_TIMEOUT", cfg.MigrationTimeout); err != nil {
		return Config{}, err
	}
	if cfg.MaxRequestBody, err = int64Env("SCRIBE_MAX_REQUEST_BODY", cfg.MaxRequestBody); err != nil {
		return Config{}, err
	}
	if cfg.AuthRateLimit, err = floatEnv("SCRIBE_AUTH_RATE_LIMIT", cfg.AuthRateLimit); err != nil {
		return Config{}, err
	}
	if cfg.AuthBurst, err = intEnv("SCRIBE_AUTH_BURST", cfg.AuthBurst); err != nil {
		return Config{}, err
	}
	if cfg.RateLimiterMaxEntries, err = intEnv("SCRIBE_RATE_LIMIT_MAX_ENTRIES", cfg.RateLimiterMaxEntries); err != nil {
		return Config{}, err
	}
	if cfg.CircuitFailureThreshold, err = uint32Env("SCRIBE_CIRCUIT_FAILURE_THRESHOLD", cfg.CircuitFailureThreshold); err != nil {
		return Config{}, err
	}
	if cfg.CircuitOpenTimeout, err = durationEnv("SCRIBE_CIRCUIT_OPEN_TIMEOUT", cfg.CircuitOpenTimeout); err != nil {
		return Config{}, err
	}
	if cfg.CircuitHalfOpenMaxRequests, err = uint32Env("SCRIBE_CIRCUIT_HALF_OPEN_MAX_REQUESTS", cfg.CircuitHalfOpenMaxRequests); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.ListenAddr) == "" {
		return errors.New("SCRIBE_LISTEN_ADDR must not be empty")
	}
	if strings.TrimSpace(c.SQLitePath) == "" {
		return errors.New("SCRIBE_SQLITE_PATH must not be empty")
	}
	if c.TokenBindingTTL < time.Second {
		return errors.New("SCRIBE_TOKEN_BINDING_TTL must be at least one second")
	}
	for _, item := range []struct {
		name string
		raw  string
	}{
		{name: "SCRIBE_PUBLIC_BASE_URL", raw: c.PublicBaseURL},
		{name: "SCRIBE_LEGACY_BASE_URL", raw: c.LegacyBaseURL},
		{name: "SCRIBE_ARMIORIAL_BASE_URL", raw: c.ArmorialBaseURL},
	} {
		if err := validateBaseURL(item.name, item.raw); err != nil {
			return err
		}
	}
	if c.RequestTimeout <= 0 {
		return errors.New("SCRIBE_UPSTREAM_TIMEOUT must be positive")
	}
	if c.ShutdownTimeout <= 0 {
		return errors.New("SCRIBE_SHUTDOWN_TIMEOUT must be positive")
	}
	if c.WriteTimeout <= 0 {
		return errors.New("SCRIBE_WRITE_TIMEOUT must be positive")
	}
	if c.WriteTimeout < c.MigrationTimeout {
		return errors.New("SCRIBE_WRITE_TIMEOUT must not be shorter than SCRIBE_MIGRATION_TIMEOUT")
	}
	if c.MigrationTTL <= 0 {
		return errors.New("SCRIBE_MIGRATION_TTL must be positive")
	}
	if c.MigrationMaxPackageBytes <= 0 {
		return errors.New("SCRIBE_MIGRATION_MAX_PACKAGE_BYTES must be positive")
	}
	if c.MigrationTimeout <= 0 {
		return errors.New("SCRIBE_MIGRATION_TIMEOUT must be positive")
	}
	if err := validateRedisURL(c.RedisURL); err != nil {
		return err
	}
	if c.MigrationServiceToken == "" || len(c.MigrationServiceToken) > 8192 || strings.IndexFunc(c.MigrationServiceToken, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return errors.New("SCRIBE_MIGRATION_SERVICE_TOKEN must be nonempty and contain no whitespace or control characters")
	}
	if c.MaxRequestBody <= 0 {
		return errors.New("SCRIBE_MAX_REQUEST_BODY must be positive")
	}
	if c.AuthRateLimit <= 0 || math.IsNaN(c.AuthRateLimit) || math.IsInf(c.AuthRateLimit, 0) {
		return errors.New("SCRIBE_AUTH_RATE_LIMIT must be a finite positive number")
	}
	if c.AuthBurst <= 0 {
		return errors.New("SCRIBE_AUTH_BURST must be positive")
	}
	if c.RateLimiterMaxEntries <= 0 {
		return errors.New("SCRIBE_RATE_LIMIT_MAX_ENTRIES must be positive")
	}
	if c.CircuitFailureThreshold == 0 {
		return errors.New("SCRIBE_CIRCUIT_FAILURE_THRESHOLD must be positive")
	}
	if c.CircuitOpenTimeout <= 0 {
		return errors.New("SCRIBE_CIRCUIT_OPEN_TIMEOUT must be positive")
	}
	if c.CircuitHalfOpenMaxRequests == 0 {
		return errors.New("SCRIBE_CIRCUIT_HALF_OPEN_MAX_REQUESTS must be positive")
	}
	return nil
}

func validateRedisURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "rediss" || u.Hostname() == "" || u.Fragment != "" {
		return errors.New("SCRIBE_REDIS_URL must be an absolute rediss URL without a fragment")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return errors.New("SCRIBE_REDIS_URL has invalid query parameters")
	}
	for _, value := range query["skip_verify"] {
		if strings.EqualFold(value, "true") || value == "1" {
			return errors.New("SCRIBE_REDIS_URL must verify TLS certificates")
		}
	}
	return nil
}

func validateBaseURL(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s must be an absolute URL without credentials, query, or fragment", name)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%s must use https", name)
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("%s must not contain a path", name)
	}
	return nil
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return value, nil
}

func int64Env(key string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return value, nil
}

func intEnv(key string, fallback int) (int, error) {
	value, err := int64Env(key, int64(fallback))
	return int(value), err
}

func uint32Env(key string, fallback uint32) (uint32, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || value == 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return uint32(value), nil
}

func floatEnv(key string, fallback float64) (float64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("%s must be a finite positive number", key)
	}
	return value, nil
}
