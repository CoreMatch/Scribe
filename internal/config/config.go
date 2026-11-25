package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr      string
	PublicBaseURL   string
	LegacyBaseURL   string
	ArmorialBaseURL string
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
	MaxRequestBody  int64
}

func Load() (Config, error) {
	cfg := Config{
		ListenAddr:      env("SCRIBE_LISTEN_ADDR", ":8080"),
		PublicBaseURL:   os.Getenv("SCRIBE_PUBLIC_BASE_URL"),
		LegacyBaseURL:   os.Getenv("SCRIBE_LEGACY_BASE_URL"),
		ArmorialBaseURL: os.Getenv("SCRIBE_ARMIORIAL_BASE_URL"),
		RequestTimeout:  15 * time.Second,
		ShutdownTimeout: 15 * time.Second,
		MaxRequestBody:  1 << 20,
	}

	var err error
	if cfg.RequestTimeout, err = durationEnv("SCRIBE_UPSTREAM_TIMEOUT", cfg.RequestTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = durationEnv("SCRIBE_SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if cfg.MaxRequestBody, err = int64Env("SCRIBE_MAX_REQUEST_BODY", cfg.MaxRequestBody); err != nil {
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
	if c.MaxRequestBody <= 0 {
		return errors.New("SCRIBE_MAX_REQUEST_BODY must be positive")
	}
	return nil
}

func validateBaseURL(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s must be an absolute URL without credentials, query, or fragment", name)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("%s must use http or https", name)
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
