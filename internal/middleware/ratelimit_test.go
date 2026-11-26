package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRateLimiterBurstAndRefill(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	limiter := NewRateLimiter(1, 2, 10)
	limiter.now = func() time.Time { return now }

	if !limiter.Allow("client") || !limiter.Allow("client") {
		t.Fatal("initial burst was not allowed")
	}
	if limiter.Allow("client") {
		t.Fatal("request above burst unexpectedly allowed")
	}
	now = now.Add(time.Second)
	if !limiter.Allow("client") {
		t.Fatal("refilled token was not allowed")
	}
}

func TestRateLimiterSeparatesClients(t *testing.T) {
	limiter := NewRateLimiter(1, 1, 10)
	if !limiter.Allow("client-a") || !limiter.Allow("client-b") {
		t.Fatal("clients shared a rate limit bucket")
	}
}

func TestRateLimiterIsConcurrencySafe(t *testing.T) {
	limiter := NewRateLimiter(1, 10, 10)
	var wg sync.WaitGroup
	allowed := make(chan bool, 100)
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allowed <- limiter.Allow("client")
		}()
	}
	wg.Wait()
	close(allowed)
	count := 0
	for value := range allowed {
		if value {
			count++
		}
	}
	if count != 10 {
		t.Fatalf("allowed %d requests, want 10", count)
	}
}

func TestRateLimitAuthenticationOnlyLimitsAuthenticate(t *testing.T) {
	limiter := NewRateLimiter(1, 1, 10)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := RateLimitAuthentication(limiter, NewRateLimiter(1, 1, 10), next)
	tests := []struct {
		path string
		want int
	}{
		{path: "/healthz", want: http.StatusNoContent},
		{path: "/authserver/refresh", want: http.StatusNoContent},
		{path: "/authserver/authenticate", want: http.StatusNoContent},
		{path: "/authserver/authenticate", want: http.StatusTooManyRequests},
	}
	for _, tt := range tests {
		request := httptest.NewRequest(http.MethodPost, tt.path, nil)
		request.RemoteAddr = "192.0.2.10:1234"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != tt.want {
			t.Fatalf("%s status = %d, want %d", tt.path, response.Code, tt.want)
		}
	}
}

func TestRateLimitAuthenticationDedicatesSignoutBucket(t *testing.T) {
	limiter := NewRateLimiter(1, 1, 10)
	signoutLimiter := NewRateLimiter(1, 1, 10)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := RateLimitAuthentication(limiter, signoutLimiter, next)
	request := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.RemoteAddr = "192.0.2.10:1234"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}

	for _, path := range []string{"/authserver/authenticate", "/authserver/signout"} {
		if response := request(path); response.Code != http.StatusNoContent {
			t.Fatalf("first %s status = %d, want 204", path, response.Code)
		}
	}
	for _, path := range []string{"/authserver/authenticate", "/authserver/signout"} {
		response := request(path)
		if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "1" {
			t.Fatalf("second %s status = %d, retry-after = %q", path, response.Code, response.Header().Get("Retry-After"))
		}
		if !strings.Contains(response.Body.String(), "TooManyRequestsException") {
			t.Fatalf("second %s error = %s", path, response.Body.String())
		}
	}
}

func TestRateLimitRetryAfterCoversTokenRefill(t *testing.T) {
	limiter := NewRateLimiter(0.2, 1, 10)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := RateLimit(limiter, next)
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/authserver/signout", nil)
		req.RemoteAddr = "192.0.2.10:1234"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	if response := request(); response.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want 204", response.Code)
	}
	response := request()
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "5" {
		t.Fatalf("limited status = %d, retry-after = %q, want 429 and 5", response.Code, response.Header().Get("Retry-After"))
	}
}

func TestRateLimitMiddlewareUsesRemoteAddrOnly(t *testing.T) {
	limiter := NewRateLimiter(1, 1, 10)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := RateLimit(limiter, next)

	request := httptest.NewRequest(http.MethodPost, "https://scribe.example/authserver/authenticate", nil)
	request.RemoteAddr = "192.0.2.10:1234"
	request.Header.Set("X-Forwarded-For", "203.0.113.10")
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, request)
	if first.Code != http.StatusNoContent {
		t.Fatalf("first status = %d", first.Code)
	}

	request.RemoteAddr = "192.0.2.10:5678"
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, request)
	if second.Code != http.StatusTooManyRequests || second.Header().Get("Retry-After") != "1" {
		t.Fatalf("second status=%d retry-after=%q", second.Code, second.Header().Get("Retry-After"))
	}

	request.RemoteAddr = "192.0.2.11:1234"
	third := httptest.NewRecorder()
	handler.ServeHTTP(third, request)
	if third.Code != http.StatusNoContent {
		t.Fatalf("different client status = %d", third.Code)
	}
}
