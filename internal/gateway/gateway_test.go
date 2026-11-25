package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/corematch/scribe/internal/binding"
	"github.com/corematch/scribe/internal/circuitbreaker"
	"github.com/corematch/scribe/internal/race"
)

type testGateway struct {
	gateway *Gateway
	store   *binding.SQLiteStore
	handler http.Handler
}

func TestAuthenticateBindsNewWinnerAndNeverReraces(t *testing.T) {
	var legacyCalls atomic.Int32
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		legacyCalls.Add(1)
		writeJSON(w, http.StatusForbidden, `{"error":"ForbiddenOperationException","errorMessage":"Invalid credentials."}`)
	}))
	defer legacy.Close()
	armorial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, authenticationBody("armorial-token", "ArmorialPlayer", true))
	}))
	defer armorial.Close()
	env := newTestGateway(t, armorial.URL, legacy.URL, race.IntentUnspecified)

	response := env.perform(http.MethodPost, "/authserver/authenticate", `{"username":"Player","password":"secret"}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "armorial-token") {
		t.Fatalf("first authenticate = %d %s", response.Code, response.Body.String())
	}
	bound, err := env.store.GetBinding(context.Background(), "Player")
	if err != nil || bound.Winner != binding.Armorial {
		t.Fatalf("binding = %+v, %v", bound, err)
	}
	if _, err := env.store.GetBindingByToken(context.Background(), "armorial-token"); err != nil {
		t.Fatalf("token binding missing: %v", err)
	}

	response = env.perform(http.MethodPost, "/authserver/authenticate", `{"username":"Player","password":"secret"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("second authenticate = %d %s", response.Code, response.Body.String())
	}
	if legacyCalls.Load() != 1 {
		t.Fatalf("legacy calls = %d, want 1", legacyCalls.Load())
	}
}

func TestBoundUserDoesNotSwitchWhenWinnerFails(t *testing.T) {
	var fail atomic.Bool
	var legacyCalls atomic.Int32
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		legacyCalls.Add(1)
		writeJSON(w, http.StatusOK, authenticationBody("legacy-token", "LegacyPlayer", true))
	}))
	defer legacy.Close()
	armorial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			writeJSON(w, http.StatusBadGateway, `{"error":"ServiceUnavailableException"}`)
			return
		}
		writeJSON(w, http.StatusOK, authenticationBody("armorial-token", "ArmorialPlayer", true))
	}))
	defer armorial.Close()
	env := newTestGateway(t, armorial.URL, legacy.URL, race.IntentUnspecified)
	if response := env.perform(http.MethodPost, "/authserver/authenticate", `{"username":"Player","password":"secret"}`); response.Code != http.StatusOK {
		t.Fatalf("initial authenticate = %d %s", response.Code, response.Body.String())
	}
	fail.Store(true)
	response := env.perform(http.MethodPost, "/authserver/authenticate", `{"username":"Player","password":"secret"}`)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("failed winner status = %d, want 502", response.Code)
	}
	if legacyCalls.Load() != 1 {
		t.Fatalf("legacy was called after binding: %d", legacyCalls.Load())
	}
	bound, err := env.store.GetBinding(context.Background(), "Player")
	if err != nil || bound.Winner != binding.Armorial {
		t.Fatalf("binding changed: %+v, %v", bound, err)
	}
}

func TestMigrationBranchesFailClosedWithoutBinding(t *testing.T) {
	tests := []struct {
		name     string
		armorial string
		intent   race.Intent
	}{
		{name: "create core", armorial: `{"error":"ForbiddenOperationException"}`},
		{name: "reuse core", armorial: authenticationBody("armorial-token", "Player", false), intent: race.IntentImport},
		{name: "intent required", armorial: authenticationBody("armorial-token", "Player", false)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, authenticationBody("legacy-token", "LegacyPlayer", true))
			}))
			defer legacy.Close()
			armorial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if strings.HasPrefix(tt.armorial, "{") && strings.Contains(tt.armorial, "accessToken") {
					writeJSON(w, http.StatusOK, tt.armorial)
				} else {
					writeJSON(w, http.StatusForbidden, tt.armorial)
				}
			}))
			defer armorial.Close()
			env := newTestGateway(t, armorial.URL, legacy.URL, tt.intent)
			response := env.perform(http.MethodPost, "/authserver/authenticate", `{"username":"Player","password":"secret"}`)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", response.Code)
			}
			if strings.Contains(response.Body.String(), "accessToken") {
				t.Fatalf("migration response leaked an upstream token: %s", response.Body.String())
			}
			if _, err := env.store.GetBinding(context.Background(), "Player"); !errors.Is(err, binding.ErrNotFound) {
				t.Fatalf("migration created a binding: %v", err)
			}
		})
	}
}

func TestUnknownTargetStateBlocksWithoutBinding(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "server error", status: http.StatusServiceUnavailable, body: `{}`},
		{name: "malformed", status: http.StatusOK, body: `not-json`},
		{name: "missing tokens", status: http.StatusOK, body: `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, authenticationBody("legacy-token", "LegacyPlayer", true))
			}))
			defer legacy.Close()
			armorial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, tt.status, tt.body)
			}))
			defer armorial.Close()
			env := newTestGateway(t, armorial.URL, legacy.URL, race.IntentUnspecified)
			response := env.perform(http.MethodPost, "/authserver/authenticate", `{"username":"Player","password":"secret"}`)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", response.Code)
			}
			if _, err := env.store.GetBinding(context.Background(), "Player"); !errors.Is(err, binding.ErrNotFound) {
				t.Fatalf("unknown state created a binding: %v", err)
			}
		})
	}
}

func TestTargetTimeoutBlocksWithoutBinding(t *testing.T) {
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, authenticationBody("legacy-token", "LegacyPlayer", true))
	}))
	defer legacy.Close()
	release := make(chan struct{})
	armorial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer armorial.Close()
	defer close(release)
	env := newTestGateway(t, armorial.URL, legacy.URL, race.IntentUnspecified)
	env.gateway.client.Timeout = 20 * time.Millisecond
	response := env.perform(http.MethodPost, "/authserver/authenticate", `{"username":"Player","password":"secret"}`)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	if _, err := env.store.GetBinding(context.Background(), "Player"); !errors.Is(err, binding.ErrNotFound) {
		t.Fatalf("timeout created a binding: %v", err)
	}
}

func TestInvalidCredentialsReturnStandardForbiddenWithoutBinding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusForbidden, `{"error":"ForbiddenOperationException","errorMessage":"Invalid credentials. Invalid username or password."}`)
	}))
	defer upstream.Close()
	env := newTestGateway(t, upstream.URL, upstream.URL, race.IntentUnspecified)
	response := env.perform(http.MethodPost, "/authserver/authenticate", `{"username":"Player","password":"wrong"}`)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "ForbiddenOperationException") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if _, err := env.store.GetBinding(context.Background(), "Player"); !errors.Is(err, binding.ErrNotFound) {
		t.Fatalf("invalid credentials created a binding: %v", err)
	}
}

func TestExistingLegacyBindingRoutesOnlyLegacy(t *testing.T) {
	var armorialCalls atomic.Int32
	armorial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		armorialCalls.Add(1)
		writeJSON(w, http.StatusOK, authenticationBody("armorial-token", "ArmorialPlayer", true))
	}))
	defer armorial.Close()
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, authenticationBody("legacy-token", "LegacyPlayer", true))
	}))
	defer legacy.Close()
	env := newTestGateway(t, armorial.URL, legacy.URL, race.IntentUnspecified)
	if _, err := env.store.BindIfAbsent(context.Background(), "Player", binding.Legacy, "migration-1"); err != nil {
		t.Fatal(err)
	}
	response := env.perform(http.MethodPost, "/authserver/authenticate", `{"username":"Player","password":"secret"}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "legacy-token") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if armorialCalls.Load() != 0 {
		t.Fatalf("armorial calls = %d, want 0", armorialCalls.Load())
	}
}

func TestRefreshAndValidateUseTokenBinding(t *testing.T) {
	var refreshCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/authserver/authenticate":
			writeJSON(w, http.StatusOK, authenticationBody("old-token", "Player", true))
		case "/authserver/refresh":
			refreshCalls.Add(1)
			writeJSON(w, http.StatusOK, authenticationBody("new-token", "Player", true))
		case "/authserver/validate":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	env := newTestGateway(t, upstream.URL, upstream.URL, race.IntentUnspecified)
	if response := env.perform(http.MethodPost, "/authserver/authenticate", `{"username":"Player","password":"secret"}`); response.Code != http.StatusOK {
		t.Fatalf("authenticate = %d %s", response.Code, response.Body.String())
	}
	response := env.perform(http.MethodPost, "/authserver/refresh", `{"accessToken":"old-token","clientToken":"client"}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "new-token") {
		t.Fatalf("refresh = %d %s", response.Code, response.Body.String())
	}
	if _, err := env.store.GetBindingByToken(context.Background(), "new-token"); err != nil {
		t.Fatalf("new token binding missing: %v", err)
	}
	response = env.perform(http.MethodPost, "/authserver/validate", `{"accessToken":"new-token"}`)
	if response.Code != http.StatusNoContent || refreshCalls.Load() != 1 {
		t.Fatalf("validate = %d, refresh calls = %d", response.Code, refreshCalls.Load())
	}
}

func TestUnknownTokenIsForcedToReauthenticate(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	env := newTestGateway(t, upstream.URL, upstream.URL, race.IntentUnspecified)
	for _, path := range []string{"/authserver/validate", "/authserver/refresh", "/authserver/invalidate"} {
		response := env.perform(http.MethodPost, path, `{"accessToken":"unknown"}`)
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s status = %d, want 403", path, response.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("unknown token reached upstream %d times", calls.Load())
	}
}

func TestSignoutUsesPermanentBinding(t *testing.T) {
	var armorialCalls atomic.Int32
	armorial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		armorialCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer armorial.Close()
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer legacy.Close()
	env := newTestGateway(t, armorial.URL, legacy.URL, race.IntentUnspecified)
	if _, err := env.store.BindIfAbsent(context.Background(), "Player", binding.Armorial, ""); err != nil {
		t.Fatal(err)
	}
	response := env.perform(http.MethodPost, "/authserver/signout", `{"username":"Player","password":"secret"}`)
	if response.Code != http.StatusNoContent || armorialCalls.Load() != 1 {
		t.Fatalf("signout = %d, armorial calls = %d", response.Code, armorialCalls.Load())
	}
}

func TestRootMetadataComesFromArmorialAndRewritesAPILocation(t *testing.T) {
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer legacy.Close()
	armorial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Authlib-Injector-API-Location", "https://upstream.invalid")
		writeJSON(w, http.StatusOK, `{"meta":{"implementationName":"Armorial"}}`)
	}))
	defer armorial.Close()
	env := newTestGateway(t, armorial.URL, legacy.URL, race.IntentUnspecified)
	response := env.perform(http.MethodGet, "/", "")
	if response.Code != http.StatusOK || response.Header().Get("X-Authlib-Injector-API-Location") != "https://auth.example.com" {
		t.Fatalf("root = %d location=%q", response.Code, response.Header().Get("X-Authlib-Injector-API-Location"))
	}
}

func TestGatewayRejectsInvalidRequestsAndInternalRoutes(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	env := newTestGateway(t, upstream.URL, upstream.URL, race.IntentUnspecified)
	if response := env.perform(http.MethodPost, "/authserver/authenticate", `not-json`); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON status = %d", response.Code)
	}
	if response := env.perform(http.MethodGet, "/internal/migrations/commit", ""); response.Code != http.StatusNotFound {
		t.Fatalf("internal route status = %d", response.Code)
	}
	if calls.Load() != 0 {
		t.Fatalf("rejected requests reached upstream %d times", calls.Load())
	}
}

func TestGatewayEnforcesRequestBodyLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	env := newTestGateway(t, upstream.URL, upstream.URL, race.IntentUnspecified)
	env.gateway.maxRequestBody = 16
	response := env.perform(http.MethodPost, "/authserver/authenticate", `{"username":"Player","password":"secret"}`)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.Code)
	}
}

func newTestGateway(t *testing.T, armorialURL, legacyURL string, intent race.Intent) *testGateway {
	t.Helper()
	store, err := binding.OpenSQLite(filepath.Join(t.TempDir(), "scribe.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	gateway, err := New(Options{
		PublicBaseURL:   "https://auth.example.com",
		LegacyBaseURL:   legacyURL,
		ArmorialBaseURL: armorialURL,
		RequestTimeout:  time.Second,
		MaxRequestBody:  1 << 20,
		MaxResponseBody: 1 << 20,
		Circuit: circuitbreaker.Settings{
			FailureThreshold:    100,
			OpenTimeout:         time.Minute,
			HalfOpenMaxRequests: 1,
		},
		MigrationIntent: intent,
	}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return &testGateway{gateway: gateway, store: store, handler: gateway.Handler()}
}

func (e *testGateway) perform(method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "https://scribe.example.com"+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	e.handler.ServeHTTP(response, request)
	return response
}

func authenticationBody(token, name string, withProfile bool) string {
	body := map[string]any{
		"accessToken": token,
		"clientToken": "client",
	}
	if withProfile {
		body["selectedProfile"] = map[string]string{"id": "12345678123412341234567812345678", "name": name}
	}
	encoded, _ := json.Marshal(body)
	return string(encoded)
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}
