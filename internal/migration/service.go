package migration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/corematch/scribe/internal/metrics"
	"github.com/corematch/scribe/internal/protocol"
)

var (
	ErrMigrationInProgress = errors.New("migration already in progress")
	ErrMigrationBlocked    = errors.New("migration is not in a committable state")
)

type Input struct {
	Identifier           string
	Password             string
	SelectedProfile      protocol.Profile
	LegacyUserID         string
	LegacyUserProperties map[string]string
}

type Result struct {
	MigrationID string
	Commit      CommitResult
}

type Runner interface {
	Migrate(context.Context, Input) (Result, error)
}

type PendingChecker interface {
	Pending(context.Context, string) (bool, error)
}

type SnapshotCapturer interface {
	Capture(context.Context, protocol.Profile) (Snapshot, error)
}

type StagingStore interface {
	Stage(context.Context, string, string, Snapshot) (StagedMigration, bool, error)
	Transition(context.Context, string, int64, State) (StagedMigration, error)
}

type Committer interface {
	Validate(CommitRequest) error
	Commit(context.Context, CommitRequest) (CommitResult, error)
}

type Service struct {
	snapshots SnapshotCapturer
	staging   StagingStore
	committer Committer
	newID     func() (string, error)
	log       *slog.Logger
	metrics   *metrics.Registry
}

func NewService(snapshots SnapshotCapturer, staging StagingStore, committer Committer, log *slog.Logger, metricsRegistry *metrics.Registry) (*Service, error) {
	if snapshots == nil || staging == nil || committer == nil {
		return nil, errors.New("migration snapshot, staging, and commit services are required")
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Service{snapshots: snapshots, staging: staging, committer: committer, newID: randomMigrationID, log: log, metrics: metricsRegistry}, nil
}

func (s *Service) Migrate(ctx context.Context, input Input) (result Result, retErr error) {
	started := time.Now()
	outcome, phase := "failed", "input"
	defer func() {
		s.metrics.Migration(outcome, phase, time.Since(started))
		if outcome == "failed" {
			s.metrics.MigrationFailed(migrationFailureReason(retErr, phase), phase)
		}
	}()

	identifier, err := protocol.NormalizeIdentifier(input.Identifier)
	if err != nil {
		return Result{}, ErrInvalidCommit
	}
	if err := validateCommitCredentials(input.SelectedProfile, input.Password); err != nil {
		return Result{}, err
	}
	phase = "snapshot"
	snapshot, err := s.snapshots.Capture(ctx, input.SelectedProfile)
	if err != nil {
		return Result{}, err
	}
	identifierDigest := sha256.Sum256([]byte(identifier))
	identifierHash := hex.EncodeToString(identifierDigest[:])
	phase = "id"
	migrationID, err := s.newID()
	if err != nil || !validMigrationID(migrationID) {
		return Result{}, errors.New("generate migration ID failed")
	}
	phase = "stage"
	staged, _, err := s.staging.Stage(ctx, migrationID, identifierHash, snapshot)
	if err != nil {
		return Result{}, err
	}
	commitRequest := CommitRequest{
		Staged:               staged,
		Identifier:           identifier,
		Password:             input.Password,
		LegacyUserID:         input.LegacyUserID,
		LegacyUserProperties: input.LegacyUserProperties,
	}
	phase = "validate"
	if err := s.committer.Validate(commitRequest); err != nil {
		return Result{}, err
	}
	phase = "transition"
	switch staged.State {
	case StateStagedInRedis, StateReconciling:
		staged, err = s.staging.Transition(ctx, staged.MigrationID, staged.Version, StateApplying)
		if err != nil {
			return Result{}, err
		}
	case StateApplying:
		return Result{}, ErrMigrationInProgress
	case StateCommitted, StateLocked:
	default:
		return Result{}, ErrMigrationBlocked
	}
	commitRequest.Staged = staged
	s.log.Info("migration applying", "migration_id", staged.MigrationID)
	phase = "commit"
	commit, err := s.committer.Commit(ctx, commitRequest)
	bookkeepingCtx := context.WithoutCancel(ctx)
	if err != nil {
		_, _ = s.staging.Transition(bookkeepingCtx, staged.MigrationID, staged.Version, StateReconciling)
		s.log.Info("migration commit failed", "migration_id", staged.MigrationID, "reason", "upstream_or_validation")
		return Result{}, err
	}
	phase = "bookkeeping"
	if _, err := s.staging.Transition(bookkeepingCtx, staged.MigrationID, staged.Version, StateCommitted); err != nil {
		s.log.Info("migration committed with pending bookkeeping", "migration_id", staged.MigrationID)
	}
	outcome = "success"
	return Result{MigrationID: staged.MigrationID, Commit: commit}, nil
}

func migrationFailureReason(err error, phase string) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case "migration_asset_conflict":
			return "asset_conflict"
		case "migration_credential_conflict":
			return "credential_conflict"
		case "migration_profile_conflict":
			return "profile_conflict"
		case "migration_target_not_empty":
			return "target_not_empty"
		case "migration_conflict":
			return "commit_conflict"
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, ErrMigrationInProgress):
		return "migration_in_progress"
	case errors.Is(err, ErrMigrationBlocked):
		return "migration_blocked"
	case errors.Is(err, ErrCommitConflict):
		return "commit_conflict"
	case errors.Is(err, ErrUnauthorized), errors.Is(err, ErrForbidden):
		return "service_auth"
	case errors.Is(err, ErrCommitTooLarge):
		return "payload_too_large"
	case errors.Is(err, ErrResponseTooLarge):
		return "response_too_large"
	case errors.Is(err, ErrInvalidCommit):
		return "invalid_request"
	case errors.Is(err, ErrInvalidResponse):
		return "invalid_response"
	case errors.Is(err, ErrUpstream):
		return "upstream_unavailable"
	case phase == "snapshot":
		return "snapshot_failed"
	case phase == "id" || phase == "stage" || phase == "transition":
		return "staging_failed"
	default:
		return "internal"
	}
}

func randomMigrationID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:]), nil
}
