package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRegistryExposesLowCardinalityMetrics(t *testing.T) {
	registry := New()
	registry.Authentication("NEW_WINS", "")
	registry.Authentication("secret-token", "password")
	registry.AuthenticationBlocked("migration_pending")
	registry.AuthenticationBlocked("secret-token")
	registry.Migration("success", "commit", 1500*time.Millisecond)
	registry.Migration("secret-token", "secret-phase", time.Second)
	registry.Recovery("blocked", "APPLYING", 2*time.Second)
	registry.RecoveryScan("success")

	response := httptest.NewRecorder()
	registry.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", response.Code)
	}
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain; version=0.0.4") {
		t.Fatalf("metrics content type = %q", got)
	}
	body := response.Body.String()
	for _, want := range []string{
		`scribe_authentication_decisions_total{outcome="NEW_WINS",reason="none"} 1`,
		`scribe_authentication_decisions_total{outcome="unknown",reason="unknown"} 1`,
		`scribe_authentication_blocked_total{reason="migration_pending"} 1`,
		`scribe_authentication_blocked_total{reason="unknown"} 1`,
		`scribe_migration_operations_total{result="success",phase="commit"} 1`,
		`scribe_migration_operations_total{result="unknown",phase="unknown"} 1`,
		`scribe_migration_duration_seconds_bucket{result="success",phase="commit",le="1"} 0`,
		`scribe_migration_duration_seconds_bucket{result="success",phase="commit",le="2.5"} 1`,
		`scribe_migration_duration_seconds_sum{result="success",phase="commit"} 1.5`,
		`scribe_recovery_operations_total{result="blocked",state="APPLYING"} 1`,
		`scribe_recovery_duration_seconds_count{result="blocked",state="APPLYING"} 1`,
		`scribe_recovery_scans_total{result="success"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}
	for _, secret := range []string{"secret-token", "password", "secret-phase"} {
		if strings.Contains(body, secret) {
			t.Fatalf("metrics leaked %q:\n%s", secret, body)
		}
	}
}

func TestRegistryRejectsNonGETRequests(t *testing.T) {
	response := httptest.NewRecorder()
	New().Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST /metrics = %d, Allow %q", response.Code, response.Header().Get("Allow"))
	}
}

func TestRegistrySupportsConcurrentUpdates(t *testing.T) {
	registry := New()
	const workers = 16
	const observations = 100
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range observations {
				registry.Authentication("BLOCK_RETRY", "target_state_unknown")
				registry.Migration("failed", "snapshot", time.Millisecond)
				registry.Recovery("blocked", "RECONCILING", time.Millisecond)
			}
		}()
	}
	wg.Wait()

	response := httptest.NewRecorder()
	registry.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	for _, want := range []string{
		`scribe_authentication_decisions_total{outcome="BLOCK_RETRY",reason="target_state_unknown"} 1600`,
		`scribe_migration_operations_total{result="failed",phase="snapshot"} 1600`,
		`scribe_recovery_operations_total{result="blocked",state="RECONCILING"} 1600`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("concurrent metrics missing %q:\n%s", want, body)
		}
	}
}

func TestNilRegistryIsNoop(t *testing.T) {
	var registry *Registry
	registry.Authentication("NEW_WINS", "")
	registry.AuthenticationBlocked("migration_pending")
	registry.Migration("success", "commit", time.Second)
	registry.Recovery("success", "LOCKED", time.Second)
	registry.RecoveryScan("success")
}
