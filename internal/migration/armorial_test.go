package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/corematch/scribe/internal/assets"
	"github.com/corematch/scribe/internal/protocol"
)

func TestCommitSendsContractMetadataAndNormalizedAssets(t *testing.T) {
	snapshot := validDigestSnapshot(t)
	identifier := "Player@Example.COM"
	staged := stagedFromSnapshot(t, testMigrationID, "Player@example.com", snapshot)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/migrations/commit" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer service-token" || r.Header.Get("Idempotency-Key") != testMigrationID {
			t.Errorf("service headers = %v", r.Header)
		}
		reader, err := r.MultipartReader()
		if err != nil {
			t.Errorf("MultipartReader() error = %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		parts := map[string][]byte{}
		contentTypes := map[string]string{}
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Errorf("NextPart() error = %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			data, err := io.ReadAll(part)
			if err != nil {
				t.Errorf("ReadAll() error = %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			parts[part.FormName()] = data
			contentTypes[part.FormName()] = part.Header.Get("Content-Type")
		}
		var metadata CommitMetadata
		if err := json.Unmarshal(parts["metadata"], &metadata); err != nil {
			t.Errorf("metadata JSON = %q, %v", parts["metadata"], err)
		}
		if metadata.MigrationID != testMigrationID || metadata.Identifier != "Player@example.com" || metadata.Email != "Player@example.com" ||
			metadata.Password != "secret-password" || metadata.Username != "LegacyPlayer" || metadata.ProfileName != "LegacyPlayer" ||
			metadata.ProfileUUID != snapshotProfileID || metadata.ProfileModel != "slim" || metadata.SkinSync ||
			metadata.SkinSHA256 != snapshot.Assets["SKIN"].PixelSHA256 || metadata.CapeSHA256 != snapshot.Assets["CAPE"].PixelSHA256 ||
			metadata.LegacyUserID != "legacy-user" || metadata.LegacyUserProperties["preferredLanguage"] != "zh-CN" {
			t.Errorf("metadata = %+v", metadata)
		}
		if contentTypes["metadata"] != "application/json" || contentTypes["skin"] != "image/png" || contentTypes["cape"] != "image/png" {
			t.Errorf("part content types = %v", contentTypes)
		}
		if !bytes.Equal(parts["skin"], snapshot.Assets["SKIN"].PNG) || !bytes.Equal(parts["cape"], snapshot.Assets["CAPE"].PNG) {
			t.Error("multipart assets differ from normalized snapshot")
		}
		writeCommitResponse(t, w, staged, "uploaded", "uploaded")
	}))
	defer server.Close()
	client := localCommitClient(t, server, ClientOptions{})

	result, err := client.Commit(context.Background(), CommitRequest{
		Staged:               staged,
		Identifier:           identifier,
		Password:             "secret-password",
		LegacyUserID:         "legacy-user",
		LegacyUserProperties: map[string]string{"preferredLanguage": "zh-CN"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.MigrationID != testMigrationID || result.Status != "committed" || result.Session.AccessToken != "new-access-token" ||
		result.Session.ClientToken != "new-client-token" || result.Session.SelectedProfile.Name != "LegacyPlayer" {
		t.Fatalf("Commit() = %+v", result)
	}
}

func TestCommitOmitsPlaceholderEmailAndAbsentAssets(t *testing.T) {
	snapshot := Snapshot{
		Profile: protocol.SignedProfile{ID: snapshotProfileID, Name: "LegacyPlayer"},
		Textures: protocol.Textures{
			ProfileID:   snapshotProfileID,
			ProfileName: "LegacyPlayer",
			Textures:    map[string]protocol.Texture{},
		},
		Assets: map[string]assets.Asset{},
	}
	staged := stagedFromSnapshot(t, testMigrationID, "LegacyPlayer", snapshot)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := r.MultipartReader()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		part, err := reader.NextPart()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var metadata CommitMetadata
		if err := json.NewDecoder(part).Decode(&metadata); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if metadata.Email != "" || metadata.SkinSHA256 != "" || metadata.CapeSHA256 != "" || metadata.ProfileModel != "default" {
			t.Errorf("metadata = %+v", metadata)
		}
		if _, err := reader.NextPart(); !errors.Is(err, io.EOF) {
			t.Errorf("unexpected extra multipart part: %v", err)
		}
		writeCommitResponse(t, w, staged, "absent", "absent")
	}))
	defer server.Close()
	client := localCommitClient(t, server, ClientOptions{})
	if _, err := client.Commit(context.Background(), CommitRequest{Staged: staged, Identifier: "LegacyPlayer", Password: "secret-password"}); err != nil {
		t.Fatal(err)
	}
}

func TestCommitMapsUpstreamErrorsWithoutLeakingMessages(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		code     string
		want     error
		wantCode string
	}{
		{name: "invalid request", status: 400, code: "invalid_request", want: ErrInvalidCommit, wantCode: "invalid_request"},
		{name: "unauthorized", status: 401, code: "unauthorized", want: ErrUnauthorized, wantCode: "unauthorized"},
		{name: "forbidden", status: 403, code: "forbidden", want: ErrForbidden, wantCode: "forbidden"},
		{name: "asset conflict", status: 409, code: "migration_asset_conflict", want: ErrCommitConflict, wantCode: "migration_asset_conflict"},
		{name: "too large", status: 413, code: "invalid_texture_file", want: ErrCommitTooLarge, wantCode: "invalid_texture_file"},
		{name: "upstream failure", status: 500, code: "migration_commit_failed", want: ErrUpstream, wantCode: "migration_commit_failed"},
		{name: "unsafe code", status: 500, code: "bad\ncode", want: ErrUpstream, wantCode: "migration_commit_failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "code": tt.code, "message": "sensitive upstream detail"})
			}))
			defer server.Close()
			client := localCommitClient(t, server, ClientOptions{})
			staged := stagedFromSnapshot(t, testMigrationID, "LegacyPlayer", validDigestSnapshot(t))
			_, err := client.Commit(context.Background(), CommitRequest{Staged: staged, Identifier: "LegacyPlayer", Password: "secret-password"})
			if !errors.Is(err, tt.want) {
				t.Fatalf("Commit() error = %v, want %v", err, tt.want)
			}
			var apiError *APIError
			if !errors.As(err, &apiError) || apiError.StatusCode != tt.status || apiError.Code != tt.wantCode {
				t.Fatalf("API error = %+v", apiError)
			}
			if strings.Contains(err.Error(), "sensitive upstream detail") {
				t.Fatalf("error leaked upstream message: %v", err)
			}
		})
	}
}

func TestCommitRejectsInvalidResponses(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CommitResult)
	}{
		{name: "wrong migration", mutate: func(r *CommitResult) { r.MigrationID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" }},
		{name: "wrong status", mutate: func(r *CommitResult) { r.Status = "applying" }},
		{name: "wrong profile", mutate: func(r *CommitResult) { r.ProfileID = strings.Repeat("a", 32) }},
		{name: "missing token", mutate: func(r *CommitResult) { r.Session.AccessToken = "" }},
		{name: "wrong selected profile", mutate: func(r *CommitResult) { r.Session.SelectedProfile.Name = "OtherPlayer" }},
		{name: "missing user", mutate: func(r *CommitResult) { r.User = nil }},
		{name: "wrong user", mutate: func(r *CommitResult) { r.User.ID = strings.Repeat("a", 32) }},
		{name: "invalid user property", mutate: func(r *CommitResult) { r.User.Properties = []map[string]string{{"name": "role"}} }},
		{name: "ambiguous core ownership", mutate: func(r *CommitResult) { r.CreatedCoreUser = false; r.ReusedCoreUser = false }},
		{name: "wrong asset digest", mutate: func(r *CommitResult) { r.Assets.Skin.SHA256 = strings.Repeat("a", 64) }},
		{name: "wrong absent asset", mutate: func(r *CommitResult) { r.Assets.Cape.Status = "absent" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			staged := stagedFromSnapshot(t, testMigrationID, "LegacyPlayer", validDigestSnapshot(t))
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				result := successfulCommitResult(staged, "uploaded", "uploaded")
				tt.mutate(&result)
				_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": result})
			}))
			defer server.Close()
			client := localCommitClient(t, server, ClientOptions{})
			if _, err := client.Commit(context.Background(), CommitRequest{Staged: staged, Identifier: "LegacyPlayer", Password: "secret-password"}); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("Commit() error = %v", err)
			}
		})
	}
}

func TestCommitRejectsInvalidRequestsBeforeSending(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("invalid commit reached upstream")
	}))
	defer server.Close()
	client := localCommitClient(t, server, ClientOptions{})
	staged := stagedFromSnapshot(t, testMigrationID, "LegacyPlayer", validDigestSnapshot(t))
	tests := []struct {
		name    string
		request CommitRequest
	}{
		{name: "short password", request: CommitRequest{Staged: staged, Identifier: "LegacyPlayer", Password: "short"}},
		{name: "wrong identifier", request: CommitRequest{Staged: staged, Identifier: "OtherPlayer", Password: "secret-password"}},
		{name: "invalid migration ID", request: CommitRequest{Staged: func() StagedMigration { copy := staged; copy.MigrationID = "invalid"; return copy }(), Identifier: "LegacyPlayer", Password: "secret-password"}},
		{name: "invalid model", request: CommitRequest{Staged: func() StagedMigration {
			copy := staged
			skin := *copy.Skin
			skin.Metadata = map[string]string{"model": "invalid"}
			copy.Skin = &skin
			copy.CommitDigest, _ = CommitDigest(copy.snapshot(), copy.IdentifierHash)
			return copy
		}(), Identifier: "LegacyPlayer", Password: "secret-password"}},
		{name: "noncanonical asset", request: CommitRequest{Staged: func() StagedMigration {
			copy := staged
			skin := *copy.Skin
			skin.PNG = append(append([]byte(nil), skin.PNG...), 0)
			copy.Skin = &skin
			copy.CommitDigest, _ = CommitDigest(copy.snapshot(), copy.IdentifierHash)
			return copy
		}(), Identifier: "LegacyPlayer", Password: "secret-password"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := client.Commit(context.Background(), tt.request); !errors.Is(err, ErrInvalidCommit) {
				t.Fatalf("Commit() error = %v", err)
			}
		})
	}
	smallClient := localCommitClient(t, server, ClientOptions{MaxRequestBody: 1})
	if _, err := smallClient.Commit(context.Background(), CommitRequest{Staged: staged, Identifier: "LegacyPlayer", Password: "secret-password"}); !errors.Is(err, ErrCommitTooLarge) {
		t.Fatalf("oversized Commit() error = %v", err)
	}
}

func TestCommitRejectsMissingAccountID(t *testing.T) {
	staged := stagedFromSnapshot(t, testMigrationID, "LegacyPlayer", validDigestSnapshot(t))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		result := successfulCommitResult(staged, "uploaded", "uploaded")
		encoded, err := json.Marshal(result)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var data map[string]any
		if err := json.Unmarshal(encoded, &data); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		delete(data, "account_id")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
	}))
	defer server.Close()
	client := localCommitClient(t, server, ClientOptions{})
	if _, err := client.Commit(context.Background(), CommitRequest{Staged: staged, Identifier: "LegacyPlayer", Password: "secret-password"}); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("Commit() error = %v", err)
	}
}

func TestCommitDisablesRedirectsAndBoundsResponses(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		var redirected atomic.Int32
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/internal/migrations/commit" {
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(http.StatusFound)
				return
			}
			redirected.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()
		client := localCommitClient(t, server, ClientOptions{})
		staged := stagedFromSnapshot(t, testMigrationID, "LegacyPlayer", validDigestSnapshot(t))
		if _, err := client.Commit(context.Background(), CommitRequest{Staged: staged, Identifier: "LegacyPlayer", Password: "secret-password"}); !errors.Is(err, ErrUpstream) {
			t.Fatalf("Commit() error = %v", err)
		}
		if redirected.Load() != 0 {
			t.Fatal("client followed redirect")
		}
	})
	t.Run("oversized response", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", 1024)))
		}))
		defer server.Close()
		client := localCommitClient(t, server, ClientOptions{MaxResponseBody: 10})
		staged := stagedFromSnapshot(t, testMigrationID, "LegacyPlayer", validDigestSnapshot(t))
		if _, err := client.Commit(context.Background(), CommitRequest{Staged: staged, Identifier: "LegacyPlayer", Password: "secret-password"}); !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("Commit() error = %v", err)
		}
	})
}

func TestNewCommitClientRejectsInvalidConfiguration(t *testing.T) {
	for _, raw := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com/prefix", "relative"} {
		if _, err := NewClient(raw, "token", ClientOptions{}); err == nil {
			t.Fatalf("NewClient(%q) unexpectedly succeeded", raw)
		}
	}
	for _, token := range []string{"", " token", "token\n", "token with spaces"} {
		if _, err := NewClient("https://example.com", token, ClientOptions{}); err == nil {
			t.Fatalf("NewClient() accepted token %q", token)
		}
	}
	for _, options := range []ClientOptions{{Timeout: -1}, {MaxResponseBody: -1}, {MaxRequestBody: -1}} {
		if _, err := NewClient("https://example.com", "token", options); err == nil {
			t.Fatalf("NewClient(%+v) unexpectedly succeeded", options)
		}
	}
}

func stagedFromSnapshot(t *testing.T, migrationID, identifier string, snapshot Snapshot) StagedMigration {
	t.Helper()
	digest := sha256.Sum256([]byte(identifier))
	identifierHash := hex.EncodeToString(digest[:])
	commitDigest, err := CommitDigest(snapshot, identifierHash)
	if err != nil {
		t.Fatal(err)
	}
	skinPayload, skinBytes, err := stageAsset(snapshot, "SKIN")
	if err != nil {
		t.Fatal(err)
	}
	capePayload, capeBytes, err := stageAsset(snapshot, "CAPE")
	if err != nil {
		t.Fatal(err)
	}
	return StagedMigration{
		MigrationID:    migrationID,
		CommitDigest:   commitDigest,
		IdentifierHash: identifierHash,
		Profile:        protocol.Profile{ID: snapshot.Profile.ID, Name: snapshot.Profile.Name},
		Skin:           stagedAsset(skinPayload, skinBytes),
		Cape:           stagedAsset(capePayload, capeBytes),
		State:          StateStagedInRedis,
		Version:        1,
	}
}

func successfulCommitResult(staged StagedMigration, skinStatus, capeStatus string) CommitResult {
	skin := AssetResult{Status: skinStatus}
	cape := AssetResult{Status: capeStatus}
	if staged.Skin != nil {
		skin.SHA256 = staged.Skin.PixelSHA256
	}
	if staged.Cape != nil {
		cape.SHA256 = staged.Cape.PixelSHA256
	}
	return CommitResult{
		MigrationID:     staged.MigrationID,
		Status:          "committed",
		CoreUID:         "core-user-id",
		AccountID:       42,
		ProfileID:       staged.Profile.ID,
		CreatedCoreUser: true,
		Session: Session{
			AccessToken:     "new-access-token",
			ClientToken:     "new-client-token",
			SelectedProfile: staged.Profile,
		},
		User: &protocol.User{
			ID:         staged.Profile.ID,
			Properties: []map[string]string{{"name": "role", "value": "member"}},
		},
		Assets: AssetResults{
			Skin: skin,
			Cape: cape,
		},
		CommittedAt: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC),
	}
}

func writeCommitResponse(t *testing.T, w http.ResponseWriter, staged StagedMigration, skinStatus, capeStatus string) {
	t.Helper()
	result := successfulCommitResult(staged, skinStatus, capeStatus)
	if staged.Skin == nil {
		result.Assets.Skin.SHA256 = ""
	}
	if staged.Cape == nil {
		result.Assets.Cape.SHA256 = ""
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": result})
}

func localCommitClient(t *testing.T, server *httptest.Server, options ClientOptions) *Client {
	t.Helper()
	client, err := NewClient(server.URL, "service-token", options)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.client.Transport.(*http.Transport)
	transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	return client
}
