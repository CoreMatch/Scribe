package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/corematch/scribe/internal/protocol"
)

type Proxy struct {
	handler http.Handler
}

func New(target *url.URL, publicBaseURL *url.URL, timeout time.Duration, log *slog.Logger) *Proxy {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
	}
	reverseProxy := httputil.NewSingleHostReverseProxy(target)
	reverseProxy.Transport = transport
	reverseProxy.Director = director(target, publicBaseURL)
	reverseProxy.ModifyResponse = func(response *http.Response) error {
		response.Header.Set("X-Authlib-Injector-API-Location", publicBaseURL.String())
		return nil
	}
	reverseProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Error("upstream request failed", "upstream", target.Host, "method", r.Method, "path", r.URL.Path)
		if errors.Is(err, context.DeadlineExceeded) {
			protocol.WriteError(w, http.StatusGatewayTimeout, "TimeoutException", "Upstream request timed out.")
			return
		}
		protocol.WriteError(w, http.StatusBadGateway, "ServiceUnavailableException", "Upstream service unavailable.")
	}
	return &Proxy{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		reverseProxy.ServeHTTP(w, r.WithContext(ctx))
	})}
}

func director(target, publicBaseURL *url.URL) func(*http.Request) {
	return func(request *http.Request) {
		request.URL.Scheme = target.Scheme
		request.URL.Host = target.Host
		request.Host = target.Host
		request.Header.Del("Forwarded")
		request.Header["X-Forwarded-For"] = nil
		request.Header.Del("X-Forwarded-Host")
		request.Header.Del("X-Forwarded-Proto")
		request.Header.Set("X-Forwarded-Proto", publicBaseURL.Scheme)
	}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.handler.ServeHTTP(w, r)
}
