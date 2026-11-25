package migration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/corematch/scribe/internal/protocol"
)

type captureCall struct {
	profile protocol.Profile
}

type fakeSnapshotCapturer struct {
	snapshot Snapshot
	err      error
	calls    []captureCall
}

func (f *fakeSnapshotCapturer) Capture(_ context.Context, profile protocol.Profile) (Snapshot, error) {
	f.calls = append(f.calls, captureCall{profile: profile})
	return f.snapshot, f.err
}

type stageCall struct {
	id             string
	identifierHash string
	snapshot       Snapshot
}

type fakeStagingStore struct {
	staged        StagedMigration
	stageErr      error
	stageCalls    []stageCall
	transitions   []State
	transitionErr error
}

func (f *fakeStagingStore) Stage(_ context.Context, id, identifierHash string, snapshot Snapshot) (StagedMigration, bool, error) {
	f.stageCalls = append(f.stageCalls, stageCall{id: id, identifierHash: identifierHash, snapshot: snapshot})
	if f.stageErr != nil {
		return StagedMigration{}, false, f.stageErr
	}
	if f.staged.MigrationID == "" {
		return StagedMigration{}, false, errors.New("missing staged migration")
	}
	return f.staged, false, nil
}

func (f *fakeStagingStore) Transition(_ context.Context, _ string, _ int64, next State) (StagedMigration, error) {
	f.transitions = append(f.transitions, next)
	if f.transitionErr != nil && next == StateCommitted {
		return StagedMigration{}, f.transitionErr
	}
	f.staged.State = next
	f.staged.Version++
	return f.staged, nil
}

type commitCall struct {
	request CommitRequest
}

type fakeCommitter struct {
	result CommitResult
	err    error
	calls  []commitCall
}

func (f *fakeCommitter) Commit(_ context.Context, request CommitRequest) (CommitResult, error) {
	f.calls = append(f.calls, commitCall{request: request})
	return f.result, f.err
}

func TestServiceMigratesSelectedProfileAndCommitsBeforeReturning(t *testing.T) {
	profile := protocol.Profile{ID: strings.Repeat("a", 32), Name: "Player"}
	capturer := &fakeSnapshotCapturer{snapshot: Snapshot{Profile: protocol.SignedProfile{ID: profile.ID, Name: profile.Name}}}
	staging := &fakeStagingStore{staged: StagedMigration{
		MigrationID:    "00000000-0000-4000-8000-000000000001",
		IdentifierHash: strings.Repeat("b", 64),
		State:          StateStagedInRedis,
		Version:        1,
	}}
	committer := &fakeCommitter{result: CommitResult{MigrationID: staging.staged.MigrationID, Status: "committed"}}
	service, err := NewService(capturer, staging, committer, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.Migrate(context.Background(), Input{
		Identifier:      "Player@Example.COM",
		Password:        "secret",
		SelectedProfile: profile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.MigrationID != staging.staged.MigrationID || result.Commit.Status != "committed" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if len(capturer.calls) != 1 || capturer.calls[0].profile != profile {
		t.Fatalf("unexpected capture calls: %#v", capturer.calls)
	}
	if len(staging.stageCalls) != 1 || !validMigrationID(staging.stageCalls[0].id) {
		t.Fatalf("unexpected stage calls: %#v", staging.stageCalls)
	}
	if staging.stageCalls[0].identifierHash == "" || len(staging.stageCalls[0].identifierHash) != 64 {
		t.Fatalf("missing identifier digest: %q", staging.stageCalls[0].identifierHash)
	}
	if len(staging.transitions) != 2 || staging.transitions[0] != StateApplying || staging.transitions[1] != StateCommitted {
		t.Fatalf("unexpected transitions: %#v", staging.transitions)
	}
	if len(committer.calls) != 1 || committer.calls[0].request.Identifier != "Player@example.com" || committer.calls[0].request.Password != "secret" {
		t.Fatalf("unexpected commit calls: %#v", committer.calls)
	}
}

func TestServiceBlocksConcurrentApply(t *testing.T) {
	committer := &fakeCommitter{}
	staging := &fakeStagingStore{staged: StagedMigration{
		MigrationID: "00000000-0000-4000-8000-000000000001",
		State:       StateApplying,
		Version:     2,
	}}
	service, err := NewService(&fakeSnapshotCapturer{}, staging, committer, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Migrate(context.Background(), Input{Identifier: "Player", Password: "secret"})
	if !errors.Is(err, ErrMigrationInProgress) {
		t.Fatalf("got %v, want ErrMigrationInProgress", err)
	}
	if len(committer.calls) != 0 || len(staging.transitions) != 0 {
		t.Fatal("in-progress migration was applied again")
	}
}

func TestServiceRetriesCommittedMigrationIdempotently(t *testing.T) {
	committer := &fakeCommitter{result: CommitResult{Status: "committed"}}
	staging := &fakeStagingStore{staged: StagedMigration{
		MigrationID: "00000000-0000-4000-8000-000000000001",
		State:       StateCommitted,
		Version:     3,
	}}
	service, err := NewService(&fakeSnapshotCapturer{}, staging, committer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Migrate(context.Background(), Input{Identifier: "Player", Password: "secret"}); err != nil {
		t.Fatal(err)
	}
	if len(committer.calls) != 1 {
		t.Fatal("committed migration was not retried for its session")
	}
	if len(staging.transitions) != 1 || staging.transitions[0] != StateCommitted {
		t.Fatalf("unexpected transitions: %#v", staging.transitions)
	}
}

func TestServiceMovesFailedCommitToReconciling(t *testing.T) {
	committer := &fakeCommitter{err: errors.New("upstream failed")}
	staging := &fakeStagingStore{staged: StagedMigration{
		MigrationID: "00000000-0000-4000-8000-000000000001",
		State:       StateStagedInRedis,
		Version:     1,
	}}
	service, err := NewService(&fakeSnapshotCapturer{}, staging, committer, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Migrate(context.Background(), Input{Identifier: "Player", Password: "secret"})
	if err == nil {
		t.Fatal("expected commit failure")
	}
	want := []State{StateApplying, StateReconciling}
	if len(staging.transitions) != len(want) {
		t.Fatalf("unexpected transitions: %#v", staging.transitions)
	}
	for i := range want {
		if staging.transitions[i] != want[i] {
			t.Fatalf("unexpected transitions: %#v", staging.transitions)
		}
	}
}

func TestServiceReturnsCommittedResultWhenBookkeepingFails(t *testing.T) {
	committer := &fakeCommitter{result: CommitResult{Status: "committed"}}
	staging := &fakeStagingStore{
		staged: StagedMigration{
			MigrationID: "00000000-0000-4000-8000-000000000001",
			State:       StateStagedInRedis,
			Version:     1,
		},
		transitionErr: errors.New("redis unavailable"),
	}
	service, err := NewService(&fakeSnapshotCapturer{}, staging, committer, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Migrate(context.Background(), Input{Identifier: "Player", Password: "secret"})
	if err != nil || result.Commit.Status != "committed" {
		t.Fatalf("committed result was lost: %#v, %v", result, err)
	}
}

func TestServiceRejectsInvalidInputBeforeCapture(t *testing.T) {
	capturer := &fakeSnapshotCapturer{}
	service, err := NewService(capturer, &fakeStagingStore{}, &fakeCommitter{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Migrate(context.Background(), Input{Identifier: "\x00"}); !errors.Is(err, ErrInvalidCommit) {
		t.Fatalf("got %v, want ErrInvalidCommit", err)
	}
	if len(capturer.calls) != 0 {
		t.Fatal("captured an invalid identifier")
	}
}
