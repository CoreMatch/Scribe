package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/corematch/scribe/internal/binding"
	"github.com/corematch/scribe/internal/metrics"
)

type fakeRecoveryStaging struct {
	records           []StagedMigration
	listErr           error
	transitions       []State
	discards          []string
	failTransition    State
	discardErr        error
	recordIdentityErr error
}

func (f *fakeRecoveryStaging) List(context.Context) ([]StagedMigration, error) {
	return f.records, f.listErr
}

func (f *fakeRecoveryStaging) RecordRollbackIdentity(_ context.Context, migrationID string, expectedVersion int64, identity RollbackIdentity) (StagedMigration, error) {
	if f.recordIdentityErr != nil {
		return StagedMigration{}, f.recordIdentityErr
	}
	if err := validateRollbackIdentity(identity); err != nil {
		return StagedMigration{}, err
	}
	for i := range f.records {
		if f.records[i].MigrationID != migrationID {
			continue
		}
		if f.records[i].Version != expectedVersion {
			return StagedMigration{}, ErrVersionConflict
		}
		if f.records[i].RollbackIdentity != nil && *f.records[i].RollbackIdentity != identity {
			return StagedMigration{}, ErrCorruptStaging
		}
		f.records[i].RollbackIdentity = &identity
		f.records[i].Version++
		return f.records[i], nil
	}
	return StagedMigration{}, ErrNotFound
}

func (f *fakeRecoveryStaging) Transition(_ context.Context, migrationID string, _ int64, next State) (StagedMigration, error) {
	f.transitions = append(f.transitions, next)
	if next == f.failTransition {
		return StagedMigration{}, errors.New("transition failed")
	}
	for i := range f.records {
		if f.records[i].MigrationID == migrationID {
			f.records[i].State = next
			f.records[i].Version++
			return f.records[i], nil
		}
	}
	return StagedMigration{}, ErrNotFound
}

func (f *fakeRecoveryStaging) ConfirmRollback(ctx context.Context, migrationID string, version int64, status MigrationStatus) (StagedMigration, error) {
	if status.Status != MigrationRolledBack || !strings.EqualFold(status.MigrationID, migrationID) || status.Result != nil {
		return StagedMigration{}, ErrInvalidResponse
	}
	if err := validateResourceInventory(status); err != nil {
		return StagedMigration{}, err
	}
	return f.Transition(ctx, migrationID, version, StateRollingBack)
}

func (f *fakeRecoveryStaging) Discard(_ context.Context, migrationID string, _ int64) error {
	f.discards = append(f.discards, migrationID)
	return f.discardErr
}

type fakeRecoveryClient struct {
	reconcileStatus MigrationStatus
	reconcileErr    error
	rollbackStatus  MigrationStatus
	rollbackErr     error
	calls           []string
}

func (f *fakeRecoveryClient) Reconcile(context.Context, string) (MigrationStatus, error) {
	f.calls = append(f.calls, "reconcile")
	return f.reconcileStatus, f.reconcileErr
}

func (f *fakeRecoveryClient) Rollback(context.Context, string) (MigrationStatus, error) {
	f.calls = append(f.calls, "rollback")
	return f.rollbackStatus, f.rollbackErr
}

type fakeRecoveryBindings struct {
	bindErr   error
	tokenErr  error
	bindCalls []string
	tokens    []string
}

func (f *fakeRecoveryBindings) BindIfAbsentByHash(_ context.Context, identifierHash string, _ binding.Winner, migrationID string) (binding.Binding, error) {
	f.bindCalls = append(f.bindCalls, identifierHash+":"+migrationID)
	return binding.Binding{}, f.bindErr
}

func (f *fakeRecoveryBindings) BindTokenByHash(_ context.Context, token, identifierHash string, _ binding.Winner) error {
	f.tokens = append(f.tokens, token+":"+identifierHash)
	return f.tokenErr
}

func recoveryRecord(t *testing.T, state State) StagedMigration {
	t.Helper()
	record := stagedFromSnapshot(t, testMigrationID, "LegacyPlayer", validDigestSnapshot(t))
	record.State = state
	record.Version = 2
	if state == StateRolledBack || state == StateRollingBack {
		identity := &RollbackIdentity{CoreUserID: "core-uuid", CoreUserOwnership: "preexisting", AccountID: "42"}
		record.RollbackIdentity = identity
	}
	return record
}

func committedRecoveryStatus(t *testing.T, record StagedMigration) MigrationStatus {
	t.Helper()
	result := successfulCommitResult(record, "uploaded", "uploaded")
	coreOwnership := "created_by_migration"
	if !result.CreatedCoreUser {
		coreOwnership = "preexisting"
	}
	return MigrationStatus{
		MigrationID: record.MigrationID,
		Status:      MigrationCommitted,
		Result:      &result,
		Resources: []MigrationResource{
			{Kind: "core_user", ResourceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Ownership: coreOwnership, Present: true},
			{Kind: "game_account", ResourceID: fmt.Sprintf("%d", result.AccountID), Ownership: "created_by_migration", Present: true},
			{Kind: "profile", ResourceID: record.Profile.ID, Ownership: "created_by_migration", Present: true},
			{Kind: "skin", ResourceID: stagedAssetHash(record.Skin), Ownership: "preexisting", Present: true},
			{Kind: "cape", ResourceID: stagedAssetHash(record.Cape), Ownership: "preexisting", Present: true},
		},
	}
}

func rollbackOwnershipStatus(t *testing.T, record StagedMigration) MigrationStatus {
	t.Helper()
	return MigrationStatus{
		MigrationID: record.MigrationID,
		Status:      MigrationFailed,
		Resources: []MigrationResource{
			{Kind: "core_user", ResourceID: "core-uuid", Ownership: "preexisting", Present: true},
			{Kind: "game_account", ResourceID: "42", Ownership: "created_by_migration", Present: true},
			{Kind: "profile", ResourceID: record.Profile.ID, Ownership: "created_by_migration", Present: true},
			{Kind: "skin", ResourceID: stagedAssetHash(record.Skin), Ownership: "preexisting", Present: true},
			{Kind: "cape", ResourceID: stagedAssetHash(record.Cape), Ownership: "preexisting", Present: true},
		},
	}
}

func rolledBackRecoveryStatus(t *testing.T, record StagedMigration) MigrationStatus {
	t.Helper()
	status := MigrationStatus{MigrationID: record.MigrationID, Status: MigrationRolledBack}
	identity := record.RollbackIdentity
	if identity == nil {
		identity = &RollbackIdentity{CoreUserID: "core-uuid", CoreUserOwnership: "preexisting", AccountID: "42"}
	}
	if identity.CoreUserID != "" {
		status.Resources = append(status.Resources, MigrationResource{
			Kind:       "core_user",
			ResourceID: identity.CoreUserID,
			Ownership:  identity.CoreUserOwnership,
			Present:    identity.CoreUserOwnership == "preexisting",
		})
	}
	if identity.AccountID != "" {
		status.Resources = append(status.Resources, MigrationResource{
			Kind: "game_account", ResourceID: identity.AccountID, Ownership: "created_by_migration", Present: false,
		})
	}
	status.Resources = append(status.Resources,
		MigrationResource{Kind: "profile", ResourceID: record.Profile.ID, Ownership: "created_by_migration", Present: false},
		MigrationResource{Kind: "skin", ResourceID: stagedAssetHash(record.Skin), Ownership: "preexisting", Present: true},
		MigrationResource{Kind: "cape", ResourceID: stagedAssetHash(record.Cape), Ownership: "preexisting", Present: true},
	)
	return status
}

func newTestRecovery(staging *fakeRecoveryStaging, client *fakeRecoveryClient, bindings *fakeRecoveryBindings) *Recovery {
	recovery, err := NewRecovery(staging, client, bindings, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		panic(err)
	}
	return recovery
}

func TestRecoveryBindsAuthoritativeCommitAfterLostResponse(t *testing.T) {
	record := recoveryRecord(t, StateApplying)
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{reconcileStatus: committedRecoveryStatus(t, record)}
	bindings := &fakeRecoveryBindings{}
	recovery := newTestRecovery(staging, client, bindings)
	if err := recovery.recover(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	wantTransitions := []State{StateReconciling, StateCommitted, StateLocked}
	if !reflect.DeepEqual(staging.transitions, wantTransitions) {
		t.Fatalf("transitions = %#v, want %#v", staging.transitions, wantTransitions)
	}
	if len(bindings.bindCalls) != 1 || bindings.bindCalls[0] != record.IdentifierHash+":"+record.MigrationID {
		t.Fatalf("binding calls = %#v", bindings.bindCalls)
	}
	if len(bindings.tokens) != 1 || bindings.tokens[0] != "new-access-token:"+record.IdentifierHash {
		t.Fatalf("token calls = %#v", bindings.tokens)
	}
	if !reflect.DeepEqual(client.calls, []string{"reconcile"}) {
		t.Fatalf("recovery calls = %#v", client.calls)
	}
}

func TestRecoveryLocksCommittedMigrationWithCoreUUIDResource(t *testing.T) {
	record := recoveryRecord(t, StateCommitted)
	status := committedRecoveryStatus(t, record)
	if status.Result.CoreUID == status.Resources[0].ResourceID {
		t.Fatal("test must distinguish Core UID from Core UUID")
	}
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{reconcileStatus: status}
	bindings := &fakeRecoveryBindings{}
	recovery := newTestRecovery(staging, client, bindings)
	if err := recovery.recover(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if staging.records[0].State != StateLocked || !reflect.DeepEqual(staging.transitions, []State{StateLocked}) {
		t.Fatalf("committed recovery did not lock: state=%s transitions=%#v", staging.records[0].State, staging.transitions)
	}
	if len(bindings.bindCalls) != 1 || len(bindings.tokens) != 1 {
		t.Fatalf("permanent bindings were not completed: bindings=%#v tokens=%d", bindings.bindCalls, len(bindings.tokens))
	}
}

func TestRecoveryLeavesCommittedMigrationWithoutSessionForRetry(t *testing.T) {
	record := recoveryRecord(t, StateCommitted)
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{reconcileStatus: MigrationStatus{MigrationID: record.MigrationID, Status: MigrationCommitted}}
	bindings := &fakeRecoveryBindings{}
	recovery := newTestRecovery(staging, client, bindings)
	if err := recovery.recover(context.Background(), record); !errors.Is(err, ErrMigrationBlocked) {
		t.Fatalf("recover() error = %v, want ErrMigrationBlocked", err)
	}
	if len(staging.transitions) != 0 || len(bindings.bindCalls) != 0 || len(bindings.tokens) != 0 {
		t.Fatal("recovery fabricated a session or changed committed state")
	}
}

func TestRecoveryRejectsCommittedResultWithDrift(t *testing.T) {
	record := recoveryRecord(t, StateCommitted)
	status := committedRecoveryStatus(t, record)
	status.LastError = &MigrationError{Code: "migration_reconcile_failed", Stage: "reconcile"}
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{reconcileStatus: status}
	bindings := &fakeRecoveryBindings{}
	recovery := newTestRecovery(staging, client, bindings)
	if err := recovery.recover(context.Background(), record); !errors.Is(err, ErrMigrationBlocked) {
		t.Fatalf("recover() error = %v, want ErrMigrationBlocked", err)
	}
	if len(staging.transitions) != 0 || len(bindings.bindCalls) != 0 || len(bindings.tokens) != 0 {
		t.Fatal("drifted committed result was bound or locked")
	}
}

func TestRecoveryRejectsCommittedInventoryDrift(t *testing.T) {
	record := recoveryRecord(t, StateCommitted)
	tests := []struct {
		name   string
		mutate func(*MigrationStatus)
	}{
		{name: "missing account", mutate: func(status *MigrationStatus) {
			status.Resources = append(status.Resources[:1], status.Resources[2:]...)
		}},
		{name: "missing core user", mutate: func(status *MigrationStatus) {
			status.Resources = status.Resources[1:]
		}},
		{name: "wrong core user", mutate: func(status *MigrationStatus) {
			status.Resources[0].ResourceID = "other-core-uuid"
		}},
		{name: "wrong core ownership", mutate: func(status *MigrationStatus) {
			if status.Resources[0].Ownership == "created_by_migration" {
				status.Resources[0].Ownership = "preexisting"
			} else {
				status.Resources[0].Ownership = "created_by_migration"
			}
		}},
		{name: "wrong account", mutate: func(status *MigrationStatus) {
			status.Resources[1].ResourceID = "43"
		}},
		{name: "absent account", mutate: func(status *MigrationStatus) {
			status.Resources[1].Present = false
		}},
		{name: "wrong profile", mutate: func(status *MigrationStatus) {
			status.Resources[2].ResourceID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		}},
		{name: "absent profile", mutate: func(status *MigrationStatus) {
			status.Resources[2].Present = false
		}},
		{name: "wrong skin digest", mutate: func(status *MigrationStatus) {
			status.Resources[3].ResourceID = strings.Repeat("a", 64)
		}},
		{name: "unexpected resource", mutate: func(status *MigrationStatus) {
			status.Resources = append(status.Resources, MigrationResource{Kind: "unexpected", ResourceID: "id", Ownership: "preexisting", Present: true})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := committedRecoveryStatus(t, record)
			tt.mutate(&status)
			staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
			client := &fakeRecoveryClient{reconcileStatus: status}
			bindings := &fakeRecoveryBindings{}
			recovery := newTestRecovery(staging, client, bindings)
			if err := recovery.recover(context.Background(), record); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("recover() error = %v, want ErrInvalidResponse", err)
			}
			if len(staging.transitions) != 0 || len(bindings.bindCalls) != 0 || len(bindings.tokens) != 0 {
				t.Fatal("mismatched committed inventory was bound or locked")
			}
		})
	}
}

func TestRecoveryRejectsRolledBackInventoryDrift(t *testing.T) {
	record := recoveryRecord(t, StateRolledBack)
	tests := []struct {
		name   string
		mutate func(*MigrationStatus)
	}{
		{name: "wrong core user", mutate: func(status *MigrationStatus) {
			status.Resources[0].ResourceID = "other-core-uuid"
		}},
		{name: "wrong account", mutate: func(status *MigrationStatus) {
			status.Resources[1].ResourceID = "43"
		}},
		{name: "missing core user", mutate: func(status *MigrationStatus) {
			status.Resources = status.Resources[1:]
		}},
		{name: "missing account", mutate: func(status *MigrationStatus) {
			status.Resources = append(status.Resources[:1], status.Resources[2:]...)
		}},
		{name: "account remains", mutate: func(status *MigrationStatus) {
			status.Resources[1].Present = true
		}},
		{name: "wrong profile", mutate: func(status *MigrationStatus) {
			status.Resources[2].ResourceID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		}},
		{name: "profile remains", mutate: func(status *MigrationStatus) {
			status.Resources[2].Present = true
		}},
		{name: "wrong skin digest", mutate: func(status *MigrationStatus) {
			status.Resources[3].ResourceID = strings.Repeat("a", 64)
		}},
		{name: "missing preexisting core", mutate: func(status *MigrationStatus) {
			status.Resources[0].Present = false
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := rolledBackRecoveryStatus(t, record)
			tt.mutate(&status)
			staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
			client := &fakeRecoveryClient{reconcileStatus: status}
			recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
			if err := recovery.recover(context.Background(), record); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("recover() error = %v, want ErrInvalidResponse", err)
			}
			if len(staging.transitions) != 0 || len(staging.discards) != 0 || !reflect.DeepEqual(client.calls, []string{"reconcile"}) {
				t.Fatalf("unsafe cleanup proceeded: transitions = %#v, discards = %#v, calls = %#v", staging.transitions, staging.discards, client.calls)
			}
		})
	}
}

func TestRecoveryRequiresRollbackIdentityBeforeCleanup(t *testing.T) {
	record := recoveryRecord(t, StateRolledBack)
	record.RollbackIdentity = nil
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{reconcileStatus: rolledBackRecoveryStatus(t, record)}
	recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
	if err := recovery.recover(context.Background(), record); !errors.Is(err, ErrMigrationBlocked) {
		t.Fatalf("recover() error = %v, want ErrMigrationBlocked", err)
	}
	if len(staging.transitions) != 0 || len(staging.discards) != 0 || !reflect.DeepEqual(client.calls, []string{"reconcile"}) {
		t.Fatalf("legacy rollback was cleaned without identity: transitions = %#v, discards = %#v, calls = %#v", staging.transitions, staging.discards, client.calls)
	}
}

func TestRecoveryCleansRollbackBeforeCoreAndAccountAllocation(t *testing.T) {
	record := recoveryRecord(t, StateRolledBack)
	record.RollbackIdentity = &RollbackIdentity{}
	status := MigrationStatus{MigrationID: record.MigrationID, Status: MigrationRolledBack, Resources: []MigrationResource{
		{Kind: "profile", ResourceID: record.Profile.ID, Ownership: "created_by_migration", Present: false},
	}}
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{reconcileStatus: status}
	recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
	if err := recovery.recover(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(staging.transitions, []State{StateDiscarded}) || len(staging.discards) != 1 {
		t.Fatalf("transitions = %#v, discards = %#v", staging.transitions, staging.discards)
	}
}

func TestRecoveryCleansMigrationOwnedCoreAfterRollback(t *testing.T) {
	record := recoveryRecord(t, StateRolledBack)
	record.RollbackIdentity = &RollbackIdentity{
		CoreUserID:        "core-uuid",
		CoreUserOwnership: "created_by_migration",
		AccountID:         "42",
	}
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{reconcileStatus: rolledBackRecoveryStatus(t, record)}
	recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
	if err := recovery.recover(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(staging.transitions, []State{StateDiscarded}) || len(staging.discards) != 1 {
		t.Fatalf("transitions = %#v, discards = %#v", staging.transitions, staging.discards)
	}
	if !reflect.DeepEqual(client.calls, []string{"reconcile"}) {
		t.Fatalf("recovery calls = %#v", client.calls)
	}
}

func TestRecoveryRejectsResidualMigrationOwnedCore(t *testing.T) {
	record := recoveryRecord(t, StateRolledBack)
	record.RollbackIdentity = &RollbackIdentity{
		CoreUserID:        "core-uuid",
		CoreUserOwnership: "created_by_migration",
		AccountID:         "42",
	}
	status := rolledBackRecoveryStatus(t, record)
	status.Resources[0].Present = true
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{reconcileStatus: status}
	recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
	if err := recovery.recover(context.Background(), record); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("recover() error = %v, want ErrInvalidResponse", err)
	}
	if len(staging.transitions) != 0 || len(staging.discards) != 0 {
		t.Fatalf("residual Core user was cleaned: transitions = %#v, discards = %#v", staging.transitions, staging.discards)
	}
}

func TestRecoveryRejectsUnexpectedResourcesForEmptyRollbackIdentity(t *testing.T) {
	for _, resource := range []MigrationResource{
		{Kind: "core_user", ResourceID: "core-uuid", Ownership: "preexisting", Present: true},
		{Kind: "game_account", ResourceID: "42", Ownership: "created_by_migration", Present: false},
	} {
		t.Run(resource.Kind, func(t *testing.T) {
			record := recoveryRecord(t, StateRolledBack)
			record.RollbackIdentity = &RollbackIdentity{}
			status := rolledBackRecoveryStatus(t, record)
			status.Resources = append(status.Resources, resource)
			staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
			client := &fakeRecoveryClient{reconcileStatus: status}
			recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
			if err := recovery.recover(context.Background(), record); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("recover() error = %v, want ErrInvalidResponse", err)
			}
			if len(staging.transitions) != 0 || len(staging.discards) != 0 {
				t.Fatalf("unexpected resource was cleaned: transitions = %#v, discards = %#v", staging.transitions, staging.discards)
			}
		})
	}
}

func TestRecoveryRevalidatesRollingBackInventoryOnRestart(t *testing.T) {
	record := recoveryRecord(t, StateRollingBack)
	status := rolledBackRecoveryStatus(t, record)
	status.Resources[2].ResourceID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{reconcileStatus: status}
	recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
	if err := recovery.recover(context.Background(), record); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("recover() error = %v, want ErrInvalidResponse", err)
	}
	if len(staging.transitions) != 0 || len(staging.discards) != 0 || !reflect.DeepEqual(client.calls, []string{"reconcile"}) {
		t.Fatalf("rollback restart skipped inventory validation: transitions = %#v, discards = %#v, calls = %#v", staging.transitions, staging.discards, client.calls)
	}
}

func TestRecoveryRollbackRequiresClearOwnership(t *testing.T) {
	record := recoveryRecord(t, StateReconciling)
	skinHash := fmt.Sprintf("%x", sha256.Sum256(record.Skin.PNG))
	capeHash := fmt.Sprintf("%x", sha256.Sum256(record.Cape.PNG))
	tests := []struct {
		name      string
		status    string
		resources []MigrationResource
		want      error
	}{
		{name: "empty partial inventory", status: MigrationFailed, resources: []MigrationResource{
			{Kind: "profile", ResourceID: record.Profile.ID, Ownership: "created_by_migration", Present: false},
		}},
		{name: "owned partial resources", status: MigrationFailed, resources: []MigrationResource{
			{Kind: "core_user", ResourceID: "core-uuid", Ownership: "preexisting", Present: true},
			{Kind: "game_account", ResourceID: "42", Ownership: "created_by_migration", Present: true},
			{Kind: "profile", ResourceID: record.Profile.ID, Ownership: "created_by_migration", Present: true},
			{Kind: "skin", ResourceID: skinHash, Ownership: "preexisting", Present: true},
			{Kind: "cape", ResourceID: capeHash, Ownership: "preexisting", Present: false},
		}},
		{name: "unexpected asset", status: MigrationFailed, resources: []MigrationResource{
			{Kind: "skin", ResourceID: strings.Repeat("a", 64), Ownership: "preexisting", Present: true},
		}, want: ErrInvalidResponse},
		{name: "unknown outcome", status: MigrationUnknown, want: ErrMigrationBlocked},
		{name: "missing preexisting core", status: MigrationFailed, resources: []MigrationResource{
			{Kind: "core_user", ResourceID: "core-uuid", Ownership: "preexisting", Present: false},
		}, want: ErrInvalidResponse},
		{name: "created core compensation", status: MigrationFailed, resources: []MigrationResource{
			{Kind: "core_user", ResourceID: "core-uuid", Ownership: "created_by_migration", Present: true},
			{Kind: "profile", ResourceID: record.Profile.ID, Ownership: "created_by_migration", Present: false},
		}},
		{name: "created core missing", status: MigrationFailed, resources: []MigrationResource{
			{Kind: "core_user", ResourceID: "core-uuid", Ownership: "created_by_migration", Present: false},
		}, want: ErrInvalidResponse},
		{name: "unexpected profile", status: MigrationFailed, resources: []MigrationResource{
			{Kind: "profile", ResourceID: strings.Repeat("a", 32), Ownership: "created_by_migration", Present: true},
		}, want: ErrInvalidResponse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rollbackStatus := MigrationStatus{MigrationID: record.MigrationID, Status: MigrationRolledBack}
			for _, resource := range tt.resources {
				if (resource.Kind == "skin" || resource.Kind == "cape") && !resource.Present {
					continue
				}
				if resource.Kind == "game_account" || resource.Kind == "profile" ||
					(resource.Kind == "core_user" && resource.Ownership == "created_by_migration") {
					resource.Present = false
				}
				rollbackStatus.Resources = append(rollbackStatus.Resources, resource)
			}
			staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
			client := &fakeRecoveryClient{
				reconcileStatus: MigrationStatus{MigrationID: record.MigrationID, Status: tt.status, Resources: tt.resources},
				rollbackStatus:  rollbackStatus,
			}
			recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
			err := recovery.recover(context.Background(), record)
			if tt.want == nil {
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(client.calls, []string{"reconcile", "rollback"}) {
					t.Fatalf("recovery calls = %#v", client.calls)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("recover() error = %v, want %v", err, tt.want)
			}
			if !reflect.DeepEqual(client.calls, []string{"reconcile"}) || len(staging.transitions) != 0 {
				t.Fatalf("unsafe rollback proceeded: calls = %#v, transitions = %#v", client.calls, staging.transitions)
			}
		})
	}
}

func TestRecoveryRejectsMismatchedStatusIdentity(t *testing.T) {
	record := recoveryRecord(t, StateReconciling)
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{reconcileStatus: MigrationStatus{MigrationID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Status: MigrationFailed}}
	recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
	if err := recovery.recover(context.Background(), record); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("recover() error = %v, want ErrInvalidResponse", err)
	}
	if len(client.calls) != 1 || len(staging.transitions) != 0 {
		t.Fatalf("calls = %#v, transitions = %#v", client.calls, staging.transitions)
	}
}

func TestRecoveryRollsBackIncompleteMigrationAndDiscardsStaging(t *testing.T) {
	record := recoveryRecord(t, StateReconciling)
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{
		reconcileStatus: MigrationStatus{MigrationID: record.MigrationID, Status: MigrationApplying, Resources: []MigrationResource{
			{Kind: "profile", ResourceID: record.Profile.ID, Ownership: "created_by_migration", Present: false},
		}},
		rollbackStatus: MigrationStatus{MigrationID: record.MigrationID, Status: MigrationRolledBack, Resources: []MigrationResource{
			{Kind: "profile", ResourceID: record.Profile.ID, Ownership: "created_by_migration", Present: false},
		}},
	}
	recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
	if err := recovery.recover(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	wantTransitions := []State{StateRollingBack, StateRolledBack, StateDiscarded}
	if !reflect.DeepEqual(staging.transitions, wantTransitions) || !reflect.DeepEqual(staging.discards, []string{record.MigrationID}) {
		t.Fatalf("rollback transitions = %#v, discards = %#v", staging.transitions, staging.discards)
	}
	if !reflect.DeepEqual(client.calls, []string{"reconcile", "rollback"}) {
		t.Fatalf("recovery calls = %#v", client.calls)
	}
}

func TestRecoveryIdentityPersistenceFailurePreventsRollback(t *testing.T) {
	for _, persistenceErr := range []error{ErrVersionConflict, ErrCorruptStaging, errors.New("redis unavailable")} {
		t.Run(persistenceErr.Error(), func(t *testing.T) {
			record := recoveryRecord(t, StateReconciling)
			staging := &fakeRecoveryStaging{records: []StagedMigration{record}, recordIdentityErr: persistenceErr}
			client := &fakeRecoveryClient{
				reconcileStatus: rollbackOwnershipStatus(t, record),
				rollbackStatus:  rolledBackRecoveryStatus(t, record),
			}
			recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
			if err := recovery.recover(context.Background(), record); !errors.Is(err, persistenceErr) {
				t.Fatalf("recover() error = %v, want %v", err, persistenceErr)
			}
			if !reflect.DeepEqual(client.calls, []string{"reconcile"}) || len(staging.transitions) != 0 || len(staging.discards) != 0 {
				t.Fatalf("rollback crossed persistence barrier: calls = %#v, transitions = %#v, discards = %#v", client.calls, staging.transitions, staging.discards)
			}
		})
	}
}

func TestRecoveryRetainsIdentityAfterLostRollbackResponse(t *testing.T) {
	record := recoveryRecord(t, StateReconciling)
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{
		reconcileStatus: rollbackOwnershipStatus(t, record),
		rollbackStatus:  rolledBackRecoveryStatus(t, record),
		rollbackErr:     ErrUpstream,
	}
	recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
	if err := recovery.recover(context.Background(), record); !errors.Is(err, ErrUpstream) {
		t.Fatalf("recover() error = %v, want ErrUpstream", err)
	}
	current := staging.records[0]
	if current.RollbackIdentity == nil || *current.RollbackIdentity != (RollbackIdentity{CoreUserID: "core-uuid", CoreUserOwnership: "preexisting", AccountID: "42"}) {
		t.Fatalf("rollback identity was not retained: %+v", current)
	}
	if len(staging.transitions) != 0 || len(staging.discards) != 0 {
		t.Fatalf("ambiguous rollback changed staging: transitions = %#v, discards = %#v", staging.transitions, staging.discards)
	}
	client.rollbackErr = nil
	client.reconcileStatus = rolledBackRecoveryStatus(t, current)
	if err := recovery.recover(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(staging.transitions, []State{StateRollingBack, StateRolledBack, StateDiscarded}) || len(staging.discards) != 1 {
		t.Fatalf("restart transitions = %#v, discards = %#v", staging.transitions, staging.discards)
	}
	if !reflect.DeepEqual(client.calls, []string{"reconcile", "rollback", "reconcile"}) {
		t.Fatalf("restart calls = %#v", client.calls)
	}
}

func TestRecoveryRejectsRollbackWithResidualResources(t *testing.T) {
	record := recoveryRecord(t, StateReconciling)
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{
		reconcileStatus: MigrationStatus{MigrationID: record.MigrationID, Status: MigrationApplying, Resources: []MigrationResource{
			{Kind: "profile", ResourceID: record.Profile.ID, Ownership: "created_by_migration", Present: false},
		}},
		rollbackStatus: MigrationStatus{
			MigrationID: record.MigrationID,
			Status:      MigrationRolledBack,
			Resources: []MigrationResource{{
				Kind:       "profile",
				ResourceID: record.Profile.ID,
				Ownership:  "created_by_migration",
				Present:    true,
			}},
		},
	}
	recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
	if err := recovery.recover(context.Background(), record); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("recover() error = %v", err)
	}
	if len(staging.transitions) != 0 || len(staging.discards) != 0 || !reflect.DeepEqual(client.calls, []string{"reconcile", "rollback"}) {
		t.Fatalf("transitions = %#v, discards = %#v, calls = %#v", staging.transitions, staging.discards, client.calls)
	}
}

func TestRecoveryAcceptsCommitObservedDuringRollback(t *testing.T) {
	record := recoveryRecord(t, StateRollingBack)
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{reconcileStatus: committedRecoveryStatus(t, record)}
	bindings := &fakeRecoveryBindings{}
	recovery := newTestRecovery(staging, client, bindings)
	if err := recovery.recover(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(staging.transitions, []State{StateCommitted, StateLocked}) || len(bindings.bindCalls) != 1 {
		t.Fatalf("transitions = %#v, bindings = %#v", staging.transitions, bindings.bindCalls)
	}
}

func TestRecoveryNeverRollsBackAuthoritativeCommittedState(t *testing.T) {
	record := recoveryRecord(t, StateCommitted)
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{reconcileStatus: MigrationStatus{MigrationID: record.MigrationID, Status: MigrationRolledBack}}
	recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
	if err := recovery.recover(context.Background(), record); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("recover() error = %v", err)
	}
	if !reflect.DeepEqual(client.calls, []string{"reconcile"}) || len(staging.transitions) != 0 {
		t.Fatal("committed migration was rolled back or discarded")
	}
}

func TestRecoveryRejectsResultForDifferentProfile(t *testing.T) {
	record := recoveryRecord(t, StateCommitted)
	status := committedRecoveryStatus(t, record)
	status.Result.ProfileID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	status.Result.Session.SelectedProfile.ID = status.Result.ProfileID
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{reconcileStatus: status}
	bindings := &fakeRecoveryBindings{}
	recovery := newTestRecovery(staging, client, bindings)
	if err := recovery.recover(context.Background(), record); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("recover() error = %v", err)
	}
	if len(bindings.bindCalls) != 0 || len(staging.transitions) != 0 {
		t.Fatal("mismatched migration result was bound")
	}
}

func TestRecoveryStopsBeforeLockingWhenBindingFails(t *testing.T) {
	record := recoveryRecord(t, StateCommitted)
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{reconcileStatus: committedRecoveryStatus(t, record)}
	bindings := &fakeRecoveryBindings{bindErr: binding.ErrBindingConflict}
	recovery := newTestRecovery(staging, client, bindings)
	if err := recovery.recover(context.Background(), record); !errors.Is(err, binding.ErrBindingConflict) {
		t.Fatalf("recover() error = %v", err)
	}
	if len(staging.transitions) != 0 {
		t.Fatal("migration locked without a permanent binding")
	}
}

func TestRecoveryRunCleansAlreadyRolledBackStaging(t *testing.T) {
	record := recoveryRecord(t, StateRolledBack)
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{reconcileStatus: rolledBackRecoveryStatus(t, record)}
	recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
	if err := recovery.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(staging.transitions, []State{StateDiscarded}) || len(staging.discards) != 1 || !reflect.DeepEqual(client.calls, []string{"reconcile"}) {
		t.Fatalf("transitions = %#v, discards = %#v, calls = %#v", staging.transitions, staging.discards, client.calls)
	}
}

func TestRecoveryRunSkipsStagedAndLockedMigrations(t *testing.T) {
	staging := &fakeRecoveryStaging{records: []StagedMigration{recoveryRecord(t, StateStagedInRedis), recoveryRecord(t, StateLocked)}}
	client := &fakeRecoveryClient{}
	recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
	if err := recovery.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.calls) != 0 || len(staging.transitions) != 0 {
		t.Fatal("nonrecoverable migration was changed")
	}
}

func TestRecoveryRunFailsClosedWhenStagingCannotBeListed(t *testing.T) {
	staging := &fakeRecoveryStaging{listErr: errors.New("redis unavailable")}
	recovery := newTestRecovery(staging, &fakeRecoveryClient{}, &fakeRecoveryBindings{})
	if err := recovery.Run(context.Background()); err == nil {
		t.Fatal("Run() ignored staging list failure")
	}
}

func TestRecoveryRunContinuesAfterPerMigrationFailure(t *testing.T) {
	record := recoveryRecord(t, StateCommitted)
	staging := &fakeRecoveryStaging{records: []StagedMigration{record, record}}
	client := &fakeRecoveryClient{reconcileErr: errors.New("armorial unavailable")}
	recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
	if err := recovery.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.calls) != 2 {
		t.Fatalf("recovery calls = %#v, want both records attempted", client.calls)
	}
}

func TestRecoveryRunRecordsMetrics(t *testing.T) {
	record := recoveryRecord(t, StateCommitted)
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{reconcileErr: errors.New("armorial unavailable")}
	registry := metrics.New()
	recovery, err := NewRecovery(staging, client, &fakeRecoveryBindings{}, nil, registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	registry.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	for _, want := range []string{
		`scribe_recovery_scans_total{result="success"} 1`,
		`scribe_recovery_operations_total{result="blocked",state="COMMITTED"} 1`,
		`scribe_recovery_duration_seconds_count{result="blocked",state="COMMITTED"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("recovery metric missing %q: %s", want, body)
		}
	}
}

func TestRecoveryFailureLogsExcludeUpstreamDetails(t *testing.T) {
	const accessToken = "armorial-access-token-canary"
	const privateKey = "-----BEGIN PRIVATE KEY-----"
	record := recoveryRecord(t, StateReconciling)
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{reconcileErr: errors.New(accessToken + " " + privateKey)}
	var logs bytes.Buffer
	recovery, err := NewRecovery(staging, client, &fakeRecoveryBindings{}, slog.New(slog.NewTextHandler(&logs, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if logs.Len() == 0 {
		t.Fatal("recovery failure was not logged")
	}
	for _, secret := range []string{accessToken, privateKey} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("recovery log leaked secret %q: %s", secret, logs.String())
		}
	}
}

func TestRecoveryReconcileFailurePreservesStaging(t *testing.T) {
	record := recoveryRecord(t, StateReconciling)
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{reconcileErr: errors.New("armorial unavailable")}
	recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
	if err := recovery.recover(context.Background(), record); err == nil {
		t.Fatal("recover() ignored reconcile failure")
	}
	if len(staging.transitions) != 0 || len(staging.discards) != 0 || !reflect.DeepEqual(client.calls, []string{"reconcile"}) {
		t.Fatalf("transitions = %#v, discards = %#v, calls = %#v", staging.transitions, staging.discards, client.calls)
	}
}

func TestRecoveryRollbackConflictPreservesStaging(t *testing.T) {
	record := recoveryRecord(t, StateReconciling)
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{
		reconcileStatus: MigrationStatus{MigrationID: record.MigrationID, Status: MigrationApplying, Resources: []MigrationResource{
			{Kind: "profile", ResourceID: record.Profile.ID, Ownership: "created_by_migration", Present: false},
		}},
		rollbackErr: ErrRollbackNotAllowed,
	}
	recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
	if err := recovery.recover(context.Background(), record); !errors.Is(err, ErrRollbackNotAllowed) {
		t.Fatalf("recover() error = %v, want ErrRollbackNotAllowed", err)
	}
	if len(staging.transitions) != 0 || len(staging.discards) != 0 {
		t.Fatalf("transitions = %#v, discards = %#v", staging.transitions, staging.discards)
	}
}

func TestRecoveryRejectsUnknownRollbackResult(t *testing.T) {
	record := recoveryRecord(t, StateReconciling)
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{
		reconcileStatus: MigrationStatus{MigrationID: record.MigrationID, Status: MigrationApplying, Resources: []MigrationResource{
			{Kind: "profile", ResourceID: record.Profile.ID, Ownership: "created_by_migration", Present: false},
		}},
		rollbackStatus: MigrationStatus{MigrationID: record.MigrationID, Status: MigrationUnknown},
	}
	recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
	if err := recovery.recover(context.Background(), record); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("recover() error = %v, want ErrInvalidResponse", err)
	}
	if len(staging.transitions) != 0 || len(staging.discards) != 0 {
		t.Fatalf("transitions = %#v, discards = %#v", staging.transitions, staging.discards)
	}
}

func TestRecoveryTokenBindingFailurePreventsLock(t *testing.T) {
	record := recoveryRecord(t, StateCommitted)
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{reconcileStatus: committedRecoveryStatus(t, record)}
	bindings := &fakeRecoveryBindings{tokenErr: errors.New("sqlite unavailable")}
	recovery := newTestRecovery(staging, client, bindings)
	if err := recovery.recover(context.Background(), record); err == nil {
		t.Fatal("recover() ignored token binding failure")
	}
	if len(bindings.bindCalls) != 1 || len(bindings.tokens) != 1 || len(staging.transitions) != 0 {
		t.Fatalf("bindings = %#v, tokens = %#v, transitions = %#v", bindings.bindCalls, bindings.tokens, staging.transitions)
	}
}

func TestRecoveryRunStopsWhenContextIsCancelled(t *testing.T) {
	record := recoveryRecord(t, StateCommitted)
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{reconcileStatus: committedRecoveryStatus(t, record)}
	recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := recovery.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if len(client.calls) != 0 || len(staging.transitions) != 0 {
		t.Fatalf("calls = %#v, transitions = %#v", client.calls, staging.transitions)
	}
}

func TestNewRecoveryRequiresDependencies(t *testing.T) {
	if _, err := NewRecovery(nil, &fakeRecoveryClient{}, &fakeRecoveryBindings{}, nil, nil); err == nil {
		t.Fatal("NewRecovery() accepted nil staging")
	}
}
