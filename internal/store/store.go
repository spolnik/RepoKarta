package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS repositories (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    path TEXT NOT NULL UNIQUE,
    discovered_at TEXT NOT NULL
);
`

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize SQLite database: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) ReplaceRepositories(ctx context.Context, repositories []catalog.Repository) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "DELETE FROM repositories"); err != nil {
		return err
	}

	discoveredAt := time.Now().UTC().Format(time.RFC3339)
	for _, repository := range repositories {
		if _, err := tx.ExecContext(
			ctx,
			"INSERT INTO repositories (name, path, discovered_at) VALUES (?, ?, ?)",
			repository.Name,
			repository.Path,
			discoveredAt,
		); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) ListRepositories(ctx context.Context) ([]catalog.Repository, error) {
	rows, err := s.db.QueryContext(
		ctx,
		"SELECT name, path FROM repositories ORDER BY name COLLATE NOCASE, path COLLATE NOCASE",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var repositories []catalog.Repository
	for rows.Next() {
		var repository catalog.Repository
		if err := rows.Scan(&repository.Name, &repository.Path); err != nil {
			return nil, err
		}
		repositories = append(repositories, repository)
	}

	return repositories, rows.Err()
}
