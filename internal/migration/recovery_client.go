package migration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/corematch/scribe/internal/protocol"
)

var (
	ErrMigrationNotFound  = errors.New("migration not found")
	ErrRollbackNotAllowed = errors.New("migration rollback not allowed")
	ErrInvalidOperation   = errors.New("invalid migration operation")
)

const (
	MigrationReceived      = "received"
	MigrationStagedInRedis = "staged_in_redis"
	MigrationApplying      = "applying"
	MigrationCommitted     = "committed"
	MigrationRolledBack    = "rolled_back"
	MigrationFailed        = "failed"
	MigrationUnknown       = "unknown"
)

type MigrationResource struct {
	Kind       string `json:"kind"`
	ResourceID string `json:"resource_id"`
	Ownership  string `json:"ownership"`
	Present    bool   `json:"present"`
}

type MigrationError struct {
	Code  string `json:"code"`
	Stage string `json:"stage"`
}

type MigrationStatus struct {
	MigrationID string              `json:"migration_id"`
	Status      string              `json:"status"`
	UpdatedAt   time.Time           `json:"updated_at"`
	Resources   []MigrationResource `json:"resources"`
	Result      *CommitResult       `json:"result,omitempty"`
	LastError   *MigrationError     `json:"last_error,omitempty"`
}

type migrationResourceWire struct {
	Kind       *string `json:"kind"`
	ResourceID *string `json:"resource_id"`
	Ownership  *string `json:"ownership"`
	Present    *bool   `json:"present"`
}

type migrationErrorWire struct {
	Code    *string `json:"code"`
	Message *string `json:"message"`
	Stage   *string `json:"stage"`
}

type migrationStatusWire struct {
	MigrationID string                   `json:"migration_id"`
	Status      string                   `json:"status"`
	UpdatedAt   time.Time                `json:"updated_at"`
	Resources   *[]migrationResourceWire `json:"resources"`
	Result      *commitResultWire        `json:"result"`
	LastError   *migrationErrorWire      `json:"last_error"`
}

func (c *Client) Reconcile(ctx context.Context, migrationID string) (MigrationStatus, error) {
	return c.operation(ctx, "reconcile", migrationID)
}

func (c *Client) Rollback(ctx context.Context, migrationID string) (MigrationStatus, error) {
	return c.operation(ctx, "rollback", migrationID)
}

func (c *Client) operation(ctx context.Context, action, migrationID string) (MigrationStatus, error) {
	if !validMigrationID(migrationID) {
		return MigrationStatus{}, ErrInvalidOperation
	}
	target := *c.baseURL
	target.Path = "/internal/migrations/" + migrationID + "/" + action
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), nil)
	if err != nil {
		return MigrationStatus{}, ErrInvalidOperation
	}
	request.Header.Set("Authorization", "Bearer "+c.serviceToken)
	request.Header.Set("Accept", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return MigrationStatus{}, ctx.Err()
		}
		return MigrationStatus{}, ErrUpstream
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return MigrationStatus{}, ctx.Err()
		}
		return MigrationStatus{}, ErrUpstream
	}
	if int64(len(body)) > c.maxResponseBody {
		return MigrationStatus{}, ErrResponseTooLarge
	}
	if response.StatusCode != http.StatusOK {
		return MigrationStatus{}, migrationOperationError(action, response.StatusCode, body)
	}
	var envelope struct {
		Success bool                `json:"success"`
		Data    migrationStatusWire `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || !envelope.Success {
		return MigrationStatus{}, ErrInvalidResponse
	}
	status, err := decodeMigrationStatus(envelope.Data, migrationID)
	if err != nil {
		return MigrationStatus{}, err
	}
	return status, nil
}

func decodeMigrationStatus(wire migrationStatusWire, migrationID string) (MigrationStatus, error) {
	if !validMigrationID(wire.MigrationID) || !strings.EqualFold(wire.MigrationID, migrationID) ||
		!validMigrationStatus(wire.Status) || wire.UpdatedAt.IsZero() || wire.Resources == nil {
		return MigrationStatus{}, ErrInvalidResponse
	}
	status := MigrationStatus{
		MigrationID: wire.MigrationID,
		Status:      wire.Status,
		UpdatedAt:   wire.UpdatedAt,
		Resources:   make([]MigrationResource, 0, len(*wire.Resources)),
	}
	for _, resource := range *wire.Resources {
		if resource.Kind == nil || resource.ResourceID == nil || resource.Ownership == nil || resource.Present == nil ||
			!validResourceKind(*resource.Kind) || *resource.ResourceID == "" ||
			(*resource.Ownership != "created_by_migration" && *resource.Ownership != "preexisting") {
			return MigrationStatus{}, ErrInvalidResponse
		}
		status.Resources = append(status.Resources, MigrationResource{
			Kind:       *resource.Kind,
			ResourceID: *resource.ResourceID,
			Ownership:  *resource.Ownership,
			Present:    *resource.Present,
		})
	}
	if wire.Result != nil {
		if wire.Result.AccountID == nil {
			return MigrationStatus{}, ErrInvalidResponse
		}
		result := wire.Result.CommitResult
		result.AccountID = *wire.Result.AccountID
		if err := validateRecoveredCommitResult(result, migrationID); err != nil {
			return MigrationStatus{}, err
		}
		status.Result = &result
	}
	if wire.LastError != nil {
		lastError := wire.LastError
		if lastError.Code == nil || lastError.Message == nil || lastError.Stage == nil || !validMigrationStage(*lastError.Stage) {
			return MigrationStatus{}, ErrInvalidResponse
		}
		status.LastError = &MigrationError{Code: safeErrorCode(*lastError.Code), Stage: *lastError.Stage}
	}
	return status, nil
}

func validateRecoveredCommitResult(result CommitResult, migrationID string) error {
	if !strings.EqualFold(result.MigrationID, migrationID) || result.Status != MigrationCommitted ||
		!protocol.ValidProfileID(result.ProfileID) || result.CoreUID == "" || result.CreatedCoreUser == result.ReusedCoreUser || result.CommittedAt.IsZero() {
		return ErrInvalidResponse
	}
	session := result.Session
	if session.AccessToken == "" || session.ClientToken == "" || !protocol.ValidProfileID(session.SelectedProfile.ID) ||
		session.SelectedProfile.Name == "" || !strings.EqualFold(session.SelectedProfile.ID, result.ProfileID) {
		return ErrInvalidResponse
	}
	for _, profile := range session.AvailableProfiles {
		if !protocol.ValidProfileID(profile.ID) || profile.Name == "" {
			return ErrInvalidResponse
		}
	}
	if err := validateRecoveredAssetResult(result.Assets.Skin); err != nil {
		return err
	}
	return validateRecoveredAssetResult(result.Assets.Cape)
}

func validateRecoveredAssetResult(result AssetResult) error {
	switch result.Status {
	case "absent":
		if result.SHA256 != "" {
			return ErrInvalidResponse
		}
	case "uploaded", "unchanged":
		if _, err := canonicalSHA256(result.SHA256); err != nil {
			return ErrInvalidResponse
		}
	default:
		return ErrInvalidResponse
	}
	return nil
}

func validMigrationStatus(status string) bool {
	switch status {
	case MigrationReceived, MigrationStagedInRedis, MigrationApplying, MigrationCommitted, MigrationRolledBack, MigrationFailed, MigrationUnknown:
		return true
	default:
		return false
	}
}

func validResourceKind(kind string) bool {
	switch kind {
	case "core_user", "game_account", "profile", "skin", "cape":
		return true
	default:
		return false
	}
}

func validMigrationStage(stage string) bool {
	switch stage {
	case "received", "staged_in_redis", "core_provision", "profile_create", "asset_apply", "verify", "commit", "reconcile", "rollback":
		return true
	default:
		return false
	}
}

func migrationOperationError(action string, status int, body []byte) error {
	var envelope struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &envelope)
	fallback := "migration_" + action + "_failed"
	code := safeErrorCode(envelope.Code)
	if code == "migration_commit_failed" {
		code = fallback
	}
	var sentinel error
	switch {
	case status == http.StatusUnauthorized:
		sentinel = ErrUnauthorized
	case status == http.StatusForbidden:
		sentinel = ErrForbidden
	case status == http.StatusNotFound:
		sentinel = ErrMigrationNotFound
	case status == http.StatusConflict && action == "rollback":
		sentinel = ErrRollbackNotAllowed
	case status == http.StatusBadRequest:
		sentinel = ErrInvalidOperation
	case status == http.StatusRequestEntityTooLarge:
		sentinel = ErrCommitTooLarge
	default:
		sentinel = ErrUpstream
	}
	return &APIError{StatusCode: status, Code: code, Err: sentinel}
}
