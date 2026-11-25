package binding

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sync"
	"testing"
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

func TestSQLiteStoresOnlyIdentifierAndTokenDigests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scribe.db")
	store, err := OpenSQLite(path)
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
	store, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindIfAbsent(context.Background(), "Player", Armorial, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLite(path)
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
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "scribe.db"))
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
