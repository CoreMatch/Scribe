package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestProxyPreservesRequestAndRewritesAPILocation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/authserver/authenticate" || r.URL.Query().Get("agent") != "Minecraft" {
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.String())
		}
		if r.Host == "" || r.Header.Get("X-Forwarded-Proto") != "https" || r.Header.Get("X-Forwarded-For") != "" {
			t.Errorf("unexpected upstream headers: request_host=%q url_host=%q proto=%q forwarded=%q", r.Host, r.URL.Host, r.Header.Get("X-Forwarded-Proto"), r.Header.Get("X-Forwarded-For"))
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"username":"Player","password":"secret"}` {
			t.Errorf("unexpected upstream body: %s", body)
		}
		w.Header().Set("X-Authlib-Injector-API-Location", "https://upstream.invalid")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	publicURL, _ := url.Parse("https://auth.example.com")
	handler := New(target, publicURL, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest(http.MethodPost, "https://scribe.example.com/authserver/authenticate?agent=Minecraft", strings.NewReader(`{"username":"Player","password":"secret"}`))
	request.Header.Set("X-Forwarded-For", "203.0.113.1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.Code)
	}
	if got := response.Header().Get("X-Authlib-Injector-API-Location"); got != publicURL.String() {
		t.Fatalf("API location = %q, want %q", got, publicURL.String())
	}
}

func TestProxyMapsUpstreamFailureWithoutLeakingDetails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	upstream.Close()
	target, _ := url.Parse(upstream.URL)
	publicURL, _ := url.Parse("https://auth.example.com")
	handler := New(target, publicURL, 100*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://scribe.example.com/", nil))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", response.Code)
	}
	if strings.Contains(response.Body.String(), "connection refused") {
		t.Fatalf("response leaked upstream details: %s", response.Body.String())
	}
}

func TestProxyFailureLogsExcludeRequestSecrets(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	upstream.Close()
	target, _ := url.Parse(upstream.URL)
	publicURL, _ := url.Parse("https://auth.example.com")
	var logs bytes.Buffer
	handler := New(target, publicURL, time.Second, slog.New(slog.NewTextHandler(&logs, nil)))
	password := "correct-horse-battery-staple"
	accessToken := "armorial-access-token-canary"
	privateKey := "-----BEGIN PRIVATE KEY-----"
	body := `{"username":"Player","password":"` + password + `","clientToken":"` + accessToken + `","key":"` + privateKey + `"}`
	request := httptest.NewRequest(http.MethodPost, "https://scribe.example.com/authserver/authenticate?token="+accessToken, strings.NewReader(body))
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if logs.Len() == 0 {
		t.Fatal("proxy failure was not logged")
	}
	for _, secret := range []string{password, accessToken, privateKey} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("proxy log leaked request secret %q: %s", secret, logs.String())
		}
	}
}

func TestProxyMapsTLSHandshakeTimeoutToGatewayTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		_, _ = io.Copy(io.Discard, connection)
	}()
	target, _ := url.Parse("https://" + listener.Addr().String())
	publicURL, _ := url.Parse("https://auth.example.com")
	handler := New(target, publicURL, 7*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://scribe.example.com/", nil))
	if response.Code != http.StatusGatewayTimeout || !strings.Contains(response.Body.String(), "TimeoutException") {
		t.Fatalf("TLS timeout response = %d %s", response.Code, response.Body.String())
	}
}

func TestProxyMapsTimeoutToGatewayTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	publicURL, _ := url.Parse("https://auth.example.com")
	handler := New(target, publicURL, 20*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://scribe.example.com/", nil))
	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", response.Code)
	}
}
