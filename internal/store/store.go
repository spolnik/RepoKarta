package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
	_ "modernc.org/sqlite"
)

const (
	currentSchemaVersion = 2

	schemaV1 = `
CREATE TABLE IF NOT EXISTS repositories (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    path TEXT NOT NULL UNIQUE,
    discovered_at TEXT NOT NULL
);`

	schemaV2 = `
ALTER TABLE repositories ADD COLUMN origin_url TEXT NOT NULL DEFAULT '';
ALTER TABLE repositories ADD COLUMN default_revision TEXT NOT NULL DEFAULT '';
ALTER TABLE repositories ADD COLUMN head_commit TEXT NOT NULL DEFAULT '';
ALTER TABLE repositories ADD COLUMN bare INTEGER NOT NULL DEFAULT 0;
ALTER TABLE repositories ADD COLUMN scan_state TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE repositories ADD COLUMN scan_error TEXT NOT NULL DEFAULT '';
ALTER TABLE repositories ADD COLUMN scanned_at TEXT NOT NULL DEFAULT '';
ALTER TABLE repositories ADD COLUMN index_state TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE repositories ADD COLUMN index_error TEXT NOT NULL DEFAULT '';
ALTER TABLE repositories ADD COLUMN indexed_commit TEXT NOT NULL DEFAULT '';
ALTER TABLE repositories ADD COLUMN indexed_at TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS repositories_name_index ON repositories(name COLLATE NOCASE);
CREATE INDEX IF NOT EXISTS repositories_index_state_index ON repositories(index_state);`
)

// Store persists RepoKarta-owned metadata. Repository source remains read-only.
type Store struct {
	db *sql.DB
}

// Open opens the SQLite database, enables WAL, and applies migrations.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec("PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure SQLite database: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > currentSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, currentSchemaVersion)
	}

	for version < currentSchemaVersion {
		next := version + 1
		var migration string
		switch next {
		case 1:
			migration = schemaV1
		case 2:
			migration = schemaV2
		default:
			return fmt.Errorf("missing migration for schema version %d", next)
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin schema migration %d: %w", next, err)
		}
		if _, err := tx.Exec(migration); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply schema migration %d: %w", next, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", next)); err != nil {
			tx.Rollback()
			return fmt.Errorf("record schema migration %d: %w", next, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit schema migration %d: %w", next, err)
		}
		version = next
	}
	return nil
}

// Close closes the metadata database.
func (s *Store) Close() error {
	return s.db.Close()
}

// SyncRepositories updates discovered repositories without discarding existing
// indexing state. Repositories no longer below the configured root are removed.
func (s *Store) SyncRepositories(ctx context.Context, repositories []catalog.Repository) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	seen := make([]string, 0, len(repositories))
	for _, repository := range repositories {
		discoveredAt := formatTime(repository.DiscoveredAt)
		scannedAt := formatTime(repository.ScannedAt)
		if _, err := tx.ExecContext(ctx, `
INSERT INTO repositories (
    name, path, origin_url, default_revision, head_commit, bare,
    scan_state, scan_error, discovered_at, scanned_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(path) DO UPDATE SET
    name = excluded.name,
    origin_url = excluded.origin_url,
    default_revision = excluded.default_revision,
    head_commit = excluded.head_commit,
    bare = excluded.bare,
    scan_state = excluded.scan_state,
    scan_error = excluded.scan_error,
    scanned_at = excluded.scanned_at,
    index_state = CASE
        WHEN repositories.indexed_commit = excluded.head_commit AND repositories.index_state = 'ready'
        THEN repositories.index_state
        ELSE 'pending'
    END,
    index_error = CASE
        WHEN repositories.indexed_commit = excluded.head_commit AND repositories.index_state = 'ready'
        THEN repositories.index_error
        ELSE ''
    END`,
			repository.Name,
			repository.Path,
			repository.OriginURL,
			repository.DefaultRevision,
			repository.HeadCommit,
			repository.Bare,
			repository.ScanState,
			repository.ScanError,
			discoveredAt,
			scannedAt,
		); err != nil {
			return fmt.Errorf("sync repository %q: %w", repository.Path, err)
		}
		seen = append(seen, repository.Path)
	}

	if len(seen) == 0 {
		if _, err := tx.ExecContext(ctx, "DELETE FROM repositories"); err != nil {
			return err
		}
	} else {
		placeholders := "?"
		arguments := make([]any, len(seen))
		for index, path := range seen {
			arguments[index] = path
			if index > 0 {
				placeholders += ",?"
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM repositories WHERE path NOT IN ("+placeholders+")", arguments...); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// ReplaceRepositories is retained for compatibility with the M0 API.
func (s *Store) ReplaceRepositories(ctx context.Context, repositories []catalog.Repository) error {
	return s.SyncRepositories(ctx, repositories)
}

// ListRepositories returns the repository catalogue in display order.
func (s *Store) ListRepositories(ctx context.Context) ([]catalog.Repository, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT
    id, name, path, origin_url, default_revision, head_commit, bare,
    scan_state, scan_error, index_state, index_error, indexed_commit,
    discovered_at, scanned_at, indexed_at
FROM repositories
ORDER BY name COLLATE NOCASE, path COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var repositories []catalog.Repository
	for rows.Next() {
		repository, err := scanRepository(rows)
		if err != nil {
			return nil, err
		}
		repositories = append(repositories, repository)
	}
	return repositories, rows.Err()
}

// RepositoryByID returns one repository or sql.ErrNoRows.
func (s *Store) RepositoryByID(ctx context.Context, id int64) (catalog.Repository, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT
    id, name, path, origin_url, default_revision, head_commit, bare,
    scan_state, scan_error, index_state, index_error, indexed_commit,
    discovered_at, scanned_at, indexed_at
FROM repositories
WHERE id = ?`, id)
	return scanRepository(row)
}

// UpdateIndexState records an indexing transition.
func (s *Store) UpdateIndexState(ctx context.Context, id int64, state, indexedCommit, indexError string) error {
	indexedAt := ""
	if state == "ready" {
		indexedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE repositories
SET index_state = ?, indexed_commit = ?, index_error = ?, indexed_at = ?
WHERE id = ?`,
		state,
		indexedCommit,
		indexError,
		indexedAt,
		id,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanRepository(row rowScanner) (catalog.Repository, error) {
	var repository catalog.Repository
	var discoveredAt, scannedAt, indexedAt string
	if err := row.Scan(
		&repository.ID,
		&repository.Name,
		&repository.Path,
		&repository.OriginURL,
		&repository.DefaultRevision,
		&repository.HeadCommit,
		&repository.Bare,
		&repository.ScanState,
		&repository.ScanError,
		&repository.IndexState,
		&repository.IndexError,
		&repository.IndexedCommit,
		&discoveredAt,
		&scannedAt,
		&indexedAt,
	); err != nil {
		return catalog.Repository{}, err
	}
	repository.DiscoveredAt = parseTime(discoveredAt)
	repository.ScannedAt = parseTime(scannedAt)
	repository.IndexedAt = parseTime(indexedAt)
	return repository, nil
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}
	}
	return parsed
}
