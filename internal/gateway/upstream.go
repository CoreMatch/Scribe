package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/corematch/scribe/internal/binding"
	"github.com/corematch/scribe/internal/protocol"
	"github.com/corematch/scribe/internal/race"
)

var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func (g *Gateway) doRequest(ctx context.Context, winner binding.Winner, incoming *http.Request, body []byte) race.Result {
	base := g.legacyBaseURL
	if winner == binding.Armorial {
		base = g.armorialBaseURL
	}
	target := *base
	target.Path = incoming.URL.Path
	target.RawQuery = incoming.URL.RawQuery
	request, err := http.NewRequestWithContext(ctx, incoming.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return race.Result{Err: err}
	}
	request.Header = incoming.Header.Clone()
	removeHopByHopHeaders(request.Header)
	request.Header.Del("Accept-Encoding")
	request.Header.Del("Forwarded")
	request.Header["X-Forwarded-For"] = nil
	request.Header.Del("X-Forwarded-Host")
	request.Header.Set("X-Forwarded-Proto", g.publicBaseURL.Scheme)
	request.Host = base.Host
	request.ContentLength = int64(len(body))

	response, err := g.client.Do(request)
	if err != nil {
		return race.Result{Err: err}
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, g.maxResponseBody+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil {
		return race.Result{Err: err}
	}
	if int64(len(responseBody)) > g.maxResponseBody {
		return race.Result{Err: errors.New("upstream response exceeds size limit")}
	}
	return race.Result{
		StatusCode: response.StatusCode,
		Header:     response.Header.Clone(),
		Body:       responseBody,
	}
}

func (g *Gateway) writeResult(w http.ResponseWriter, result race.Result) {
	if result.Err != nil {
		if isTimeout(result.Err) {
			writeProtocolError(w, http.StatusGatewayTimeout, "TimeoutException", "Upstream request timed out.")
			return
		}
		writeProtocolError(w, http.StatusBadGateway, "ServiceUnavailableException", "Upstream service unavailable.")
		return
	}
	removeHopByHopHeaders(result.Header)
	result.Header.Del("X-Authlib-Injector-API-Location")
	for name, values := range result.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.Header().Set("X-Authlib-Injector-API-Location", g.publicBaseURL.String())
	w.Header().Set("Cache-Control", "no-store")
	if w.Header().Get("Content-Type") == "" && len(result.Body) > 0 {
		w.Header().Set("Content-Type", "application/json")
	}
	status := result.StatusCode
	if status == 0 {
		status = http.StatusBadGateway
	}
	w.WriteHeader(status)
	if status != http.StatusNoContent {
		_, _ = w.Write(result.Body)
	}
}

func writeProtocolError(w http.ResponseWriter, status int, errorName, message string) {
	protocol.WriteError(w, status, errorName, message)
}

func removeHopByHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			header.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range hopByHopHeaders {
		header.Del(name)
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout()
}
