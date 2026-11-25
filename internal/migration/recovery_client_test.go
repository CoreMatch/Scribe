package migration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRecoveryOperationsSendAuthorizedContractRequests(t *testing.T) {
	tests := []struct {
		action string
		status string
		call   func(*Client) (MigrationStatus, error)
	}{
		{action: "reconcile", status: MigrationApplying, call: func(client *Client) (MigrationStatus, error) {
			return client.Reconcile(context.Background(), testMigrationID)
		}},
		{action: "rollback", status: MigrationRolledBack, call: func(client *Client) (MigrationStatus, error) {
			return client.Rollback(context.Background(), testMigrationID)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/internal/migrations/"+testMigrationID+"/"+tt.action {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer service-token" || r.Header.Get("Idempotency-Key") != "" {
					t.Errorf("unexpected headers: %v", r.Header)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || len(body) != 0 {
					t.Errorf("operation body = %q, %v", body, err)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"success": true,
					"data": MigrationStatus{
						MigrationID: testMigrationID,
						Status:      tt.status,
						UpdatedAt:   time.Now().UTC(),
						Resources:   []MigrationResource{},
					},
				})
			}))
			defer server.Close()
			client := localCommitClient(t, server, ClientOptions{})
			status, err := tt.call(client)
			if err != nil || status.Status != tt.status || status.MigrationID != testMigrationID {
				t.Fatalf("operation = %+v, %v", status, err)
			}
		})
	}
}

func TestRecoveryOperationsMapErrorsWithoutLeakingMessages(t *testing.T) {
	tests := []struct {
		name   string
		action string
		status int
		code   string
		want   error
	}{
		{name: "reconcile not found", action: "reconcile", status: http.StatusNotFound, code: "migration_not_found", want: ErrMigrationNotFound},
		{name: "rollback not found", action: "rollback", status: http.StatusNotFound, code: "migration_not_found", want: ErrMigrationNotFound},
		{name: "rollback forbidden", action: "rollback", status: http.StatusForbidden, code: "forbidden", want: ErrForbidden},
		{name: "committed rollback", action: "rollback", status: http.StatusConflict, code: "migration_rollback_not_allowed", want: ErrRollbackNotAllowed},
		{name: "invalid operation", action: "reconcile", status: http.StatusBadRequest, code: "invalid_request", want: ErrInvalidOperation},
		{name: "upstream failure", action: "rollback", status: http.StatusInternalServerError, code: "migration_rollback_failed", want: ErrUpstream},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "code": tt.code, "message": "sensitive upstream detail"})
			}))
			defer server.Close()
			client := localCommitClient(t, server, ClientOptions{})
			var err error
			if tt.action == "rollback" {
				_, err = client.Rollback(context.Background(), testMigrationID)
			} else {
				_, err = client.Reconcile(context.Background(), testMigrationID)
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("operation error = %v, want %v", err, tt.want)
			}
			var apiError *APIError
			if !errors.As(err, &apiError) || apiError.StatusCode != tt.status || apiError.Code != tt.code {
				t.Fatalf("API error = %+v", apiError)
			}
			if strings.Contains(err.Error(), "sensitive upstream detail") {
				t.Fatalf("operation error leaked upstream detail: %v", err)
			}
		})
	}
}

func TestRecoveryOperationsRejectInvalidIDsBeforeSending(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("invalid operation reached upstream")
	}))
	defer server.Close()
	client := localCommitClient(t, server, ClientOptions{})
	if _, err := client.Reconcile(context.Background(), "invalid"); !errors.Is(err, ErrInvalidOperation) {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if _, err := client.Rollback(context.Background(), "invalid"); !errors.Is(err, ErrInvalidOperation) {
		t.Fatalf("Rollback() error = %v", err)
	}
}

func TestRecoveryOperationsRejectInvalidStatusResponses(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "wrong migration", mutate: func(data map[string]any) { data["migration_id"] = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" }},
		{name: "unknown status", mutate: func(data map[string]any) { data["status"] = "invalid" }},
		{name: "missing timestamp", mutate: func(data map[string]any) { data["updated_at"] = "" }},
		{name: "missing resources", mutate: func(data map[string]any) { delete(data, "resources") }},
		{name: "invalid resource kind", mutate: func(data map[string]any) {
			data["resources"] = []map[string]any{{"kind": "invalid", "resource_id": "resource", "ownership": "preexisting", "present": true}}
		}},
		{name: "missing resource ownership", mutate: func(data map[string]any) {
			data["resources"] = []map[string]any{{"kind": "profile", "resource_id": "resource", "present": true}}
		}},
		{name: "invalid error stage", mutate: func(data map[string]any) {
			data["last_error"] = map[string]any{"code": "failed", "message": "detail", "stage": "invalid"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				data := map[string]any{
					"migration_id": testMigrationID,
					"status":       MigrationApplying,
					"updated_at":   time.Now().UTC(),
					"resources":    []any{},
				}
				tt.mutate(data)
				_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
			}))
			defer server.Close()
			client := localCommitClient(t, server, ClientOptions{})
			if _, err := client.Reconcile(context.Background(), testMigrationID); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("Reconcile() error = %v", err)
			}
		})
	}
}

func TestRecoveryStatusValidatesCommittedResultAndDropsErrorMessage(t *testing.T) {
	staged := stagedFromSnapshot(t, testMigrationID, "LegacyPlayer", validDigestSnapshot(t))
	result := successfulCommitResult(staged, "uploaded", "uploaded")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"migration_id": testMigrationID,
				"status":       MigrationCommitted,
				"updated_at":   time.Now().UTC(),
				"resources":    []any{},
				"result":       result,
				"last_error":   map[string]any{"code": "old_failure", "message": "sensitive upstream detail", "stage": "verify"},
			},
		})
	}))
	defer server.Close()
	client := localCommitClient(t, server, ClientOptions{})
	status, err := client.Reconcile(context.Background(), testMigrationID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Result == nil || status.Result.Session.AccessToken != "new-access-token" || status.LastError == nil || status.LastError.Code != "old_failure" {
		t.Fatalf("status = %+v", status)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "sensitive upstream detail") {
		t.Fatalf("status leaked upstream message: %s", encoded)
	}
}

func TestRecoveryOperationRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 1024))
	}))
	defer server.Close()
	client := localCommitClient(t, server, ClientOptions{MaxResponseBody: 16})
	if _, err := client.Reconcile(context.Background(), testMigrationID); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("Reconcile() error = %v", err)
	}
}
