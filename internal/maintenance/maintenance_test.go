package maintenance

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/catalog"
)

type testStore struct {
	repositories []catalog.Repository
	images       map[string]struct{}
}

func (s testStore) ListRepositories(context.Context) ([]catalog.Repository, error) {
	return append([]catalog.Repository(nil), s.repositories...), nil
}

func (s testStore) ConversationImagePaths(context.Context) (map[string]struct{}, error) {
	output := make(map[string]struct{}, len(s.images))
	for path := range s.images {
		output[path] = struct{}{}
	}
	return output, nil
}

func TestInventoryAndCleanupPreserveLiveState(t *testing.T) {
	service, dataDirectory := newTestService(t, testStore{
		repositories: []catalog.Repository{{ID: 7, Name: "payments", IndexedCommit: "abc123"}},
		images:       map[string]struct{}{"live.png": {}},
	})
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	writeOwnedFile(t, dataDirectory, "repokarta.db", "database")
	writeOwnedFile(t, dataDirectory, "security", "saml.key", "private-secret")
	writeOwnedFile(t, dataDirectory, "docs", "repository-7", "manifest.json", `{"version":3}`)
	oldMap := writeOwnedFile(t, dataDirectory, "maps", "repository-7-old.json", `{"version":4}`)
	newMap := writeOwnedFile(t, dataDirectory, "maps", "repository-7-new.json", `{"version":4}`)
	if err := os.Chtimes(oldMap, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newMap, now, now); err != nil {
		t.Fatal(err)
	}
	writeOwnedFile(t, dataDirectory, "conversations", "live.png", "live")
	orphan := writeOwnedFile(t, dataDirectory, "conversations", "orphan.png", "orphan")
	logFile := writeOwnedFile(t, dataDirectory, "logs", "repokarta.log", "log")
	temporary := writeOwnedFile(t, dataDirectory, "indexes", "shard.tmp", "partial")

	inventory, err := service.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cleanable := make(map[string]Item)
	protected := make(map[string]Item)
	for _, item := range inventory.Items {
		if item.Cleanable {
			cleanable[filepath.Base(item.RelativePath)] = item
		} else {
			protected[filepath.Base(item.RelativePath)] = item
		}
	}
	for _, expected := range []string{"repository-7-old.json", "orphan.png", "repokarta.log", "shard.tmp"} {
		if _, exists := cleanable[expected]; !exists {
			t.Fatalf("expected %s to be cleanable: %#v", expected, inventory.Items)
		}
	}
	for _, expected := range []string{"repokarta.db", "saml.key", "manifest.json", "repository-7-new.json", "live.png"} {
		if _, exists := protected[expected]; !exists {
			t.Fatalf("expected %s to be protected: %#v", expected, inventory.Items)
		}
	}

	targets := []string{
		cleanable["repository-7-old.json"].ID,
		cleanable["orphan.png"].ID,
		cleanable["repokarta.log"].ID,
		cleanable["shard.tmp"].ID,
	}
	plan, err := service.Plan(context.Background(), targets)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != len(targets) || plan.TotalBytes == 0 || plan.Token == "" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	result, err := service.Execute(context.Background(), targets, plan.Token)
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedItems != len(targets) {
		t.Fatalf("removed %d items, want %d", result.RemovedItems, len(targets))
	}
	for _, path := range []string{oldMap, orphan, logFile, temporary} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("cleaned path still exists: %s", path)
		}
	}
	for _, relative := range []string{
		"repokarta.db",
		filepath.Join("security", "saml.key"),
		filepath.Join("docs", "repository-7", "manifest.json"),
		filepath.Join("maps", "repository-7-new.json"),
		filepath.Join("conversations", "live.png"),
	} {
		if _, err := os.Stat(filepath.Join(dataDirectory, relative)); err != nil {
			t.Fatalf("protected path was removed: %s: %v", relative, err)
		}
	}
}

func TestCleanupRejectsChangedAndFabricatedPlans(t *testing.T) {
	service, dataDirectory := newTestService(t, testStore{})
	logFile := writeOwnedFile(t, dataDirectory, "logs", "repokarta.log", "first")
	inventory, err := service.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var target Item
	for _, item := range inventory.Items {
		if filepath.Base(item.RelativePath) == "repokarta.log" {
			target = item
		}
	}
	plan, err := service.Plan(context.Background(), []string{target.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logFile, []byte("changed after preview"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Execute(context.Background(), []string{target.ID}, plan.Token); err == nil ||
		!strings.Contains(err.Error(), "preview") {
		t.Fatalf("changed target error = %v", err)
	}
	if _, err := service.Plan(context.Background(), []string{"logs-fabricated"}); err == nil {
		t.Fatal("fabricated target was accepted")
	}
}

func TestDiagnosticsRedactSecretsAndOmitContent(t *testing.T) {
	service, dataDirectory := newTestService(t, testStore{
		repositories: []catalog.Repository{{
			ID:            12,
			Name:          "billing",
			IndexedCommit: "deadbeef",
			ScanState:     "error",
			IndexState:    "ready",
			ScanError:     `password=hunter2 at C:\repositories\billing and https://user:secret@example.com`,
		}},
	})
	writeOwnedFile(t, dataDirectory, "repokarta.db", "conversation prompt: do not include")
	writeOwnedFile(t, dataDirectory, "logs", "repokarta.log", "Bearer should-never-appear-token")
	writeOwnedFile(t, dataDirectory, "docs", "repository-12", "overview.md", "private source-derived text")

	content, name, err := service.Diagnostics(context.Background(), DiagnosticContext{
		AuthMode:  "saml",
		PublicURL: "https://repo.example.com",
		ProviderStatuses: []agent.Status{{
			ID: "claude", Name: "Claude Code", Available: true, Authenticated: true,
			Detail: "token=provider-secret Bearer abcdefghijklmnopqrstuvwxyz",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, "repokarta-diagnostics-") || !strings.HasSuffix(name, ".zip") {
		t.Fatalf("archive name = %q", name)
	}
	archive, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.File) != 2 {
		t.Fatalf("archive files = %d, want 2", len(archive.File))
	}
	var combined strings.Builder
	for _, file := range archive.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		combined.Write(data)
	}
	output := combined.String()
	for _, forbidden := range []string{
		"hunter2",
		"provider-secret",
		"abcdefghijklmnopqrst",
		"do not include",
		"private source-derived text",
		service.config.DataDirectory,
		service.config.RepositoryRoot,
	} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("diagnostics leaked %q: %s", forbidden, output)
		}
	}
	for _, expected := range []string{`"mode": "saml"`, `"database": 8`, `"maps": 4`, `"wiki": 3`, "redacted"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("diagnostics missing %q: %s", expected, output)
		}
	}
}

func TestNewRejectsOverlappingSourceAndDataRoots(t *testing.T) {
	root := t.TempDir()
	if _, err := New(Config{
		DataDirectory:  filepath.Join(root, "repositories", ".repokarta"),
		RepositoryRoot: filepath.Join(root, "repositories"),
	}, testStore{}); err == nil {
		t.Fatal("overlapping storage boundary was accepted")
	}
}

func newTestService(t *testing.T, storage testStore) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	dataDirectory := filepath.Join(root, "data")
	repositoryRoot := filepath.Join(root, "repositories")
	if err := os.MkdirAll(dataDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repositoryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	service, err := New(Config{
		DataDirectory:   dataDirectory,
		RepositoryRoot:  repositoryRoot,
		Version:         "0.38.0-dev",
		Address:         "127.0.0.1:7331",
		DatabaseVersion: 8,
		MapVersion:      4,
		WikiVersion:     3,
		Now: func() time.Time {
			return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
		},
	}, storage)
	if err != nil {
		t.Fatal(err)
	}
	return service, dataDirectory
}

func writeOwnedFile(t *testing.T, root string, parts ...string) string {
	t.Helper()
	content := parts[len(parts)-1]
	path := filepath.Join(append([]string{root}, parts[:len(parts)-1]...)...)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
