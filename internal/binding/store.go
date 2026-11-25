package binding

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Winner string

const (
	Legacy   Winner = "legacy"
	Armorial Winner = "armorial"
)

var (
	ErrNotFound        = errors.New("binding not found")
	ErrBindingConflict = errors.New("binding conflict")
	ErrTokenConflict   = errors.New("token binding conflict")
)

type Binding struct {
	IdentifierHash string
	Winner         Winner
	MigrationID    string
	CreatedAt      time.Time
}

type Store interface {
	GetBinding(ctx context.Context, identifier string) (Binding, error)
	BindIfAbsent(ctx context.Context, identifier string, winner Winner, migrationID string) (Binding, error)
	GetBindingByToken(ctx context.Context, accessToken string) (Binding, error)
	BindToken(ctx context.Context, accessToken, identifier string, winner Winner) error
	BindTokenByHash(ctx context.Context, accessToken, identifierHash string, winner Winner) error
	Close() error
}

type SQLiteStore struct {
	db *sql.DB
}

func OpenSQLite(path string) (*SQLiteStore, error) {
	if path == "" {
		return nil, errors.New("sqlite path must not be empty")
	}
	if path != ":memory:" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve sqlite path: %w", err)
		}
		if err := ensureParent(absolute); err != nil {
			return nil, err
		}
		path = absolute
	}
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(FULL)",
	}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(4)
		db.SetMaxIdleConns(4)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	store := &SQLiteStore{db: db}
	if err := store.initSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func ensureParent(path string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return fmt.Errorf("create sqlite directory: %w", err)
	}
	return nil
}

func (s *SQLiteStore) initSchema() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS bindings (
    identifier_hash TEXT PRIMARY KEY,
    winner TEXT NOT NULL CHECK (winner IN ('legacy', 'armorial')),
    migration_id TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS token_bindings (
    token_hash TEXT PRIMARY KEY,
    identifier_hash TEXT NOT NULL REFERENCES bindings(identifier_hash) ON DELETE CASCADE,
    winner TEXT NOT NULL CHECK (winner IN ('legacy', 'armorial')),
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS token_bindings_identifier_idx ON token_bindings(identifier_hash);
`)
	if err != nil {
		return fmt.Errorf("initialize binding schema: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetBinding(ctx context.Context, identifier string) (Binding, error) {
	hash, err := digest(identifier)
	if err != nil {
		return Binding{}, err
	}
	return s.getBindingByHash(ctx, hash)
}

func (s *SQLiteStore) BindIfAbsent(ctx context.Context, identifier string, winner Winner, migrationID string) (Binding, error) {
	hash, err := digest(identifier)
	if err != nil {
		return Binding{}, err
	}
	if !validWinner(winner) {
		return Binding{}, errors.New("invalid binding winner")
	}
	now := time.Now().UTC().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Binding{}, fmt.Errorf("begin binding transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
INSERT INTO bindings (identifier_hash, winner, migration_id, created_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(identifier_hash) DO NOTHING`, hash, winner, migrationID, now)
	if err != nil {
		return Binding{}, fmt.Errorf("insert binding: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return Binding{}, fmt.Errorf("inspect binding insert: %w", err)
	}
	if inserted == 1 {
		if err := tx.Commit(); err != nil {
			return Binding{}, fmt.Errorf("commit binding: %w", err)
		}
		return Binding{IdentifierHash: hash, Winner: winner, MigrationID: migrationID, CreatedAt: time.Unix(now, 0).UTC()}, nil
	}
	existing, err := scanBinding(tx.QueryRowContext(ctx, `SELECT identifier_hash, winner, migration_id, created_at FROM bindings WHERE identifier_hash = ?`, hash))
	if err != nil {
		return Binding{}, err
	}
	if existing.Winner != winner || existing.MigrationID != migrationID {
		return Binding{}, fmt.Errorf("%w for identifier", ErrBindingConflict)
	}
	if err := tx.Commit(); err != nil {
		return Binding{}, fmt.Errorf("commit binding: %w", err)
	}
	return existing, nil
}

func (s *SQLiteStore) GetBindingByToken(ctx context.Context, accessToken string) (Binding, error) {
	hash, err := digest(accessToken)
	if err != nil {
		return Binding{}, err
	}
	row := s.db.QueryRowContext(ctx, `
SELECT b.identifier_hash, b.winner, b.migration_id, b.created_at
FROM token_bindings t
JOIN bindings b ON b.identifier_hash = t.identifier_hash
WHERE t.token_hash = ? AND t.winner = b.winner`, hash)
	binding, err := scanBinding(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Binding{}, ErrNotFound
	}
	return binding, err
}

func (s *SQLiteStore) BindToken(ctx context.Context, accessToken, identifier string, winner Winner) error {
	identifierHash, err := digest(identifier)
	if err != nil {
		return err
	}
	return s.BindTokenByHash(ctx, accessToken, identifierHash, winner)
}

func (s *SQLiteStore) BindTokenByHash(ctx context.Context, accessToken, identifierHash string, winner Winner) error {
	tokenHash, err := digest(accessToken)
	if err != nil {
		return err
	}
	if identifierHash == "" {
		return errors.New("identifier hash must not be empty")
	}
	if !validWinner(winner) {
		return errors.New("invalid token binding winner")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin token binding transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
INSERT INTO token_bindings (token_hash, identifier_hash, winner, created_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(token_hash) DO NOTHING`, tokenHash, identifierHash, winner, time.Now().UTC().Unix())
	if err != nil {
		return fmt.Errorf("insert token binding: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect token binding insert: %w", err)
	}
	if inserted == 0 {
		var existingIdentifier, existingWinner string
		err := tx.QueryRowContext(ctx, `SELECT identifier_hash, winner FROM token_bindings WHERE token_hash = ?`, tokenHash).Scan(&existingIdentifier, &existingWinner)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: token disappeared during insert", ErrTokenConflict)
		}
		if err != nil {
			return fmt.Errorf("read token binding: %w", err)
		}
		if existingIdentifier != identifierHash || existingWinner != string(winner) {
			return fmt.Errorf("%w for token", ErrTokenConflict)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit token binding: %w", err)
	}
	return nil
}

func (s *SQLiteStore) getBindingByHash(ctx context.Context, hash string) (Binding, error) {
	binding, err := scanBinding(s.db.QueryRowContext(ctx, `SELECT identifier_hash, winner, migration_id, created_at FROM bindings WHERE identifier_hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return Binding{}, ErrNotFound
	}
	return binding, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanBinding(row rowScanner) (Binding, error) {
	var binding Binding
	var createdAt int64
	if err := row.Scan(&binding.IdentifierHash, &binding.Winner, &binding.MigrationID, &createdAt); err != nil {
		return Binding{}, err
	}
	binding.CreatedAt = time.Unix(createdAt, 0).UTC()
	return binding, nil
}

func digest(value string) (string, error) {
	if value == "" {
		return "", errors.New("binding value must not be empty")
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:]), nil
}

func validWinner(winner Winner) bool {
	return winner == Legacy || winner == Armorial
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}
