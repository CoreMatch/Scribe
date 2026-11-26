package main

import (
	"net/http"

	"github.com/corematch/scribe/internal/circuitbreaker"
	"github.com/corematch/scribe/internal/config"
	"github.com/corematch/scribe/internal/gateway"
	"github.com/corematch/scribe/internal/metrics"
	"github.com/corematch/scribe/internal/middleware"
	"github.com/corematch/scribe/internal/server"
)

func httpMux(cfg config.Config, gateway *gateway.Gateway, ready func() error, registry *metrics.Registry) http.Handler {
	limiter := middleware.NewRateLimiter(cfg.AuthRateLimit, float64(cfg.AuthBurst), cfg.RateLimiterMaxEntries)
	api := middleware.SecurityHeaders(middleware.LimitRequestBody(cfg.MaxRequestBody, middleware.RateLimitAuthentication(limiter, gateway.Handler())))
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", server.HealthHandler())
	mux.Handle("GET /readyz", server.ReadyHandler(ready))
	mux.Handle("GET /metrics", middleware.SecurityHeaders(registry.Handler()))
	mux.Handle("/", api)
	return mux
}

func circuitbreakerSettings(cfg config.Config) circuitbreaker.Settings {
	return circuitbreaker.Settings{
		FailureThreshold:    cfg.CircuitFailureThreshold,
		OpenTimeout:         cfg.CircuitOpenTimeout,
		HalfOpenMaxRequests: cfg.CircuitHalfOpenMaxRequests,
	}
}
