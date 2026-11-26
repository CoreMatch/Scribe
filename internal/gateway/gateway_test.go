package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/corematch/scribe/internal/metrics"
	"github.com/corematch/scribe/internal/migration"
	"github.com/corematch/scribe/internal/protocol"
	"github.com/corematch/scribe/internal/race"
)

type testGateway struct {
	gateway *Gateway
	store   *binding.SQLiteStore
	handler http.Handler
	metrics *metrics.Registry
}

type fakeMigrator struct {
	result migration.Result
	err    error
	calls  []migration.Input
}

func (f *fakeMigrator) Migrate(_ context.Context, input migration.Input) (migration.Result, error) {
	f.calls = append(f.calls, input)
	return f.result, f.err
}

type migratorFunc func(context.Context, migration.Input) (migration.Result, error)

func (f migratorFunc) Migrate(ctx context.Context, input migration.Input) (migration.Result, error) {
	return f(ctx, input)
}

type fakePendingChecker struct {
	pending bool
	err     error
	calls   []string
}

func (f *fakePendingChecker) Pending(_ context.Context, identifier string) (bool, error) {
	f.calls = append(f.calls, identifier)
	return f.pending, f.err
}

func successfulMigration() migration.Result {
	return migration.Result{
		MigrationID: "00000000-0000-4000-8000-000000000001",
		Commit: migration.CommitResult{
			MigrationID: "00000000-0000-4000-8000-000000000001",
			Status:      "committed",
			Session: migration.Session{
				AccessToken:     "migrated-token",
				ClientToken:     "migrated-client",
				SelectedProfile: protocol.Profile{ID: "12345678123412341234567812345678", Name: "LegacyPlayer"},
			},
		},
	}
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
	env.gateway.migrationPending = &fakePendingChecker{}

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

	legacyCallsBeforeRetry := legacyCalls.Load()
	response = env.perform(http.MethodPost, "/authserver/authenticate", `{"username":"Player","password":"secret"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("second authenticate = %d %s", response.Code, response.Body.String())
	}
	if legacyCalls.Load() != legacyCallsBeforeRetry {
		t.Fatalf("legacy calls = %d, want %d", legacyCalls.Load(), legacyCallsBeforeRetry)
	}
	metricResponse := httptest.NewRecorder()
	env.metrics.Handler().ServeHTTP(metricResponse, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(metricResponse.Body.String(), `scribe_authentication_decisions_total{outcome="NEW_WINS",reason="none"} 1`) {
		t.Fatalf("authentication decision metric = %s", metricResponse.Body.String())
	}
}

func TestAuthenticateDecodesClientRequestedGzip(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte(authenticationBody("gzip-token", "Player", true))); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "gzip" {
			t.Errorf("Accept-Encoding = %q, want transport gzip negotiation", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(compressed.Bytes())
	}))
	defer upstream.Close()
	env := newTestGateway(t, upstream.URL, upstream.URL, race.IntentUnspecified)
	request := httptest.NewRequest(http.MethodPost, "https://scribe.example.com/authserver/authenticate", strings.NewReader(`{"username":"Player","password":"secret"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	env.handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "gzip-token") {
		t.Fatalf("gzip authenticate = %d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Encoding") != "" {
		t.Fatalf("client response remained compressed: %q", response.Header().Get("Content-Encoding"))
	}
	if _, err := env.store.GetBindingByToken(context.Background(), "gzip-token"); err != nil {
		t.Fatalf("gzip authentication token binding missing: %v", err)
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
	legacyCallsBeforeFailure := legacyCalls.Load()
	fail.Store(true)
	response := env.perform(http.MethodPost, "/authserver/authenticate", `{"username":"Player","password":"secret"}`)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("failed winner status = %d, want 502", response.Code)
	}
	if legacyCalls.Load() != legacyCallsBeforeFailure {
		t.Fatalf("legacy was called after binding: %d", legacyCalls.Load())
	}
	bound, err := env.store.GetBinding(context.Background(), "Player")
	if err != nil || bound.Winner != binding.Armorial {
		t.Fatalf("binding changed: %+v, %v", bound, err)
	}
}

func TestNewWinnerIsBlockedByPendingMigration(t *testing.T) {
	for _, pendingErr := range []error{nil, errors.New("redis unavailable")} {
		t.Run(fmt.Sprint(pendingErr), func(t *testing.T) {
			legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusForbidden, `{"error":"ForbiddenOperationException"}`)
			}))
			defer legacy.Close()
			armorial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, authenticationBody("premature-token", "Player", true))
			}))
			defer armorial.Close()
			env := newTestGateway(t, armorial.URL, legacy.URL, race.IntentUnspecified)
			checker := &fakePendingChecker{pending: pendingErr == nil, err: pendingErr}
			env.gateway.migrationPending = checker
			response := env.perform(http.MethodPost, "/authserver/authenticate", `{"username":"Player@Example.COM","password":"secret"}`)
			if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "premature-token") {
				t.Fatalf("pending migration response = %d %s", response.Code, response.Body.String())
			}
			if len(checker.calls) != 1 || checker.calls[0] != "Player@example.com" {
				t.Fatalf("pending checks = %#v", checker.calls)
			}
			if _, err := env.store.GetBinding(context.Background(), "Player@example.com"); !errors.Is(err, binding.ErrNotFound) {
				t.Fatalf("pending migration created a binding: %v", err)
			}
		})
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
		{name: "reuse core defaults to import", armorial: authenticationBody("armorial-token", "Player", false)},
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

func TestAuthenticateMigratesLegacyUserBeforeBinding(t *testing.T) {
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"accessToken":"legacy-token","clientToken":"legacy-client","selectedProfile":{"id":"12345678123412341234567812345678","name":"LegacyPlayer"},"user":{"id":"legacy-user","properties":[{"name":"legacy-role","value":"member"}]}}`)
	}))
	defer legacy.Close()
	armorial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusForbidden, `{"error":"ForbiddenOperationException"}`)
	}))
	defer armorial.Close()
	env := newTestGateway(t, armorial.URL, legacy.URL, race.IntentUnspecified)
	migrator := &fakeMigrator{result: successfulMigration()}
	env.gateway.migrator = migrator
	checker := &fakePendingChecker{pending: true}
	env.gateway.migrationPending = checker

	response := env.perform(http.MethodPost, "/authserver/authenticate", `{"username":"Player@Example.COM","password":"secret"}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "migrated-token") {
		t.Fatalf("migration response = %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "legacy-token") || strings.Contains(response.Body.String(), "armorial-token") {
		t.Fatalf("migration leaked an upstream token: %s", response.Body.String())
	}
	if len(migrator.calls) != 1 {
		t.Fatalf("migration calls = %d, want 1", len(migrator.calls))
	}
	if len(checker.calls) != 0 {
		t.Fatalf("migration retry was blocked by pending check: %#v", checker.calls)
	}
	input := migrator.calls[0]
	if input.Identifier != "Player@example.com" || input.Password != "secret" || input.SelectedProfile.Name != "LegacyPlayer" {
		t.Fatalf("unexpected migration input: %#v", input)
	}
	if input.LegacyUserID != "legacy-user" || input.LegacyUserProperties["legacy-role"] != "member" {
		t.Fatalf("legacy user metadata was not passed: %#v", input)
	}
	bound, err := env.store.GetBinding(context.Background(), "Player@example.com")
	if err != nil || bound.Winner != binding.Armorial || bound.MigrationID != migrator.result.MigrationID {
		t.Fatalf("binding = %+v, %v", bound, err)
	}
	if _, err := env.store.GetBindingByToken(context.Background(), "migrated-token"); err != nil {
		t.Fatalf("migrated token binding missing: %v", err)
	}
}

func TestAuthenticateDefaultsProfilelessTargetToMigration(t *testing.T) {
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, authenticationBody("legacy-token", "LegacyPlayer", true))
	}))
	defer legacy.Close()
	armorial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, authenticationBody("armorial-token", "Player", false))
	}))
	defer armorial.Close()
	env := newTestGateway(t, armorial.URL, legacy.URL, race.IntentUnspecified)
	migrator := &fakeMigrator{result: successfulMigration()}
	env.gateway.migrator = migrator

	response := env.perform(http.MethodPost, "/authserver/authenticate", `{"username":"Player","password":"secret"}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "migrated-token") {
		t.Fatalf("migration response = %d %s", response.Code, response.Body.String())
	}
	if len(migrator.calls) != 1 {
		t.Fatalf("migration calls = %d, want 1", len(migrator.calls))
	}
	if strings.Contains(response.Body.String(), "legacy-token") || strings.Contains(response.Body.String(), "armorial-token") {
		t.Fatalf("migration response leaked an upstream token: %s", response.Body.String())
	}
}

func TestMigrationFailuresDoNotBindOrLeakTokens(t *testing.T) {
	tests := []struct {
		name   string
		result migration.Result
		err    error
	}{
		{name: "migration error", err: errors.New("commit failed")},
		{name: "uncommitted result", result: migration.Result{MigrationID: "migration", Commit: migration.CommitResult{MigrationID: "migration", Status: "applying"}}},
		{name: "incomplete session", result: migration.Result{MigrationID: "migration", Commit: migration.CommitResult{MigrationID: "migration", Status: "committed"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, authenticationBody("legacy-token", "LegacyPlayer", true))
			}))
			defer legacy.Close()
			armorial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusForbidden, `{"error":"ForbiddenOperationException"}`)
			}))
			defer armorial.Close()
			env := newTestGateway(t, armorial.URL, legacy.URL, race.IntentUnspecified)
			env.gateway.migrator = &fakeMigrator{result: tt.result, err: tt.err}
			response := env.perform(http.MethodPost, "/authserver/authenticate", `{"username":"Player","password":"secret"}`)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", response.Code)
			}
			if strings.Contains(response.Body.String(), "token") {
				t.Fatalf("migration failure leaked a token: %s", response.Body.String())
			}
			if _, err := env.store.GetBinding(context.Background(), "Player"); !errors.Is(err, binding.ErrNotFound) {
				t.Fatalf("failed migration created a binding: %v", err)
			}
		})
	}
}

func TestCommittedMigrationCannotReplaceExistingBinding(t *testing.T) {
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, authenticationBody("legacy-token", "LegacyPlayer", true))
	}))
	defer legacy.Close()
	armorial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusForbidden, `{"error":"ForbiddenOperationException"}`)
	}))
	defer armorial.Close()
	env := newTestGateway(t, armorial.URL, legacy.URL, race.IntentUnspecified)
	env.gateway.migrator = migratorFunc(func(context.Context, migration.Input) (migration.Result, error) {
		if _, err := env.store.BindIfAbsent(context.Background(), "Player", binding.Legacy, "existing-migration"); err != nil {
			return migration.Result{}, err
		}
		return successfulMigration(), nil
	})
	response := env.perform(http.MethodPost, "/authserver/authenticate", `{"username":"Player","password":"secret"}`)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "migrated-token") {
		t.Fatalf("conflicting migration response = %d %s", response.Code, response.Body.String())
	}
	bound, err := env.store.GetBinding(context.Background(), "Player")
	if err != nil || bound.Winner != binding.Legacy || bound.MigrationID != "existing-migration" {
		t.Fatalf("binding changed after conflict: %+v, %v", bound, err)
	}
}

func TestMigrationUserMetadata(t *testing.T) {
	id, properties := migrationUser(&protocol.User{ID: "user", Properties: []map[string]string{
		{"value": "ignored"},
		{"name": "role", "value": "member"},
		{"name": "empty"},
	}})
	if id != "user" || len(properties) != 2 || properties["role"] != "member" || properties["empty"] != "" {
		t.Fatalf("unexpected metadata: %q %#v", id, properties)
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

func TestProfilelessTargetSuccessDoesNotIssueUnboundToken(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "legacy rejects credentials", status: http.StatusForbidden, body: `{"error":"ForbiddenOperationException"}`},
		{name: "legacy has no profile", status: http.StatusOK, body: authenticationBody("legacy-token", "LegacyPlayer", false)},
	} {
		t.Run(test.name, func(t *testing.T) {
			legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, test.status, test.body)
			}))
			defer legacy.Close()
			armorial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, authenticationBody("unbound-token", "Player", false))
			}))
			defer armorial.Close()
			env := newTestGateway(t, armorial.URL, legacy.URL, race.IntentUnspecified)
			response := env.perform(http.MethodPost, "/authserver/authenticate", `{"username":"Player","password":"secret"}`)
			if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "unbound-token") {
				t.Fatalf("profileless success response = %d %s", response.Code, response.Body.String())
			}
			if _, err := env.store.GetBinding(context.Background(), "Player"); !errors.Is(err, binding.ErrNotFound) {
				t.Fatalf("profileless success created a binding: %v", err)
			}
		})
	}
}

func TestMalformedTargetProfilesBlockWithoutBinding(t *testing.T) {
	for _, body := range []string{
		`{"accessToken":"target-token","clientToken":"client","selectedProfile":{"id":"not-a-uuid","name":"Player"}}`,
		`{"accessToken":"target-token","clientToken":"client","selectedProfile":{"id":"12345678123412341234567812345678","name":"Player"},"availableProfiles":[{"id":"bad","name":"Other"}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, authenticationBody("legacy-token", "LegacyPlayer", true))
			}))
			defer legacy.Close()
			armorial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, body)
			}))
			defer armorial.Close()
			env := newTestGateway(t, armorial.URL, legacy.URL, race.IntentUnspecified)
			response := env.perform(http.MethodPost, "/authserver/authenticate", `{"username":"Player","password":"secret"}`)
			if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "target-token") {
				t.Fatalf("malformed target response = %d %s", response.Code, response.Body.String())
			}
			if _, err := env.store.GetBinding(context.Background(), "Player"); !errors.Is(err, binding.ErrNotFound) {
				t.Fatalf("malformed target response created a binding: %v", err)
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

func TestInvalidateUsesTokenBinding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	env := newTestGateway(t, upstream.URL, upstream.URL, race.IntentUnspecified)
	if _, err := env.store.BindIfAbsent(context.Background(), "Player", binding.Armorial, ""); err != nil {
		t.Fatal(err)
	}
	if err := env.store.BindToken(context.Background(), "bound-token", "Player", binding.Armorial); err != nil {
		t.Fatal(err)
	}
	response := env.perform(http.MethodPost, "/authserver/invalidate", `{"accessToken":"bound-token"}`)
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
		t.Fatalf("invalidate = %d %s", response.Code, response.Body.String())
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
	for _, test := range []struct {
		path   string
		status int
	}{
		{path: "/authserver/validate", status: http.StatusForbidden},
		{path: "/authserver/refresh", status: http.StatusForbidden},
		{path: "/authserver/invalidate", status: http.StatusNoContent},
	} {
		response := env.perform(http.MethodPost, test.path, `{"accessToken":"unknown"}`)
		if response.Code != test.status {
			t.Fatalf("%s status = %d, want %d", test.path, response.Code, test.status)
		}
		if test.status == http.StatusForbidden && !strings.Contains(response.Body.String(), "ForbiddenOperationException") {
			t.Fatalf("%s response = %s", test.path, response.Body.String())
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

func TestSignoutBlocksPendingMigration(t *testing.T) {
	for _, pendingErr := range []error{nil, errors.New("redis unavailable")} {
		t.Run(fmt.Sprint(pendingErr), func(t *testing.T) {
			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer upstream.Close()
			env := newTestGateway(t, upstream.URL, upstream.URL, race.IntentUnspecified)
			checker := &fakePendingChecker{pending: pendingErr == nil, err: pendingErr}
			env.gateway.migrationPending = checker
			response := env.perform(http.MethodPost, "/authserver/signout", `{"username":"Player","password":"secret"}`)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("signout status = %d, want 503", response.Code)
			}
			if upstreamCalls.Load() != 0 || len(checker.calls) != 1 || checker.calls[0] != "Player" {
				t.Fatalf("pending signout reached upstream: calls=%d checks=%#v", upstreamCalls.Load(), checker.calls)
			}
		})
	}
}

func TestSignoutWithoutPendingMigrationUsesLegacy(t *testing.T) {
	var legacyCalls atomic.Int32
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		legacyCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer legacy.Close()
	armorial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer armorial.Close()
	env := newTestGateway(t, armorial.URL, legacy.URL, race.IntentUnspecified)
	env.gateway.migrationPending = &fakePendingChecker{}
	response := env.perform(http.MethodPost, "/authserver/signout", `{"username":"Player","password":"secret"}`)
	if response.Code != http.StatusNoContent || legacyCalls.Load() != 1 {
		t.Fatalf("signout = %d, legacy calls = %d", response.Code, legacyCalls.Load())
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

func TestRemoveHopByHopHeadersHonorsConnectionTokens(t *testing.T) {
	header := http.Header{
		"Connection":       {"X-Client-Hop, X-Other-Hop", "Keep-Alive"},
		"X-Client-Hop":     {"secret"},
		"X-Other-Hop":      {"secret"},
		"Keep-Alive":       {"timeout=5"},
		"Proxy-Connection": {"keep-alive"},
		"X-End-To-End":     {"preserved"},
	}
	removeHopByHopHeaders(header)
	for _, name := range []string{"Connection", "X-Client-Hop", "X-Other-Hop", "Keep-Alive", "Proxy-Connection"} {
		if values := header.Values(name); len(values) != 0 {
			t.Fatalf("hop-by-hop header %s survived: %#v", name, values)
		}
	}
	if header.Get("X-End-To-End") != "preserved" {
		t.Fatalf("end-to-end header was removed: %#v", header)
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

func TestGatewayRequiresPendingCheckerWithMigrator(t *testing.T) {
	env := newTestGateway(t, "http://armorial.invalid", "http://legacy.invalid", race.IntentUnspecified)
	_, err := New(Options{
		PublicBaseURL:   "https://auth.example.com",
		LegacyBaseURL:   "http://legacy.invalid",
		ArmorialBaseURL: "http://armorial.invalid",
		RequestTimeout:  time.Second,
		Migrator:        &fakeMigrator{},
	}, env.store, nil)
	if err == nil || !strings.Contains(err.Error(), "pending checker") {
		t.Fatalf("New() error = %v, want missing pending checker error", err)
	}
}

func newTestGateway(t *testing.T, armorialURL, legacyURL string, intent race.Intent) *testGateway {
	t.Helper()
	store, err := binding.OpenSQLite(filepath.Join(t.TempDir(), "scribe.db"), 15*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	registry := metrics.New()
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
		Metrics:         registry,
	}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return &testGateway{gateway: gateway, store: store, handler: gateway.Handler(), metrics: registry}
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
