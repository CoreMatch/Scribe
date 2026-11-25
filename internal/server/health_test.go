package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadyHandlerChecksDependency(t *testing.T) {
	called := false
	handler := ReadyHandler(func() error {
		called = true
		return errors.New("redis password must not appear")
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if !called || response.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready response = %d, called = %v", response.Code, called)
	}
	if strings.Contains(response.Body.String(), "redis") || strings.Contains(response.Body.String(), "password") {
		t.Fatalf("readiness leaked dependency details: %s", response.Body.String())
	}
}

func TestReadyHandlerWithoutDependencyIsHealthy(t *testing.T) {
	response := httptest.NewRecorder()
	ReadyHandler(nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("ready status = %d, want 200", response.Code)
	}
}
