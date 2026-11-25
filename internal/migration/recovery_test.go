package migration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/corematch/scribe/internal/binding"
)

type fakeRecoveryStaging struct {
	records        []StagedMigration
	listErr        error
	transitions    []State
	discards       []string
	failTransition State
	discardErr     error
}

func (f *fakeRecoveryStaging) List(context.Context) ([]StagedMigration, error) {
	return f.records, f.listErr
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
	return record
}

func committedRecoveryStatus(t *testing.T, record StagedMigration) MigrationStatus {
	t.Helper()
	result := successfulCommitResult(record, "uploaded", "uploaded")
	return MigrationStatus{MigrationID: record.MigrationID, Status: MigrationCommitted, Result: &result}
}

func newTestRecovery(staging *fakeRecoveryStaging, client *fakeRecoveryClient, bindings *fakeRecoveryBindings) *Recovery {
	recovery, err := NewRecovery(staging, client, bindings, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

func TestRecoveryLeavesCommittedMigrationWithoutSessionForRetry(t *testing.T) {
	record := recoveryRecord(t, StateCommitted)
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{reconcileStatus: MigrationStatus{MigrationID: record.MigrationID, Status: MigrationCommitted}}
	bindings := &fakeRecoveryBindings{}
	recovery := newTestRecovery(staging, client, bindings)
	if err := recovery.recover(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if len(staging.transitions) != 0 || len(bindings.bindCalls) != 0 || len(bindings.tokens) != 0 {
		t.Fatal("recovery fabricated a session or changed committed state")
	}
}

func TestRecoveryRollsBackIncompleteMigrationAndDiscardsStaging(t *testing.T) {
	record := recoveryRecord(t, StateReconciling)
	staging := &fakeRecoveryStaging{records: []StagedMigration{record}}
	client := &fakeRecoveryClient{
		reconcileStatus: MigrationStatus{MigrationID: record.MigrationID, Status: MigrationApplying},
		rollbackStatus:  MigrationStatus{MigrationID: record.MigrationID, Status: MigrationRolledBack},
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
	client := &fakeRecoveryClient{}
	recovery := newTestRecovery(staging, client, &fakeRecoveryBindings{})
	if err := recovery.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(staging.transitions, []State{StateDiscarded}) || len(staging.discards) != 1 || len(client.calls) != 0 {
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
		reconcileStatus: MigrationStatus{MigrationID: record.MigrationID, Status: MigrationApplying},
		rollbackErr:     ErrRollbackNotAllowed,
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
		reconcileStatus: MigrationStatus{MigrationID: record.MigrationID, Status: MigrationApplying},
		rollbackStatus:  MigrationStatus{MigrationID: record.MigrationID, Status: MigrationUnknown},
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
	if _, err := NewRecovery(nil, &fakeRecoveryClient{}, &fakeRecoveryBindings{}, nil); err == nil {
		t.Fatal("NewRecovery() accepted nil staging")
	}
}
