package migration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/corematch/scribe/internal/binding"
	"github.com/redis/go-redis/v9"
)

func TestRecoveryRepeatedStartupIsIdempotent(t *testing.T) {
	for _, scenario := range []string{"committed", "rolled_back", "committed_without_result"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			redisServer := miniredis.RunT(t)
			redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
			t.Cleanup(func() { _ = redisClient.Close() })
			staging, err := NewRedisStore(redisClient, RedisOptions{})
			if err != nil {
				t.Fatal(err)
			}
			bindings, err := binding.OpenSQLite(filepath.Join(t.TempDir(), "scribe.db"), 15*24*time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = bindings.Close() })

			const identifier = "LegacyPlayer"
			digest := sha256.Sum256([]byte(identifier))
			record, created, err := staging.Stage(ctx, testMigrationID, hex.EncodeToString(digest[:]), validDigestSnapshot(t))
			if err != nil || !created {
				t.Fatalf("Stage() = %+v, %v, %v", record, created, err)
			}
			record, err = staging.Transition(ctx, record.MigrationID, record.Version, StateApplying)
			if err != nil {
				t.Fatal(err)
			}

			client := &fakeRecoveryClient{}
			switch scenario {
			case "committed":
				record, err = staging.Transition(ctx, record.MigrationID, record.Version, StateCommitted)
				if err != nil {
					t.Fatal(err)
				}
				client.reconcileStatus = committedRecoveryStatus(t, record)
			case "rolled_back":
				record, err = staging.Transition(ctx, record.MigrationID, record.Version, StateReconciling)
				if err != nil {
					t.Fatal(err)
				}
				resources := []MigrationResource{
					{Kind: "core_user", ResourceID: "core-uuid", Ownership: "preexisting", Present: true},
					{Kind: "profile", ResourceID: record.Profile.ID, Ownership: "created_by_migration", Present: false},
				}
				client.reconcileStatus = MigrationStatus{MigrationID: record.MigrationID, Status: MigrationFailed, Resources: resources}
				client.rollbackStatus = MigrationStatus{MigrationID: record.MigrationID, Status: MigrationRolledBack, Resources: resources}
			case "committed_without_result":
				record, err = staging.Transition(ctx, record.MigrationID, record.Version, StateCommitted)
				if err != nil {
					t.Fatal(err)
				}
				client.reconcileStatus = MigrationStatus{MigrationID: record.MigrationID, Status: MigrationCommitted}
			}

			runStartup := func() error {
				recovery, err := NewRecovery(staging, client, bindings, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
				if err != nil {
					return err
				}
				return recovery.Run(ctx)
			}
			if err := runStartup(); err != nil {
				t.Fatal(err)
			}
			if err := runStartup(); err != nil {
				t.Fatal(err)
			}

			switch scenario {
			case "committed":
				current, err := staging.Load(ctx, record.MigrationID)
				if err != nil || current.State != StateLocked {
					t.Fatalf("recovered record = %+v, %v", current, err)
				}
				bound, err := bindings.GetBinding(ctx, identifier)
				if err != nil || bound.Winner != binding.Armorial || bound.MigrationID != record.MigrationID {
					t.Fatalf("binding = %+v, %v", bound, err)
				}
				tokenBinding, err := bindings.GetBindingByToken(ctx, "new-access-token")
				if err != nil || tokenBinding.Winner != binding.Armorial || tokenBinding.MigrationID != record.MigrationID {
					t.Fatalf("token binding = %+v, %v", tokenBinding, err)
				}
				if len(client.calls) != 1 {
					t.Fatalf("recovery calls = %#v, want one reconcile", client.calls)
				}
			case "rolled_back":
				if _, err := staging.Load(ctx, record.MigrationID); !errors.Is(err, ErrNotFound) {
					t.Fatalf("rolled-back staging still exists: %v", err)
				}
				if _, err := bindings.GetBinding(ctx, identifier); !errors.Is(err, binding.ErrNotFound) {
					t.Fatalf("rolled-back migration created a binding: %v", err)
				}
				if len(client.calls) != 2 {
					t.Fatalf("recovery calls = %#v, want reconcile and rollback once", client.calls)
				}
			case "committed_without_result":
				current, err := staging.Load(ctx, record.MigrationID)
				if err != nil || current.State != StateCommitted {
					t.Fatalf("blocked record = %+v, %v", current, err)
				}
				if _, err := bindings.GetBinding(ctx, identifier); !errors.Is(err, binding.ErrNotFound) {
					t.Fatalf("migration without an authoritative result was bound: %v", err)
				}
				if len(client.calls) != 2 {
					t.Fatalf("recovery calls = %#v, want both startups blocked", client.calls)
				}
			}
		})
	}
}
