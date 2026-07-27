package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/scip-code/scip/bindings/go/scip"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/scipindex"
	"github.com/spolnik/RepoKarta/internal/store"
	"google.golang.org/protobuf/proto"
)

// TestVersionIsTheCurrentImplementationVersion keeps the reported version in
// step with the completed milestone recorded in SCOPE.md.
func TestVersionIsTheCurrentImplementationVersion(t *testing.T) {
	if version != "0.75.0-dev" {
		t.Fatalf("version = %q, want %q", version, "0.75.0-dev")
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+(-[a-z]+)?$`).MatchString(version) {
		t.Fatalf("version %q is not a semantic version", version)
	}
}

func TestSCIPImportBindsArtifactToIndexedCommit(t *testing.T) {
	dataDirectory := t.TempDir()
	database, err := store.Open(filepath.Join(dataDirectory, "repokarta.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SyncRepositories(context.Background(), []catalog.Repository{{
		Name:         "fixture",
		Path:         filepath.Join(t.TempDir(), "fixture"),
		HeadCommit:   "abc123",
		ScanState:    "ready",
		DiscoveredAt: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	repositories, err := database.ListRepositories(context.Background())
	if err != nil || len(repositories) != 1 {
		t.Fatalf("repositories = %#v, %v", repositories, err)
	}
	if err := database.UpdateIndexState(
		context.Background(),
		repositories[0].ID,
		"ready",
		"abc123",
		"",
	); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	input := &scip.Index{
		Metadata: &scip.Metadata{ToolInfo: &scip.ToolInfo{Name: "fixture-indexer"}},
		Documents: []*scip.Document{{
			RelativePath: "main.go",
			Language:     "go",
		}},
	}
	content, err := proto.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(t.TempDir(), "index.scip")
	if err := os.WriteFile(indexPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REPOKARTA_DEPENDENCY_REGISTRIES", "")
	if err := runSCIP([]string{
		"import",
		"-data-dir", dataDirectory,
		"-repository-id", "1",
		"-revision", "abc123",
		"-root", "backend",
		indexPath,
	}); err != nil {
		t.Fatal(err)
	}
	artifacts, err := scipindex.New(filepath.Join(dataDirectory, "scip"))
	if err != nil {
		t.Fatal(err)
	}
	artifact, ok, err := artifacts.Read(context.Background(), 1, "abc123")
	if err != nil || !ok || len(artifact.Documents) != 1 ||
		artifact.Documents[0].Path != "backend/main.go" {
		t.Fatalf("artifact = %#v, %v, %v", artifact, ok, err)
	}
	if err := runSCIP([]string{
		"import",
		"-data-dir", dataDirectory,
		"-repository-id", "1",
		"-revision", "different",
		"-root", "backend",
		indexPath,
	}); err == nil {
		t.Fatal("expected mismatched revision to be rejected")
	}
}
