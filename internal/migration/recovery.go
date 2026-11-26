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
	return r.completeCommitted(ctx, record, status.Result, status.LastError)
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
		return r.completeCommitted(ctx, record, status.Result, status.LastError)
	case MigrationRolledBack:
		return r.finishRollback(ctx, record, status)
	}
	if err := validateRollbackOwnership(status, record); err != nil {
		return err
	}
	rolledBack, err := r.client.Rollback(ctx, record.MigrationID)
	if err != nil {
		return err
	}
	if err := validateRecoveryStatusIdentity(rolledBack, record.MigrationID); err != nil {
		return err
	}
	switch rolledBack.Status {
	case MigrationRolledBack:
		return r.finishRollback(ctx, record, rolledBack)
	case MigrationCommitted:
		return r.completeCommitted(ctx, record, rolledBack.Result, rolledBack.LastError)
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
				if resource.Present {
					return ErrRollbackNotAllowed
				}
				return ErrInvalidResponse
			}
			if !resource.Present {
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
				!strings.EqualFold(resource.ResourceID, fmt.Sprintf("%x", sha256.Sum256(asset.PNG))) {
				return ErrInvalidResponse
			}
		default:
			return ErrInvalidResponse
		}
	}
	return nil
}

func (r *Recovery) completeCommitted(ctx context.Context, record StagedMigration, result *CommitResult, lastError *MigrationError) error {
	if result == nil || lastError != nil {
		return ErrMigrationBlocked
	}
	if err := validateRecoveredCommitResult(*result, record.MigrationID); err != nil {
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
