package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/corematch/scribe/internal/assets"
	"github.com/corematch/scribe/internal/protocol"
	"github.com/redis/go-redis/v9"
)

const testMigrationID = "12345678-1234-1234-1234-123456789abc"

func TestRedisStageStoresAtomicCredentialFreePackage(t *testing.T) {
	server := miniredis.RunT(t)
	store, client := testRedisStore(t, server, RedisOptions{})
	snapshot := validDigestSnapshot(t)
	snapshot.Profile.Properties = []protocol.ProfileProperty{{Name: "textures", Value: "signed-value-secret", Signature: "signature-secret"}}
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	identifierHash := strings.Repeat("ab", 32)

	record, created, err := store.Stage(context.Background(), testMigrationID, identifierHash, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !created || record.State != StateStagedInRedis || record.Version != 1 || len(record.CommitDigest) != 64 {
		t.Fatalf("Stage() = %+v, created=%v", record, created)
	}
	if record.Skin == nil || record.Cape == nil || !bytes.Equal(record.Skin.PNG, snapshot.Assets["SKIN"].PNG) {
		t.Fatalf("staged assets = %+v", record)
	}
	loaded, err := store.Load(context.Background(), testMigrationID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, record) {
		t.Fatalf("Load() = %+v, want %+v", loaded, record)
	}

	key := stagingKey(testMigrationID)
	payload, err := client.HGet(context.Background(), key, "payload").Bytes()
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"textures.example", "token=secret", "signed-value-secret", "signature-secret", "password", "accessToken", "properties"} {
		if bytes.Contains(payload, []byte(forbidden)) {
			t.Fatalf("staged payload contains %q: %s", forbidden, payload)
		}
	}
	skin, err := client.HGet(context.Background(), key, "skin").Bytes()
	if err != nil || !bytes.Equal(skin, snapshot.Assets["SKIN"].PNG) {
		t.Fatalf("staged skin = %x, %v", skin, err)
	}
	if ttl := server.TTL(key); ttl != 24*time.Hour {
		t.Fatalf("TTL = %v", ttl)
	}
}

func TestRedisStageIsIdempotentAndRejectsDigestConflicts(t *testing.T) {
	server := miniredis.RunT(t)
	store, _ := testRedisStore(t, server, RedisOptions{})
	snapshot := validDigestSnapshot(t)
	identifierHash := strings.Repeat("ab", 32)
	first, created, err := store.Stage(context.Background(), testMigrationID, identifierHash, snapshot)
	if err != nil || !created {
		t.Fatalf("Stage() = %+v, %v, %v", first, created, err)
	}

	equivalent := validDigestSnapshot(t)
	equivalent.Textures.Timestamp++
	skin := equivalent.Textures.Textures["SKIN"]
	skin.URL = "https://textures.example/different.png"
	equivalent.Textures.Textures["SKIN"] = skin
	server.FastForward(time.Hour)
	second, created, err := store.Stage(context.Background(), testMigrationID, strings.ToUpper(identifierHash), equivalent)
	if err != nil || created || !reflect.DeepEqual(first, second) {
		t.Fatalf("idempotent Stage() = %+v, %v, %v", second, created, err)
	}
	if ttl := server.TTL(stagingKey(testMigrationID)); ttl != 24*time.Hour {
		t.Fatalf("refreshed TTL = %v", ttl)
	}

	conflicting := validDigestSnapshot(t)
	conflicting.Profile.Name = "DifferentPlayer"
	conflicting.Textures.ProfileName = conflicting.Profile.Name
	if _, _, err := store.Stage(context.Background(), testMigrationID, identifierHash, conflicting); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting Stage() error = %v", err)
	}
	loaded, err := store.Load(context.Background(), testMigrationID)
	if err != nil || !reflect.DeepEqual(loaded, first) {
		t.Fatalf("conflict changed record: %+v, %v", loaded, err)
	}
}

func TestRedisIdentifierRetryRefreshesExistingPackageTTL(t *testing.T) {
	server := miniredis.RunT(t)
	store, client := testRedisStore(t, server, RedisOptions{})
	identifierHash := strings.Repeat("ab", 32)
	secondID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	first, created, err := store.Stage(context.Background(), testMigrationID, identifierHash, validDigestSnapshot(t))
	if err != nil || !created {
		t.Fatalf("Stage() = %+v, %v, %v", first, created, err)
	}
	server.FastForward(23 * time.Hour)
	retried, created, err := store.Stage(context.Background(), secondID, identifierHash, validDigestSnapshot(t))
	if err != nil || created || !reflect.DeepEqual(retried, first) {
		t.Fatalf("retry Stage() = %+v, %v, %v", retried, created, err)
	}
	for _, key := range []string{stagingKey(testMigrationID), identifierIndexKey(identifierHash)} {
		if ttl := server.TTL(key); ttl != 24*time.Hour {
			t.Fatalf("TTL(%q) = %v, want 24h", key, ttl)
		}
	}
	if exists, err := client.Exists(context.Background(), stagingKey(secondID)).Result(); err != nil || exists != 0 {
		t.Fatalf("retry created proposed package: exists=%d, err=%v", exists, err)
	}
}

func TestRedisIdentifierRetryDoesNotRefreshMissingPackageIndex(t *testing.T) {
	server := miniredis.RunT(t)
	store, client := testRedisStore(t, server, RedisOptions{})
	identifierHash := strings.Repeat("ab", 32)
	if _, _, err := store.Stage(context.Background(), testMigrationID, identifierHash, validDigestSnapshot(t)); err != nil {
		t.Fatal(err)
	}
	if err := client.Del(context.Background(), stagingKey(testMigrationID)).Err(); err != nil {
		t.Fatal(err)
	}
	server.FastForward(23 * time.Hour)
	secondID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	if _, _, err := store.Stage(context.Background(), secondID, identifierHash, validDigestSnapshot(t)); err == nil {
		t.Fatal("Stage() reused an index with a missing package")
	}
	if ttl := server.TTL(identifierIndexKey(identifierHash)); ttl != time.Hour {
		t.Fatalf("stale index TTL = %v, want 1h", ttl)
	}
}

func TestRedisActiveMigrationsOutliveStagingTTL(t *testing.T) {
	server := miniredis.RunT(t)
	store, _ := testRedisStore(t, server, RedisOptions{TTL: time.Hour})
	identifierDigest := sha256.Sum256([]byte("Player@example.com"))
	identifierHash := hex.EncodeToString(identifierDigest[:])
	record, created, err := store.Stage(context.Background(), testMigrationID, identifierHash, validDigestSnapshot(t))
	if err != nil || !created {
		t.Fatalf("Stage() = %+v, %v, %v", record, created, err)
	}
	for _, next := range []State{StateApplying, StateReconciling, StateUnknown, StateReconciling, StateRollingBack, StateCommitted, StateLocked} {
		if next == StateRollingBack {
			record = confirmTestRollback(t, store, record)
		} else {
			record, err = store.Transition(context.Background(), record.MigrationID, record.Version, next)
			if err != nil {
				t.Fatalf("Transition(%s): %v", next, err)
			}
		}
		if next == StateApplying {
			retryID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
			retried, created, err := store.Stage(context.Background(), retryID, identifierHash, validDigestSnapshot(t))
			if err != nil || created || retried.State != StateApplying {
				t.Fatalf("active retry = %+v, %v, %v", retried, created, err)
			}
		}
		ttl := time.Duration(0)
		if next == StateLocked {
			ttl = time.Hour
		}
		for _, key := range []string{stagingKey(record.MigrationID), identifierIndexKey(identifierHash)} {
			if got := server.TTL(key); got != ttl {
				t.Fatalf("TTL(%q) in %s = %v, want %v", key, next, got, ttl)
			}
		}
		server.FastForward(2 * time.Hour)
		loaded, err := store.Load(context.Background(), record.MigrationID)
		if next == StateLocked {
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("locked migration did not expire: %+v, %v", loaded, err)
			}
			continue
		}
		if err != nil || loaded.State != next {
			t.Fatalf("Load() in %s = %+v, %v", next, loaded, err)
		}
		if pending, err := store.Pending(context.Background(), "Player@Example.COM"); err != nil || !pending {
			t.Fatalf("Pending() in %s = %v, %v", next, pending, err)
		}
	}
}

func TestRedisTransitionRejectsMismatchedIdentifierIndex(t *testing.T) {
	server := miniredis.RunT(t)
	store, client := testRedisStore(t, server, RedisOptions{})
	identifierHash := strings.Repeat("ab", 32)
	if _, _, err := store.Stage(context.Background(), testMigrationID, identifierHash, validDigestSnapshot(t)); err != nil {
		t.Fatal(err)
	}
	if err := client.Set(context.Background(), identifierIndexKey(identifierHash), "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), testMigrationID, 1, StateApplying); !errors.Is(err, ErrCorruptStaging) {
		t.Fatalf("Transition() error = %v", err)
	}
	record, err := store.Load(context.Background(), testMigrationID)
	if err != nil || record.State != StateStagedInRedis || record.Version != 1 {
		t.Fatalf("record changed despite corrupt index: %+v, %v", record, err)
	}
}

func TestRedisTransitionRepairsMissingIdentifierIndex(t *testing.T) {
	server := miniredis.RunT(t)
	store, client := testRedisStore(t, server, RedisOptions{TTL: time.Hour})
	identifierHash := strings.Repeat("ab", 32)
	if _, _, err := store.Stage(context.Background(), testMigrationID, identifierHash, validDigestSnapshot(t)); err != nil {
		t.Fatal(err)
	}
	if err := client.Del(context.Background(), identifierIndexKey(identifierHash)).Err(); err != nil {
		t.Fatal(err)
	}
	record, err := store.Transition(context.Background(), testMigrationID, 1, StateApplying)
	if err != nil || record.State != StateApplying {
		t.Fatalf("Transition() = %+v, %v", record, err)
	}
	indexed, err := client.Get(context.Background(), identifierIndexKey(identifierHash)).Result()
	if err != nil || indexed != testMigrationID || server.TTL(identifierIndexKey(identifierHash)) != 0 {
		t.Fatalf("repaired index = %q, TTL = %v, error = %v", indexed, server.TTL(identifierIndexKey(identifierHash)), err)
	}
}

func TestRedisStageWithoutAssets(t *testing.T) {
	server := miniredis.RunT(t)
	store, _ := testRedisStore(t, server, RedisOptions{})
	snapshot := Snapshot{
		Profile: protocol.SignedProfile{ID: snapshotProfileID, Name: "LegacyPlayer"},
		Textures: protocol.Textures{
			ProfileID:   snapshotProfileID,
			ProfileName: "LegacyPlayer",
			Textures:    map[string]protocol.Texture{},
		},
		Assets: map[string]assets.Asset{},
	}
	record, created, err := store.Stage(context.Background(), testMigrationID, strings.Repeat("ab", 32), snapshot)
	if err != nil || !created {
		t.Fatalf("Stage() = %+v, %v, %v", record, created, err)
	}
	loaded, err := store.Load(context.Background(), testMigrationID)
	if err != nil || loaded.Skin != nil || loaded.Cape != nil {
		t.Fatalf("Load() = %+v, %v", loaded, err)
	}
}

func TestRedisStageReusesMigrationByIdentifier(t *testing.T) {
	server := miniredis.RunT(t)
	store, client := testRedisStore(t, server, RedisOptions{})
	identifierHash := strings.Repeat("ab", 32)
	secondID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	first, created, err := store.Stage(context.Background(), testMigrationID, identifierHash, validDigestSnapshot(t))
	if err != nil || !created {
		t.Fatalf("Stage() = %+v, %v, %v", first, created, err)
	}
	second, created, err := store.Stage(context.Background(), secondID, identifierHash, validDigestSnapshot(t))
	if err != nil || created || !reflect.DeepEqual(first, second) {
		t.Fatalf("retry Stage() = %+v, %v, %v", second, created, err)
	}
	indexed, err := client.Get(context.Background(), identifierIndexKey(identifierHash)).Result()
	if err != nil || indexed != testMigrationID {
		t.Fatalf("identifier index = %q, %v", indexed, err)
	}
	if ttl := server.TTL(identifierIndexKey(identifierHash)); ttl != 24*time.Hour {
		t.Fatalf("index TTL = %v", ttl)
	}
	conflicting := validDigestSnapshot(t)
	conflicting.Profile.Name = "DifferentPlayer"
	conflicting.Textures.ProfileName = conflicting.Profile.Name
	if _, _, err := store.Stage(context.Background(), secondID, identifierHash, conflicting); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed retry Stage() error = %v", err)
	}
}

func TestRedisStageConcurrentIdentifiersReuseOneMigration(t *testing.T) {
	server := miniredis.RunT(t)
	store, _ := testRedisStore(t, server, RedisOptions{})
	identifierHash := strings.Repeat("ab", 32)
	secondID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const workers = 10
	type stageResult struct {
		record  StagedMigration
		created bool
		err     error
	}
	results := make(chan stageResult, workers)
	var wait sync.WaitGroup
	for index := range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			id := testMigrationID
			if index%2 == 1 {
				id = secondID
			}
			record, created, err := store.Stage(context.Background(), id, identifierHash, validDigestSnapshot(t))
			results <- stageResult{record: record, created: created, err: err}
		}()
	}
	wait.Wait()
	close(results)
	migrationID := ""
	creators := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("Stage() error = %v", result.err)
		}
		if result.created {
			creators++
		}
		if migrationID == "" {
			migrationID = result.record.MigrationID
		}
		if result.record.MigrationID != migrationID {
			t.Fatalf("concurrent Stage() IDs = %q and %q", migrationID, result.record.MigrationID)
		}
	}
	if creators != 1 {
		t.Fatalf("creators = %d", creators)
	}
}

func TestRedisStageAllowsOneConcurrentCreator(t *testing.T) {
	server := miniredis.RunT(t)
	store, _ := testRedisStore(t, server, RedisOptions{})
	snapshot := validDigestSnapshot(t)
	const workers = 10
	created := make(chan bool, workers)
	errs := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, isNew, err := store.Stage(context.Background(), testMigrationID, strings.Repeat("ab", 32), snapshot)
			created <- isNew
			errs <- err
		}()
	}
	wait.Wait()
	close(created)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Stage() error = %v", err)
		}
	}
	creators := 0
	for isNew := range created {
		if isNew {
			creators++
		}
	}
	if creators != 1 {
		t.Fatalf("creators = %d", creators)
	}
}

func TestRedisTransitionUsesCompareAndSet(t *testing.T) {
	server := miniredis.RunT(t)
	store, _ := testRedisStore(t, server, RedisOptions{})
	if _, _, err := store.Stage(context.Background(), testMigrationID, strings.Repeat("ab", 32), validDigestSnapshot(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), testMigrationID, 1, StateCommitted); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("invalid transition error = %v", err)
	}
	applying, err := store.Transition(context.Background(), testMigrationID, 1, StateApplying)
	if err != nil || applying.Version != 2 || applying.State != StateApplying {
		t.Fatalf("Transition() = %+v, %v", applying, err)
	}
	if _, err := store.Transition(context.Background(), testMigrationID, 1, StateApplying); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale transition error = %v", err)
	}
	committed, err := store.Transition(context.Background(), testMigrationID, 2, StateCommitted)
	if err != nil || committed.State != StateCommitted || committed.Version != 3 {
		t.Fatalf("Transition() = %+v, %v", committed, err)
	}
	locked, err := store.Transition(context.Background(), testMigrationID, 3, StateLocked)
	if err != nil || locked.State != StateLocked || locked.Version != 4 {
		t.Fatalf("Transition() = %+v, %v", locked, err)
	}
	if _, err := store.Transition(context.Background(), testMigrationID, 4, StateRollingBack); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("committed rollback error = %v", err)
	}
}

func TestRedisTransitionRejectsUnreconciledDiscard(t *testing.T) {
	server := miniredis.RunT(t)
	store, _ := testRedisStore(t, server, RedisOptions{})
	if _, _, err := store.Stage(context.Background(), testMigrationID, strings.Repeat("ab", 32), validDigestSnapshot(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), testMigrationID, 1, StateDiscarded); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("staged discard error = %v", err)
	}
	if _, err := store.Transition(context.Background(), testMigrationID, 1, StateRollingBack); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("staged rollback error = %v", err)
	}
	record, err := store.Transition(context.Background(), testMigrationID, 1, StateApplying)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), testMigrationID, record.Version, StateRollingBack); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("applying rollback error = %v", err)
	}
	record, err = store.Transition(context.Background(), testMigrationID, record.Version, StateReconciling)
	if err != nil {
		t.Fatal(err)
	}
	for _, next := range []State{StateRolledBack, StateDiscarded} {
		if _, err := store.Transition(context.Background(), testMigrationID, record.Version, next); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("reconciling %s error = %v", next, err)
		}
	}
	record = confirmTestRollback(t, store, record)
	record, err = store.Transition(context.Background(), testMigrationID, record.Version, StateUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), testMigrationID, record.Version, StateDiscarded); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("unknown discard error = %v", err)
	}
	if _, err := store.Transition(context.Background(), testMigrationID, record.Version, StateRollingBack); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("unknown rollback error = %v", err)
	}
}

func TestRedisConfirmRollbackRequiresAuthoritativeStatus(t *testing.T) {
	server := miniredis.RunT(t)
	store, _ := testRedisStore(t, server, RedisOptions{})
	if _, _, err := store.Stage(context.Background(), testMigrationID, strings.Repeat("ab", 32), validDigestSnapshot(t)); err != nil {
		t.Fatal(err)
	}
	record, err := store.Transition(context.Background(), testMigrationID, 1, StateApplying)
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.Transition(context.Background(), testMigrationID, record.Version, StateReconciling)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []MigrationStatus{
		{MigrationID: testMigrationID, Status: MigrationApplying},
		{MigrationID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Status: MigrationRolledBack},
		{MigrationID: testMigrationID, Status: MigrationRolledBack, Result: &CommitResult{}},
		{MigrationID: testMigrationID, Status: MigrationRolledBack, Resources: []MigrationResource{
			{Kind: "profile", ResourceID: strings.Repeat("a", 32), Ownership: "created_by_migration", Present: true},
		}},
		{MigrationID: testMigrationID, Status: MigrationRolledBack, Resources: []MigrationResource{
			{Kind: "core_user", ResourceID: "42", Ownership: "preexisting", Present: false},
		}},
		{MigrationID: testMigrationID, Status: MigrationRolledBack, Resources: []MigrationResource{
			{Kind: "profile", ResourceID: strings.Repeat("a", 32), Ownership: "created_by_migration", Present: false},
			{Kind: "profile", ResourceID: strings.Repeat("a", 32), Ownership: "preexisting", Present: true},
		}},
	} {
		if _, err := store.ConfirmRollback(context.Background(), testMigrationID, record.Version, status); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("ConfirmRollback(%+v) error = %v", status, err)
		}
	}
	record = confirmTestRollback(t, store, record)
	if record.State != StateRollingBack {
		t.Fatalf("confirmed rollback state = %s", record.State)
	}
}

func TestRedisTransitionAllowsOneConcurrentWorker(t *testing.T) {
	server := miniredis.RunT(t)
	store, _ := testRedisStore(t, server, RedisOptions{})
	if _, _, err := store.Stage(context.Background(), testMigrationID, strings.Repeat("ab", 32), validDigestSnapshot(t)); err != nil {
		t.Fatal(err)
	}
	const workers = 12
	results := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := store.Transition(context.Background(), testMigrationID, 1, StateApplying)
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	successes := 0
	versionConflicts := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrVersionConflict):
			versionConflicts++
		default:
			t.Fatalf("unexpected transition error: %v", err)
		}
	}
	if successes != 1 || versionConflicts != workers-1 {
		t.Fatalf("successes=%d, versionConflicts=%d", successes, versionConflicts)
	}
}

func TestRedisStagingExpiresAndDetectsCorruption(t *testing.T) {
	t.Run("expiry", func(t *testing.T) {
		server := miniredis.RunT(t)
		store, _ := testRedisStore(t, server, RedisOptions{TTL: time.Hour})
		if _, _, err := store.Stage(context.Background(), testMigrationID, strings.Repeat("ab", 32), validDigestSnapshot(t)); err != nil {
			t.Fatal(err)
		}
		server.FastForward(2 * time.Hour)
		if _, err := store.Load(context.Background(), testMigrationID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Load() error = %v", err)
		}
	})
	t.Run("asset corruption", func(t *testing.T) {
		server := miniredis.RunT(t)
		store, client := testRedisStore(t, server, RedisOptions{})
		if _, _, err := store.Stage(context.Background(), testMigrationID, strings.Repeat("ab", 32), validDigestSnapshot(t)); err != nil {
			t.Fatal(err)
		}
		if err := client.HSet(context.Background(), stagingKey(testMigrationID), "skin", "corrupt").Err(); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(context.Background(), testMigrationID); !errors.Is(err, ErrCorruptStaging) {
			t.Fatalf("Load() error = %v", err)
		}
	})
	t.Run("state corruption", func(t *testing.T) {
		server := miniredis.RunT(t)
		store, client := testRedisStore(t, server, RedisOptions{})
		if _, _, err := store.Stage(context.Background(), testMigrationID, strings.Repeat("ab", 32), validDigestSnapshot(t)); err != nil {
			t.Fatal(err)
		}
		if err := client.HSet(context.Background(), stagingKey(testMigrationID), "state", "INVALID").Err(); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(context.Background(), testMigrationID); !errors.Is(err, ErrCorruptStaging) {
			t.Fatalf("Load() error = %v", err)
		}
	})
}

func TestRedisStoreRejectsInvalidInputs(t *testing.T) {
	server := miniredis.RunT(t)
	store, _ := testRedisStore(t, server, RedisOptions{})
	snapshot := validDigestSnapshot(t)
	if _, _, err := store.Stage(context.Background(), "not-a-uuid", strings.Repeat("ab", 32), snapshot); err == nil {
		t.Fatal("Stage() accepted an invalid migration ID")
	}
	if _, err := store.Load(context.Background(), "not-a-uuid"); err == nil {
		t.Fatal("Load() accepted an invalid migration ID")
	}
	if _, err := store.Transition(context.Background(), testMigrationID, 1, State("INVALID")); err == nil {
		t.Fatal("Transition() accepted an invalid state")
	}
	smallStore, err := NewRedisStore(store.client, RedisOptions{MaxPackageBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := smallStore.Stage(context.Background(), testMigrationID, strings.Repeat("ab", 32), snapshot); !errors.Is(err, ErrPackageTooLarge) {
		t.Fatalf("Stage() error = %v", err)
	}
	unsafe := validDigestSnapshot(t)
	skin := unsafe.Textures.Textures["SKIN"]
	skin.Metadata = map[string]string{"model": "slim", "password": "secret"}
	unsafe.Textures.Textures["SKIN"] = skin
	identifierHash := strings.Repeat("ab", 32)
	if _, _, err := store.Stage(context.Background(), testMigrationID, identifierHash, unsafe); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("unsafe metadata Stage() error = %v", err)
	}
	if count, err := store.client.Exists(context.Background(), stagingKey(testMigrationID), identifierIndexKey(identifierHash)).Result(); err != nil || count != 0 {
		t.Fatalf("unsafe metadata created Redis keys: count=%d, err=%v", count, err)
	}
	if _, err := NewRedisStore(nil, RedisOptions{}); err == nil {
		t.Fatal("NewRedisStore() accepted a nil client")
	}
	for _, options := range []RedisOptions{{TTL: -1}, {TTL: time.Microsecond}, {MaxPackageBytes: -1}} {
		if _, err := NewRedisStore(store.client, options); err == nil {
			t.Fatalf("NewRedisStore(%+v) unexpectedly succeeded", options)
		}
	}
}

func TestRedisPendingUsesNormalizedIdentifierIndex(t *testing.T) {
	server := miniredis.RunT(t)
	store, _ := testRedisStore(t, server, RedisOptions{})
	normalized := "Player@example.com"
	digest := sha256.Sum256([]byte(normalized))
	identifierHash := hex.EncodeToString(digest[:])
	if pending, err := store.Pending(context.Background(), "Player@Example.COM"); err != nil || pending {
		t.Fatalf("Pending() before staging = %v, %v", pending, err)
	}
	if _, _, err := store.Stage(context.Background(), testMigrationID, identifierHash, validDigestSnapshot(t)); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.Pending(context.Background(), " Player@Example.COM "); err != nil || !pending {
		t.Fatalf("Pending() after staging = %v, %v", pending, err)
	}
	if pending, err := store.Pending(context.Background(), "OtherPlayer"); err != nil || pending {
		t.Fatalf("Pending() for another identifier = %v, %v", pending, err)
	}
	record, err := store.Transition(context.Background(), testMigrationID, 1, StateApplying)
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.Transition(context.Background(), testMigrationID, record.Version, StateReconciling)
	if err != nil {
		t.Fatal(err)
	}
	record = confirmTestRollback(t, store, record)
	record, err = store.Transition(context.Background(), testMigrationID, record.Version, StateRolledBack)
	if err != nil {
		t.Fatal(err)
	}
	server.FastForward(25 * time.Hour)
	if _, err := store.Load(context.Background(), record.MigrationID); err != nil {
		t.Fatalf("rolled-back staging expired before discard: %v", err)
	}
	if _, err := store.Transition(context.Background(), testMigrationID, record.Version, StateDiscarded); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.Pending(context.Background(), normalized); err != nil || pending {
		t.Fatalf("Pending() after discard = %v, %v", pending, err)
	}
	server.FastForward(25 * time.Hour)
	if pending, err := store.Pending(context.Background(), normalized); err != nil || pending {
		t.Fatalf("Pending() after expiry = %v, %v", pending, err)
	}
}

func TestRedisPendingFailsClosedForCorruptIndex(t *testing.T) {
	server := miniredis.RunT(t)
	store, client := testRedisStore(t, server, RedisOptions{})
	normalized := "Player"
	digest := sha256.Sum256([]byte(normalized))
	identifierHash := hex.EncodeToString(digest[:])
	if err := client.Set(context.Background(), identifierIndexKey(identifierHash), testMigrationID, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.Pending(context.Background(), normalized); pending || !errors.Is(err, ErrNotFound) {
		t.Fatalf("Pending() with missing package = %v, %v", pending, err)
	}
	if err := client.Set(context.Background(), identifierIndexKey(identifierHash), "invalid", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.Pending(context.Background(), normalized); pending || !errors.Is(err, ErrCorruptStaging) {
		t.Fatalf("Pending() with invalid index = %v, %v", pending, err)
	}
}

func TestRedisListReturnsPackagesWithoutIdentifierIndexes(t *testing.T) {
	server := miniredis.RunT(t)
	store, _ := testRedisStore(t, server, RedisOptions{})
	secondID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	if _, _, err := store.Stage(context.Background(), testMigrationID, strings.Repeat("ab", 32), validDigestSnapshot(t)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Stage(context.Background(), secondID, strings.Repeat("cd", 32), validDigestSnapshot(t)); err != nil {
		t.Fatal(err)
	}
	records, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].MigrationID != testMigrationID || records[1].MigrationID != secondID {
		t.Fatalf("List() = %+v", records)
	}
	server.FastForward(25 * time.Hour)
	records, err = store.List(context.Background())
	if err != nil || len(records) != 0 {
		t.Fatalf("List() after expiry = %+v, %v", records, err)
	}
}

func TestRedisListFailsClosedForUnexpectedKeys(t *testing.T) {
	server := miniredis.RunT(t)
	store, client := testRedisStore(t, server, RedisOptions{})
	if err := client.Set(context.Background(), "scribe:migration:invalid", "corrupt", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(context.Background()); !errors.Is(err, ErrCorruptStaging) {
		t.Fatalf("List() error = %v", err)
	}
}

func TestRedisReconcileCanObserveAuthoritativeTerminalStates(t *testing.T) {
	server := miniredis.RunT(t)
	store, _ := testRedisStore(t, server, RedisOptions{})
	snapshot := validDigestSnapshot(t)
	identifierHash := strings.Repeat("ab", 32)
	if _, _, err := store.Stage(context.Background(), testMigrationID, identifierHash, snapshot); err != nil {
		t.Fatal(err)
	}
	record, err := store.Transition(context.Background(), testMigrationID, 1, StateApplying)
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.Transition(context.Background(), testMigrationID, record.Version, StateReconciling)
	if err != nil {
		t.Fatal(err)
	}
	record = confirmTestRollback(t, store, record)
	record, err = store.Transition(context.Background(), testMigrationID, record.Version, StateRolledBack)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), testMigrationID, record.Version, StateDiscarded); err != nil {
		t.Fatalf("reconciled discard transition failed: %v", err)
	}
}

func TestRedisDiscardRemovesPackageAndIdentifierIndexAtomically(t *testing.T) {
	server := miniredis.RunT(t)
	store, client := testRedisStore(t, server, RedisOptions{})
	identifierHash := strings.Repeat("ab", 32)
	if _, _, err := store.Stage(context.Background(), testMigrationID, identifierHash, validDigestSnapshot(t)); err != nil {
		t.Fatal(err)
	}
	if err := store.Discard(context.Background(), testMigrationID, 1); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Discard() before terminal state = %v", err)
	}
	record, err := store.Transition(context.Background(), testMigrationID, 1, StateApplying)
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.Transition(context.Background(), testMigrationID, record.Version, StateReconciling)
	if err != nil {
		t.Fatal(err)
	}
	record = confirmTestRollback(t, store, record)
	record, err = store.Transition(context.Background(), testMigrationID, record.Version, StateRolledBack)
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.Transition(context.Background(), testMigrationID, record.Version, StateDiscarded)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Discard(context.Background(), testMigrationID, record.Version-1); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("Discard() with stale version = %v", err)
	}
	if err := store.Discard(context.Background(), testMigrationID, record.Version); err != nil {
		t.Fatal(err)
	}
	if count, err := client.Exists(context.Background(), stagingKey(testMigrationID), identifierIndexKey(identifierHash)).Result(); err != nil || count != 0 {
		t.Fatalf("discarded keys remain: count=%d, err=%v", count, err)
	}
}

func TestRedisDiscardRejectsMismatchedIdentifierIndex(t *testing.T) {
	server := miniredis.RunT(t)
	store, client := testRedisStore(t, server, RedisOptions{})
	identifierHash := strings.Repeat("ab", 32)
	if _, _, err := store.Stage(context.Background(), testMigrationID, identifierHash, validDigestSnapshot(t)); err != nil {
		t.Fatal(err)
	}
	record, err := store.Transition(context.Background(), testMigrationID, 1, StateApplying)
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.Transition(context.Background(), testMigrationID, record.Version, StateReconciling)
	if err != nil {
		t.Fatal(err)
	}
	record = confirmTestRollback(t, store, record)
	record, err = store.Transition(context.Background(), testMigrationID, record.Version, StateRolledBack)
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.Transition(context.Background(), testMigrationID, record.Version, StateDiscarded)
	if err != nil {
		t.Fatal(err)
	}
	otherID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	if err := client.Set(context.Background(), identifierIndexKey(identifierHash), otherID, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := store.Discard(context.Background(), testMigrationID, record.Version); !errors.Is(err, ErrCorruptStaging) {
		t.Fatalf("Discard() with mismatched index = %v", err)
	}
	if exists, err := client.Exists(context.Background(), stagingKey(testMigrationID)).Result(); err != nil || exists != 1 {
		t.Fatalf("package was deleted despite corrupt index: exists=%d, err=%v", exists, err)
	}
}

func confirmTestRollback(t *testing.T, store *RedisStore, record StagedMigration) StagedMigration {
	t.Helper()
	confirmed, err := store.ConfirmRollback(context.Background(), record.MigrationID, record.Version, MigrationStatus{
		MigrationID: record.MigrationID,
		Status:      MigrationRolledBack,
	})
	if err != nil {
		t.Fatal(err)
	}
	return confirmed
}

func testRedisStore(t *testing.T, server *miniredis.Miniredis, options RedisOptions) (*RedisStore, *redis.Client) {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, err := NewRedisStore(client, options)
	if err != nil {
		t.Fatal(err)
	}
	return store, client
}
