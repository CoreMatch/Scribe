package migration

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/corematch/scribe/internal/binding"
	"github.com/corematch/scribe/internal/metrics"
)

type RecoveryClient interface {
	Reconcile(context.Context, string) (MigrationStatus, error)
	Rollback(context.Context, string) (MigrationStatus, error)
}

type RecoveryStaging interface {
	List(context.Context) ([]StagedMigration, error)
	RecordRollbackIdentity(context.Context, string, int64, RollbackIdentity) (StagedMigration, error)
	Transition(context.Context, string, int64, State) (StagedMigration, error)
	ConfirmRollback(context.Context, string, int64, MigrationStatus) (StagedMigration, error)
	Discard(context.Context, string, int64) error
}

type RecoveryBindings interface {
	BindIfAbsentByHash(context.Context, string, binding.Winner, string) (binding.Binding, error)
	BindTokenByHash(context.Context, string, string, binding.Winner) error
}

type Recovery struct {
	staging  RecoveryStaging
	client   RecoveryClient
	bindings RecoveryBindings
	log      *slog.Logger
	metrics  *metrics.Registry
}

func NewRecovery(staging RecoveryStaging, client RecoveryClient, bindings RecoveryBindings, log *slog.Logger, metricsRegistry *metrics.Registry) (*Recovery, error) {
	if staging == nil || client == nil || bindings == nil {
		return nil, errors.New("migration recovery dependencies are required")
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Recovery{staging: staging, client: client, bindings: bindings, log: log, metrics: metricsRegistry}, nil
}

func (r *Recovery) Run(ctx context.Context) error {
	records, err := r.staging.List(ctx)
	if err != nil {
		r.metrics.RecoveryScan("failed")
		return err
	}
	r.metrics.RecoveryScan("success")
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		started := time.Now()
		err := r.recover(ctx, record)
		result := "success"
		if err != nil {
			result = "blocked"
			r.log.Info("migration recovery blocked", "migration_id", record.MigrationID, "reason", "recovery_failed")
		}
		r.metrics.Recovery(result, string(record.State), time.Since(started))
	}
	return nil
}

func (r *Recovery) recover(ctx context.Context, record StagedMigration) error {
	switch record.State {
	case StateStagedInRedis, StateLocked:
		return nil
	case StateDiscarded:
		return r.staging.Discard(ctx, record.MigrationID, record.Version)
	case StateRolledBack:
		status, err := r.client.Reconcile(ctx, record.MigrationID)
		if err != nil {
			return err
		}
		if err := validateRecoveryStatusIdentity(status, record.MigrationID); err != nil {
			return err
		}
		if status.Status != MigrationRolledBack {
			return ErrInvalidResponse
		}
		if err := validateRolledBackInventory(status, record); err != nil {
			return err
		}
		return r.discardRecord(ctx, record)
	case StateCommitted:
		return r.recoverCommitted(ctx, record)
	case StateApplying, StateUnknown:
		current, err := r.staging.Transition(ctx, record.MigrationID, record.Version, StateReconciling)
		if err != nil {
			return err
		}
		return r.recoverActive(ctx, current)
	case StateReconciling, StateRollingBack:
		return r.recoverActive(ctx, record)
	default:
		return ErrMigrationBlocked
	}
}

func (r *Recovery) recoverCommitted(ctx context.Context, record StagedMigration) error {
	status, err := r.client.Reconcile(ctx, record.MigrationID)
	if err != nil {
		return err
	}
	if err := validateRecoveryStatusIdentity(status, record.MigrationID); err != nil {
		return err
	}
	if status.Status != MigrationCommitted {
		return ErrInvalidResponse
	}
	return r.completeCommitted(ctx, record, status)
}

func (r *Recovery) recoverActive(ctx context.Context, record StagedMigration) error {
	status, err := r.client.Reconcile(ctx, record.MigrationID)
	if err != nil {
		return err
	}
	if err := validateRecoveryStatusIdentity(status, record.MigrationID); err != nil {
		return err
	}
	switch status.Status {
	case MigrationCommitted:
		return r.completeCommitted(ctx, record, status)
	case MigrationRolledBack:
		return r.finishRollback(ctx, record, status)
	}
	if err := validateRollbackOwnership(status, record); err != nil {
		return err
	}
	identity, err := rollbackIdentityFromStatus(status)
	if err != nil {
		return err
	}
	current, err := r.staging.RecordRollbackIdentity(ctx, record.MigrationID, record.Version, identity)
	if err != nil {
		return err
	}
	rolledBack, err := r.client.Rollback(ctx, current.MigrationID)
	if err != nil {
		return err
	}
	if err := validateRecoveryStatusIdentity(rolledBack, current.MigrationID); err != nil {
		return err
	}
	switch rolledBack.Status {
	case MigrationRolledBack:
		return r.finishRollback(ctx, current, rolledBack)
	case MigrationCommitted:
		return r.completeCommitted(ctx, current, rolledBack)
	default:
		return ErrInvalidResponse
	}
}

func validateRecoveryStatusIdentity(status MigrationStatus, migrationID string) error {
	if !strings.EqualFold(status.MigrationID, migrationID) {
		return ErrInvalidResponse
	}
	return nil
}

func validateRollbackOwnership(status MigrationStatus, record StagedMigration) error {
	switch status.Status {
	case MigrationReceived, MigrationStagedInRedis, MigrationApplying, MigrationFailed:
	case MigrationUnknown:
		return ErrMigrationBlocked
	default:
		return ErrInvalidResponse
	}
	for _, resource := range status.Resources {
		switch resource.Kind {
		case "core_user":
			if resource.Ownership == "created_by_migration" {
				if resource.ResourceID == "" {
					return ErrInvalidResponse
				}
				continue
			}
			if resource.Ownership != "preexisting" || !resource.Present || resource.ResourceID == "" {
				return ErrInvalidResponse
			}
		case "game_account":
			if resource.Ownership != "created_by_migration" {
				return ErrInvalidResponse
			}
		case "profile":
			if resource.Ownership != "created_by_migration" || !strings.EqualFold(resource.ResourceID, record.Profile.ID) {
				return ErrInvalidResponse
			}
		case "skin", "cape":
			asset := record.Skin
			if resource.Kind == "cape" {
				asset = record.Cape
			}
			if resource.Ownership != "preexisting" || asset == nil ||
				!strings.EqualFold(resource.ResourceID, stagedAssetHash(asset)) {
				return ErrInvalidResponse
			}
		default:
			return ErrInvalidResponse
		}
	}
	return nil
}

func rollbackIdentityFromStatus(status MigrationStatus) (RollbackIdentity, error) {
	identity := RollbackIdentity{}
	seen := make(map[string]struct{}, len(status.Resources))
	for _, resource := range status.Resources {
		if _, duplicate := seen[resource.Kind]; duplicate {
			return RollbackIdentity{}, ErrInvalidResponse
		}
		seen[resource.Kind] = struct{}{}
		switch resource.Kind {
		case "core_user":
			identity.CoreUserID = resource.ResourceID
			identity.CoreUserOwnership = resource.Ownership
		case "game_account":
			identity.AccountID = resource.ResourceID
		}
	}
	if err := validateRollbackIdentity(identity); err != nil {
		return RollbackIdentity{}, ErrInvalidResponse
	}
	if _, ok := seen["profile"]; !ok {
		return RollbackIdentity{}, ErrInvalidResponse
	}
	return identity, nil
}

func validateCommittedInventory(status MigrationStatus, record StagedMigration, result *CommitResult) error {
	if result == nil || status.Result == nil || status.LastError != nil || status.Status != MigrationCommitted {
		return ErrMigrationBlocked
	}
	if err := validateRecoveredCommitResult(*result, record.MigrationID); err != nil {
		return err
	}
	if err := validateResourceInventory(status); err != nil {
		return err
	}
	coreOwnership := "preexisting"
	if result.CreatedCoreUser {
		coreOwnership = "created_by_migration"
	}
	expected := map[string]MigrationResource{
		"core_user":    {Kind: "core_user", Ownership: coreOwnership, Present: true},
		"game_account": {Kind: "game_account", Ownership: "created_by_migration", Present: true},
		"profile":      {Kind: "profile", ResourceID: record.Profile.ID, Ownership: "created_by_migration", Present: true},
	}
	for name, asset := range map[string]*StagedAsset{"skin": record.Skin, "cape": record.Cape} {
		if asset != nil {
			expected[name] = MigrationResource{Kind: name, ResourceID: stagedAssetHash(asset), Ownership: "preexisting", Present: true}
		}
	}
	seen := make(map[string]struct{}, len(status.Resources))
	for _, resource := range status.Resources {
		kind := strings.ToLower(resource.Kind)
		if _, duplicate := seen[kind]; duplicate {
			return ErrInvalidResponse
		}
		seen[kind] = struct{}{}
		wanted, ok := expected[kind]
		if !ok || resource.Ownership != wanted.Ownership || resource.Present != wanted.Present {
			return ErrInvalidResponse
		}
		switch kind {
		case "core_user":
			if resource.ResourceID == "" || !strings.EqualFold(resource.ResourceID, result.CoreUID) {
				return ErrInvalidResponse
			}
		case "game_account":
			if resource.ResourceID != fmt.Sprintf("%d", result.AccountID) {
				return ErrInvalidResponse
			}
		default:
			if !strings.EqualFold(resource.ResourceID, wanted.ResourceID) {
				return ErrInvalidResponse
			}
		}
	}
	if len(seen) != len(expected) {
		return ErrInvalidResponse
	}
	return nil
}

func validateRolledBackInventory(status MigrationStatus, record StagedMigration) error {
	if status.Result != nil || status.LastError != nil || status.Status != MigrationRolledBack {
		return ErrInvalidResponse
	}
	if record.RollbackIdentity == nil {
		return ErrMigrationBlocked
	}
	if err := validateResourceInventory(status); err != nil {
		return err
	}
	identity := *record.RollbackIdentity
	expected := map[string]MigrationResource{
		"profile": {Kind: "profile", ResourceID: record.Profile.ID, Ownership: "created_by_migration", Present: false},
	}
	if identity.CoreUserID != "" {
		expected["core_user"] = MigrationResource{
			Kind:       "core_user",
			ResourceID: identity.CoreUserID,
			Ownership:  identity.CoreUserOwnership,
			Present:    identity.CoreUserOwnership == "preexisting",
		}
	}
	if identity.AccountID != "" {
		expected["game_account"] = MigrationResource{Kind: "game_account", ResourceID: identity.AccountID, Ownership: "created_by_migration", Present: false}
	}
	expectedAssets := make(map[string]struct{}, 2)
	for name, asset := range map[string]*StagedAsset{"skin": record.Skin, "cape": record.Cape} {
		if asset != nil {
			expectedAssets[name] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(status.Resources))
	for _, resource := range status.Resources {
		kind := strings.ToLower(resource.Kind)
		if _, duplicate := seen[kind]; duplicate {
			return ErrInvalidResponse
		}
		seen[kind] = struct{}{}
		if wanted, ok := expected[kind]; ok {
			if resource.Ownership != wanted.Ownership || resource.Present != wanted.Present ||
				!strings.EqualFold(resource.ResourceID, wanted.ResourceID) {
				return ErrInvalidResponse
			}
			continue
		}
		if kind != "skin" && kind != "cape" {
			return ErrInvalidResponse
		}
		asset := record.Skin
		if kind == "cape" {
			asset = record.Cape
		}
		if _, ok := expectedAssets[kind]; !ok || asset == nil || resource.Ownership != "preexisting" ||
			(resource.Present && !strings.EqualFold(resource.ResourceID, stagedAssetHash(asset))) {
			return ErrInvalidResponse
		}
	}
	for kind := range expected {
		if _, ok := seen[kind]; !ok {
			return ErrInvalidResponse
		}
	}
	return nil
}

func stagedAssetHash(asset *StagedAsset) string {
	if asset == nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(asset.PNG))
}

func (r *Recovery) completeCommitted(ctx context.Context, record StagedMigration, status MigrationStatus) error {
	if status.Result == nil || status.LastError != nil {
		return ErrMigrationBlocked
	}
	result := status.Result
	if err := validateCommittedInventory(status, record, result); err != nil {
		return err
	}
	if !strings.EqualFold(result.ProfileID, record.Profile.ID) || result.Session.SelectedProfile.Name != record.Profile.Name {
		return ErrInvalidResponse
	}
	if err := validateAssetResult(result.Assets.Skin, record.Skin); err != nil {
		return err
	}
	if err := validateAssetResult(result.Assets.Cape, record.Cape); err != nil {
		return err
	}
	if _, err := r.bindings.BindIfAbsentByHash(ctx, record.IdentifierHash, binding.Armorial, record.MigrationID); err != nil {
		return err
	}
	if err := r.bindings.BindTokenByHash(ctx, result.Session.AccessToken, record.IdentifierHash, binding.Armorial); err != nil {
		return err
	}
	current := record
	if current.State != StateCommitted {
		next, err := r.staging.Transition(ctx, current.MigrationID, current.Version, StateCommitted)
		if err != nil {
			return err
		}
		current = next
	}
	_, err := r.staging.Transition(ctx, current.MigrationID, current.Version, StateLocked)
	return err
}

func (r *Recovery) finishRollback(ctx context.Context, record StagedMigration, status MigrationStatus) error {
	if err := validateRolledBackInventory(status, record); err != nil {
		return err
	}
	current := record
	if current.State != StateRollingBack {
		next, err := r.staging.ConfirmRollback(ctx, current.MigrationID, current.Version, status)
		if err != nil {
			return err
		}
		current = next
	}
	current, err := r.staging.Transition(ctx, current.MigrationID, current.Version, StateRolledBack)
	if err != nil {
		return err
	}
	return r.discardRecord(ctx, current)
}

func (r *Recovery) discardRecord(ctx context.Context, record StagedMigration) error {
	current, err := r.staging.Transition(ctx, record.MigrationID, record.Version, StateDiscarded)
	if err != nil {
		return err
	}
	return r.staging.Discard(ctx, current.MigrationID, current.Version)
}
