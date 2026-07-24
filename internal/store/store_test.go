package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
	_ "modernc.org/sqlite"
)

func TestOpenMigratesM0DatabaseAndPreservesIndexStateAcrossScans(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "repokarta.db")
	legacy, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(schemaV1); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(
		"INSERT INTO repositories (name, path, discovered_at) VALUES (?, ?, ?)",
		"legacy",
		filepath.Join(t.TempDir(), "legacy"),
		time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	storage, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()

	repository := catalog.Repository{
		Name:            "repo",
		Path:            filepath.Join(t.TempDir(), "repo"),
		HeadCommit:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DefaultRevision: "main",
		ScanState:       "ready",
		DiscoveredAt:    time.Now(),
		ScannedAt:       time.Now(),
	}
	if err := storage.SyncRepositories(context.Background(), []catalog.Repository{repository}); err != nil {
		t.Fatal(err)
	}
	repositories, err := storage.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 1 || repositories[0].IndexState != "pending" {
		t.Fatalf("unexpected repositories after sync: %#v", repositories)
	}

	if err := storage.UpdateIndexState(
		context.Background(),
		repositories[0].ID,
		"ready",
		repository.HeadCommit,
		"",
	); err != nil {
		t.Fatal(err)
	}
	if err := storage.SyncRepositories(context.Background(), []catalog.Repository{repository}); err != nil {
		t.Fatal(err)
	}
	repositories, err = storage.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if repositories[0].IndexState != "ready" || repositories[0].IndexedCommit != repository.HeadCommit {
		t.Fatalf("expected ready index state to survive unchanged scan, got %#v", repositories[0])
	}

	repository.HeadCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := storage.SyncRepositories(context.Background(), []catalog.Repository{repository}); err != nil {
		t.Fatal(err)
	}
	repositories, err = storage.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if repositories[0].IndexState != "pending" {
		t.Fatalf("expected changed commit to become pending, got %#v", repositories[0])
	}
}
