package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/corematch/scribe/internal/binding"
	"github.com/corematch/scribe/internal/circuitbreaker"
	"github.com/corematch/scribe/internal/metrics"
	"github.com/corematch/scribe/internal/migration"
	"github.com/corematch/scribe/internal/proxy"
	"github.com/corematch/scribe/internal/race"
)

type Options struct {
	PublicBaseURL    string
	LegacyBaseURL    string
	ArmorialBaseURL  string
	PureProxy        bool
	RequestTimeout   time.Duration
	MaxRequestBody   int64
	MaxResponseBody  int64
	Circuit          circuitbreaker.Settings
	MigrationIntent  race.Intent
	Migrator         migration.Runner
	MigrationPending migration.PendingChecker
	Metrics          *metrics.Registry
}

type Gateway struct {
	store            binding.Store
	client           *http.Client
	publicBaseURL    *url.URL
	legacyBaseURL    *url.URL
	armorialBaseURL  *url.URL
	legacyProxy      http.Handler
	armorialProxy    http.Handler
	legacyBreaker    *circuitbreaker.Breaker
	armorialBreaker  *circuitbreaker.Breaker
	maxRequestBody   int64
	maxResponseBody  int64
	migrationIntent  race.Intent
	migrator         migration.Runner
	migrationPending migration.PendingChecker
	metrics          *metrics.Registry
	pureProxy        bool
	log              *slog.Logger
}

func New(options Options, store binding.Store, log *slog.Logger) (*Gateway, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	publicURL, err := parseBaseURL(options.PublicBaseURL)
	if err != nil {
		return nil, err
	}
	legacyURL, err := parseBaseURL(options.LegacyBaseURL)
	if err != nil {
		return nil, err
	}
	if options.RequestTimeout <= 0 {
		return nil, errors.New("request timeout must be positive")
	}
	if options.PureProxy {
		log.Warn("starting in pure proxy mode; migration and binding routing disabled", "upstream", "legacy")
		return &Gateway{
			publicBaseURL: publicURL,
			legacyBaseURL: legacyURL,
			legacyProxy:   proxy.New(legacyURL, publicURL, options.RequestTimeout, log),
			pureProxy:     true,
			log:           log,
		}, nil
	}
	if store == nil {
		return nil, errors.New("binding store is required")
	}
	armorialURL, err := parseBaseURL(options.ArmorialBaseURL)
	if err != nil {
		return nil, err
	}
	if options.Migrator != nil && options.MigrationPending == nil {
		return nil, errors.New("migration pending checker is required")
	}
	maxResponseBody := options.MaxResponseBody
	if maxResponseBody <= 0 {
		maxResponseBody = 1 << 20
	}
	maxRequestBody := options.MaxRequestBody
	if maxRequestBody <= 0 {
		maxRequestBody = 1 << 20
	}
	legacyBreaker, err := circuitbreaker.New(options.Circuit)
	if err != nil {
		return nil, err
	}
	armorialBreaker, err := circuitbreaker.New(options.Circuit)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: options.RequestTimeout,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   options.RequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Gateway{
		store:            store,
		client:           client,
		publicBaseURL:    publicURL,
		legacyBaseURL:    legacyURL,
		armorialBaseURL:  armorialURL,
		legacyProxy:      proxy.New(legacyURL, publicURL, options.RequestTimeout, log),
		armorialProxy:    proxy.New(armorialURL, publicURL, options.RequestTimeout, log),
		legacyBreaker:    legacyBreaker,
		armorialBreaker:  armorialBreaker,
		maxRequestBody:   maxRequestBody,
		maxResponseBody:  maxResponseBody,
		migrationIntent:  options.MigrationIntent,
		migrator:         options.Migrator,
		migrationPending: options.MigrationPending,
		metrics:          options.Metrics,
		log:              log,
	}, nil
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	rootProxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.armorialProxy.ServeHTTP(w, r)
	})
	authenticate := http.HandlerFunc(g.authenticate)
	refresh := http.HandlerFunc(g.refresh)
	validate := http.HandlerFunc(g.validate)
	invalidate := http.HandlerFunc(g.invalidate)
	signout := http.HandlerFunc(g.signout)
	if g.pureProxy {
		legacy := http.HandlerFunc(g.legacyProxy.ServeHTTP)
		rootProxy = legacy
		authenticate = legacy
		refresh = legacy
		validate = legacy
		invalidate = legacy
		signout = legacy
	}
	mux.Handle("GET /{$}", rootProxy)
	mux.Handle("POST /authserver/authenticate", authenticate)
	mux.Handle("POST /authserver/refresh", refresh)
	mux.Handle("POST /authserver/validate", validate)
	mux.Handle("POST /authserver/invalidate", invalidate)
	mux.Handle("POST /authserver/signout", signout)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeProtocolError(w, http.StatusNotFound, "IllegalArgumentException", "Not found.")
	})
	return mux
}

func (g *Gateway) execute(ctx context.Context, winner binding.Winner, request *http.Request, body []byte) race.Result {
	fn := func(ctx context.Context) race.Result {
		return g.doRequest(ctx, winner, request, body)
	}
	if winner == binding.Armorial {
		return g.armorialBreaker.Execute(ctx, fn)
	}
	return g.legacyBreaker.Execute(ctx, fn)
}

func parseBaseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("invalid base URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("base URL must use http or https")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return nil, errors.New("base URL must not contain a path")
	}
	return parsed, nil
}
