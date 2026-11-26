package binding

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSQLiteBindingIsPermanentAndIdempotent(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	first, err := store.BindIfAbsent(ctx, "Player", Legacy, "migration-1")
	if err != nil {
		t.Fatalf("BindIfAbsent() error = %v", err)
	}
	second, err := store.BindIfAbsent(ctx, "Player", Legacy, "migration-1")
	if err != nil {
		t.Fatalf("BindIfAbsent() retry error = %v", err)
	}
	if first.Winner != Legacy || second.Winner != Legacy || !first.CreatedAt.Equal(second.CreatedAt) {
		t.Fatalf("idempotent bindings differ: first=%+v second=%+v", first, second)
	}
	if _, err := store.BindIfAbsent(ctx, "Player", Armorial, ""); !errors.Is(err, ErrBindingConflict) {
		t.Fatalf("winner switch error = %v, want ErrBindingConflict", err)
	}
	got, err := store.GetBinding(ctx, "Player")
	if err != nil || got.Winner != Legacy || got.MigrationID != "migration-1" {
		t.Fatalf("GetBinding() = %+v, %v", got, err)
	}
}

func TestSQLiteConcurrentBindingsHaveOneWinner(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	const attempts = 24
	results := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			winner := Legacy
			migrationID := "migration-legacy"
			if i%2 == 0 {
				winner = Armorial
				migrationID = ""
			}
			_, err := store.BindIfAbsent(ctx, "Player", winner, migrationID)
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	successes := 0
	conflicts := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrBindingConflict):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent binding error: %v", err)
		}
	}
	if successes == 0 || conflicts == 0 {
		t.Fatalf("successes=%d conflicts=%d, want both nonzero", successes, conflicts)
	}
	binding, err := store.GetBinding(ctx, "Player")
	if err != nil {
		t.Fatalf("GetBinding() error = %v", err)
	}
	if binding.Winner == Legacy && binding.MigrationID != "migration-legacy" {
		t.Fatalf("legacy binding has migration ID %q", binding.MigrationID)
	}
	if binding.Winner == Armorial && binding.MigrationID != "" {
		t.Fatalf("armorial binding has migration ID %q", binding.MigrationID)
	}
}

func TestSQLiteTokenBindingRoutesToSameWinner(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.BindIfAbsent(ctx, "Player", Armorial, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindToken(ctx, "access-token", "Player", Armorial); err != nil {
		t.Fatalf("BindToken() error = %v", err)
	}
	if err := store.BindToken(ctx, "access-token", "Player", Armorial); err != nil {
		t.Fatalf("BindToken() retry error = %v", err)
	}
	binding, err := store.GetBindingByToken(ctx, "access-token")
	if err != nil || binding.Winner != Armorial {
		t.Fatalf("GetBindingByToken() = %+v, %v", binding, err)
	}
	if err := store.BindToken(ctx, "access-token", "Player", Legacy); !errors.Is(err, ErrTokenConflict) {
		t.Fatalf("token winner switch error = %v, want ErrTokenConflict", err)
	}
	if _, err := store.GetBindingByToken(ctx, "unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token error = %v, want ErrNotFound", err)
	}
}

func TestSQLiteTokenBindingByHash(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	bound, err := store.BindIfAbsent(ctx, "Player", Armorial, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindTokenByHash(ctx, "refreshed-token", bound.IdentifierHash, Armorial); err != nil {
		t.Fatalf("BindTokenByHash() error = %v", err)
	}
	got, err := store.GetBindingByToken(ctx, "refreshed-token")
	if err != nil || got.IdentifierHash != bound.IdentifierHash || got.Winner != Armorial {
		t.Fatalf("GetBindingByToken() = %+v, %v", got, err)
	}
}

func TestSQLiteExpiredTokenBindingIsNotRoutableAndCanBePruned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scribe.db")
	store, err := OpenSQLite(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.BindIfAbsent(ctx, "Player", Armorial, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindToken(ctx, "expired-token", "Player", Armorial); err != nil {
		t.Fatal(err)
	}
	tokenSum := sha256.Sum256([]byte("expired-token"))
	expiredAt := time.Now().UTC().Add(-2 * time.Hour).Unix()
	if _, err := store.db.ExecContext(ctx, `UPDATE token_bindings SET created_at = ? WHERE token_hash = ?`, expiredAt, hex.EncodeToString(tokenSum[:])); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetBindingByToken(ctx, "expired-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired token error = %v, want ErrNotFound", err)
	}
	removed, err := store.PruneTokenBindings(ctx)
	if err != nil || removed != 1 {
		t.Fatalf("PruneTokenBindings() = %d, %v, want 1, nil", removed, err)
	}
	removed, err = store.PruneTokenBindings(ctx)
	if err != nil || removed != 0 {
		t.Fatalf("second PruneTokenBindings() = %d, %v, want 0, nil", removed, err)
	}
	if _, err := store.GetBinding(ctx, "Player"); err != nil {
		t.Fatalf("token pruning removed permanent binding: %v", err)
	}
}

func TestSQLitePrunesExpiredTokenBindingsOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scribe.db")
	store, err := OpenSQLite(path, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := store.BindIfAbsent(ctx, "Player", Armorial, ""); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"old-token", "fresh-token"} {
		if err := store.BindToken(ctx, token, "Player", Armorial); err != nil {
			t.Fatal(err)
		}
	}
	oldSum := sha256.Sum256([]byte("old-token"))
	if _, err := store.db.ExecContext(ctx, `UPDATE token_bindings SET created_at = ? WHERE token_hash = ?`, time.Now().UTC().Add(-2*time.Hour).Unix(), hex.EncodeToString(oldSum[:])); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLite(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.GetBindingByToken(ctx, "old-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old token error = %v, want ErrNotFound", err)
	}
	if _, err := reopened.GetBindingByToken(ctx, "fresh-token"); err != nil {
		t.Fatalf("fresh token was pruned: %v", err)
	}
	var count int
	if err := reopened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM token_bindings`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("token binding count = %d, want 1", count)
	}
}

func TestOpenSQLiteRejectsInvalidTokenBindingTTL(t *testing.T) {
	for _, ttl := range []time.Duration{0, 500 * time.Millisecond, -time.Second} {
		if store, err := OpenSQLite(filepath.Join(t.TempDir(), "scribe.db"), ttl); err == nil {
			_ = store.Close()
			t.Fatalf("OpenSQLite() accepted TTL %s", ttl)
		}
	}
}

func TestSQLiteBindingByHashSupportsCredentialFreeRecovery(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	identifierSum := sha256.Sum256([]byte("Player"))
	identifierHash := hex.EncodeToString(identifierSum[:])
	first, err := store.BindIfAbsentByHash(ctx, strings.ToUpper(identifierHash), Armorial, "migration-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.BindIfAbsentByHash(ctx, identifierHash, Armorial, "migration-1")
	if err != nil || !first.CreatedAt.Equal(second.CreatedAt) {
		t.Fatalf("idempotent hash binding = %+v, %v", second, err)
	}
	if _, err := store.BindIfAbsentByHash(ctx, identifierHash, Legacy, "other-migration"); !errors.Is(err, ErrBindingConflict) {
		t.Fatalf("hash winner switch error = %v", err)
	}
	if err := store.BindTokenByHash(ctx, "recovered-token", identifierHash, Armorial); err != nil {
		t.Fatal(err)
	}
	bound, err := store.GetBindingByToken(ctx, "recovered-token")
	if err != nil || bound.MigrationID != "migration-1" || bound.Winner != Armorial {
		t.Fatalf("recovered token binding = %+v, %v", bound, err)
	}
	plain, err := store.GetBinding(ctx, "Player")
	if err != nil || plain.IdentifierHash != identifierHash {
		t.Fatalf("plaintext lookup after recovery = %+v, %v", plain, err)
	}
}

func TestSQLiteRejectsInvalidIdentifierHashes(t *testing.T) {
	store := openTestStore(t)
	for _, hash := range []string{"", "short", strings.Repeat("z", 64)} {
		if _, err := store.BindIfAbsentByHash(context.Background(), hash, Armorial, "migration"); err == nil {
			t.Fatalf("BindIfAbsentByHash(%q) unexpectedly succeeded", hash)
		}
	}
}

func TestSQLiteStoresOnlyIdentifierAndTokenDigests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scribe.db")
	store, err := OpenSQLite(path, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.BindIfAbsent(ctx, "secret-identifier", Legacy, "migration-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindToken(ctx, "secret-token", "secret-identifier", Legacy); err != nil {
		t.Fatal(err)
	}

	identifierSum := sha256.Sum256([]byte("secret-identifier"))
	tokenSum := sha256.Sum256([]byte("secret-token"))
	var identifierHash, tokenHash string
	if err := store.db.QueryRowContext(ctx, `SELECT identifier_hash FROM bindings`).Scan(&identifierHash); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT token_hash FROM token_bindings`).Scan(&tokenHash); err != nil {
		t.Fatal(err)
	}
	if identifierHash != hex.EncodeToString(identifierSum[:]) || tokenHash != hex.EncodeToString(tokenSum[:]) {
		t.Fatalf("stored values are not SHA-256 digests: identifier=%q token=%q", identifierHash, tokenHash)
	}
}

func TestSQLiteBindingSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scribe.db")
	store, err := OpenSQLite(path, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindIfAbsent(context.Background(), "Player", Armorial, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLite(path, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	binding, err := reopened.GetBinding(context.Background(), "Player")
	if err != nil || binding.Winner != Armorial {
		t.Fatalf("binding after reopen = %+v, %v", binding, err)
	}
}

func TestSQLiteRejectsInvalidBindingValues(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.BindIfAbsent(context.Background(), "", Armorial, ""); err == nil {
		t.Fatal("empty identifier unexpectedly succeeded")
	}
	if _, err := store.BindIfAbsent(context.Background(), "Player", Winner("other"), ""); err == nil {
		t.Fatal("invalid winner unexpectedly succeeded")
	}
	if err := store.BindToken(context.Background(), "", "Player", Armorial); err == nil {
		t.Fatal("empty token unexpectedly succeeded")
	}
}

func TestSQLiteMissingBinding(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.GetBinding(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetBinding() error = %v, want ErrNotFound", err)
	}
}

func openTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "scribe.db"), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
			t.Errorf("Close() error: %v", err)
		}
	})
	return store
}
