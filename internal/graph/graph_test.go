package graph

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

type graphStore struct {
	repository catalog.Repository
}

func (s graphStore) ListRepositories(context.Context) ([]catalog.Repository, error) {
	return []catalog.Repository{s.repository}, nil
}

func (s graphStore) RepositoryByID(_ context.Context, id int64) (catalog.Repository, error) {
	if id == s.repository.ID {
		return s.repository, nil
	}
	return catalog.Repository{}, os.ErrNotExist
}

func TestSnapshotBuildsEvidenceBackedInventoryAndDependencyGraph(t *testing.T) {
	root := t.TempDir()
	writeGraphFixture(t, root, "go.mod", `module example.com/acme

go 1.26

require github.com/google/uuid v1.6.0
`)
	writeGraphFixture(t, root, "cmd/server/main.go", `package main

import (
	"net/http"
	"example.com/acme/internal/service"
)

func main() {
	http.HandleFunc("/healthz", func(http.ResponseWriter, *http.Request) {})
	service.Run()
}
`)
	writeGraphFixture(t, root, "internal/service/service.go", `package service

func Run() {}
`)
	writeGraphFixture(t, root, "web/package.json", `{
  "name": "@acme/web",
  "dependencies": {
    "htmx.org": "2.0.10"
  }
}
`)
	writeGraphFixture(t, root, "README.md", "# Acme\n")
	runGraphGit(t, root, "init")
	runGraphGit(t, root, "config", "user.email", "graph@example.com")
	runGraphGit(t, root, "config", "user.name", "Graph Test")
	runGraphGit(t, root, "add", ".")
	runGraphGit(t, root, "commit", "-m", "fixture")
	revision := strings.TrimSpace(runGraphGit(t, root, "rev-parse", "HEAD"))

	snapshotDirectory := filepath.Join(t.TempDir(), "maps")
	service, err := New(graphStore{repository: catalog.Repository{
		ID:            7,
		Name:          "acme",
		Path:          root,
		HeadCommit:    revision,
		IndexedCommit: revision,
		IndexState:    "ready",
	}}, snapshotDirectory, "http://127.0.0.1:7331")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.Snapshot(context.Background(), 7, false)
	if err != nil {
		t.Fatal(err)
	}

	if len(snapshot.Repositories) != 1 || snapshot.Repositories[0].Name != "acme" {
		t.Fatalf("repositories = %#v", snapshot.Repositories)
	}
	if snapshot.FileCount != 5 || len(snapshot.Languages) == 0 {
		t.Fatalf("inventory = files %d, languages %#v", snapshot.FileCount, snapshot.Languages)
	}
	if len(snapshot.Manifests) != 2 {
		t.Fatalf("manifests = %#v", snapshot.Manifests)
	}
	assertGraphNode(t, snapshot, "repository", "acme")
	assertGraphNode(t, snapshot, "package", "server")
	assertGraphNode(t, snapshot, "package", "service")
	assertGraphNode(t, snapshot, "dependency", "github.com/google/uuid")
	assertGraphNode(t, snapshot, "dependency", "htmx.org")
	assertGraphNode(t, snapshot, "route", "/healthz")
	assertGraphEdge(t, snapshot, "import", "imports")
	assertGraphEdge(t, snapshot, "route", "serves")

	for _, node := range snapshot.Nodes {
		if len(node.Evidence) == 0 || node.Evidence[0].Revision != revision || node.Evidence[0].URL == "" {
			t.Fatalf("node has incomplete evidence: %#v", node)
		}
	}
	for _, edge := range snapshot.Edges {
		if len(edge.Evidence) == 0 || edge.Evidence[0].Revision != revision || edge.Evidence[0].URL == "" {
			t.Fatalf("edge has incomplete evidence: %#v", edge)
		}
	}
	entries, err := os.ReadDir(snapshotDirectory)
	if err != nil || len(entries) != 1 || filepath.Ext(entries[0].Name()) != ".json" {
		t.Fatalf("snapshot files = %#v, err = %v", entries, err)
	}

	cached, err := service.Snapshot(context.Background(), 7, false)
	if err != nil {
		t.Fatal(err)
	}
	if cached.ID != snapshot.ID || !cached.GeneratedAt.Equal(snapshot.GeneratedAt) {
		t.Fatalf("snapshot was not loaded from cache: first %#v, cached %#v", snapshot.GeneratedAt, cached.GeneratedAt)
	}
}

func assertGraphNode(t *testing.T, snapshot Snapshot, kind, label string) {
	t.Helper()
	for _, node := range snapshot.Nodes {
		if node.Kind == kind && node.Label == label {
			return
		}
	}
	t.Fatalf("missing %s node %q in %#v", kind, label, snapshot.Nodes)
}

func assertGraphEdge(t *testing.T, snapshot Snapshot, kind, label string) {
	t.Helper()
	for _, edge := range snapshot.Edges {
		if edge.Kind == kind && edge.Label == label {
			return
		}
	}
	t.Fatalf("missing %s edge %q in %#v", kind, label, snapshot.Edges)
}

func writeGraphFixture(t *testing.T, root, name, content string) {
	t.Helper()
	filePath := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGraphGit(t *testing.T, root string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return string(output)
}
