package main

import (
	"net/http"

	"github.com/corematch/scribe/internal/server"
)

func httpMux() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", server.HealthHandler())
	mux.Handle("GET /readyz", server.HealthHandler())
	return mux
}
