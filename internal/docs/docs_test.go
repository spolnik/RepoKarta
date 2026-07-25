package docs

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/graph"
)

type memoryStorage struct {
	mu         sync.Mutex
	repository catalog.Repository
	pages      map[string]Page
}

func (s *memoryStorage) RepositoryByID(_ context.Context, id int64) (catalog.Repository, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.repository.ID {
		return catalog.Repository{}, fmt.Errorf("repository %d not found", id)
	}
	return s.repository, nil
}

func (s *memoryStorage) ListRepositories(context.Context) ([]catalog.Repository, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []catalog.Repository{s.repository}, nil
}

func (s *memoryStorage) ListDocumentPages(_ context.Context, repositoryID int64) ([]Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pages []Page
	for _, page := range s.pages {
		if page.RepositoryID == repositoryID {
			pages = append(pages, clonePage(page))
		}
	}
	return pages, nil
}

func (s *memoryStorage) SaveDocumentPage(_ context.Context, page Page) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pages[page.Slug] = clonePage(page)
	return nil
}

func clonePage(page Page) Page {
	page.SupportingFiles = slices.Clone(page.SupportingFiles)
	page.Citations = slices.Clone(page.Citations)
	return page
}

func TestGenerationResumeSelectiveStalenessAndExport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repositoryPath := initializeDocumentationRepository(t)
	firstRevision := commitAll(t, repositoryPath, "initial")
	storage := &memoryStorage{
		repository: catalog.Repository{
			ID:            1,
			Name:          "fixture",
			Path:          repositoryPath,
			HeadCommit:    firstRevision,
			IndexedCommit: firstRevision,
		},
		pages: make(map[string]Page),
	}
	maps, err := graph.New(storage, filepath.Join(t.TempDir(), "maps"), "http://127.0.0.1:7331")
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(storage, maps, filepath.Join(t.TempDir(), "docs"))
	if err != nil {
		t.Fatal(err)
	}

	plan, err := service.Plan(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Pages) != 3 || plan.Pending != 3 {
		t.Fatalf("unexpected initial plan: %+v", plan)
	}
	if plan.Steering.Title != "Fixture handbook" {
		t.Fatalf("steering title = %q", plan.Steering.Title)
	}

	generated, err := service.Generate(ctx, GenerateRequest{RepositoryID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if generated.Ready != 3 || generated.Pending != 0 || generated.Failed != 0 {
		t.Fatalf("unexpected generated site: %+v", generated)
	}
	for _, page := range generated.Pages {
		if page.Revision != firstRevision || page.Provider != deterministicProvider || page.Model != deterministicModel {
			t.Fatalf("missing generation provenance: %+v", page)
		}
		if len(page.Citations) == 0 || len(page.SupportingFiles) == 0 {
			t.Fatalf("page is not grounded: %+v", page)
		}
		if err := validateGeneratedPage(page, firstRevision); err != nil {
			t.Fatalf("validate %s: %v", page.Slug, err)
		}
	}

	architecture := pageBySlug(t, generated, "architecture")
	architecture.Status = StatusGenerating
	if err := storage.SaveDocumentPage(ctx, architecture); err != nil {
		t.Fatal(err)
	}
	recovered, err := service.Plan(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if pageBySlug(t, recovered, "architecture").Status != StatusError {
		t.Fatalf("interrupted page was not made retryable: %+v", recovered.Pages)
	}
	recovered, err = service.Generate(ctx, GenerateRequest{RepositoryID: 1, Page: "architecture"})
	if err != nil {
		t.Fatal(err)
	}
	architecture = pageBySlug(t, recovered, "architecture")
	if architecture.Status != StatusReady {
		t.Fatalf("targeted retry did not recover page: %+v", architecture)
	}
	architectureGeneratedAt := architecture.GeneratedAt

	writeFile(t, filepath.Join(repositoryPath, "NOTES.txt"), "unrelated\n")
	secondRevision := commitAll(t, repositoryPath, "unrelated")
	updateRevision(storage, secondRevision)
	unchanged, err := service.Plan(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Stale != 0 {
		t.Fatalf("unrelated file made pages stale: %+v", unchanged.Pages)
	}

	writeFile(t, filepath.Join(repositoryPath, "go.mod"), "module example.com/fixture\n\ngo 1.26\n\nrequire example.com/new v1.0.0\n")
	thirdRevision := commitAll(t, repositoryPath, "dependency")
	updateRevision(storage, thirdRevision)
	staleSite, err := service.Plan(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if pageBySlug(t, staleSite, "overview").Status != StatusStale {
		t.Fatalf("overview did not become stale: %+v", staleSite.Pages)
	}
	if pageBySlug(t, staleSite, "dependencies").Status != StatusStale {
		t.Fatalf("dependencies did not become stale: %+v", staleSite.Pages)
	}
	if pageBySlug(t, staleSite, "architecture").Status != StatusReady {
		t.Fatalf("unaffected architecture page became stale: %+v", staleSite.Pages)
	}

	refreshed, err := service.Generate(ctx, GenerateRequest{RepositoryID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Stale != 0 || refreshed.Ready != 3 {
		t.Fatalf("selective refresh did not complete: %+v", refreshed)
	}
	refreshedArchitecture := pageBySlug(t, refreshed, "architecture")
	if !refreshedArchitecture.GeneratedAt.Equal(architectureGeneratedAt) ||
		refreshedArchitecture.Revision != firstRevision {
		t.Fatalf("unaffected page was regenerated: before=%+v after=%+v", architecture, refreshedArchitecture)
	}
	if pageBySlug(t, refreshed, "dependencies").Revision != thirdRevision {
		t.Fatalf("affected page was not refreshed to %s", thirdRevision)
	}

	content, fileName, err := service.Export(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if fileName != "repokarta-wiki-fixture.zip" {
		t.Fatalf("export filename = %q", fileName)
	}
	archive, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, file := range archive.File {
		names = append(names, file.Name)
	}
	for _, expected := range []string{"README.md", "architecture.md", "dependencies.md", "overview.md", "repokarta-manifest.json"} {
		if !slices.Contains(names, expected) {
			t.Fatalf("export missing %s: %v", expected, names)
		}
	}
}

func TestSteeringRejectsUnknownFieldsAndUnsafeMermaid(t *testing.T) {
	t.Parallel()
	repositoryPath := t.TempDir()
	runGitTest(t, repositoryPath, "init")
	runGitTest(t, repositoryPath, "config", "user.email", "test@example.com")
	runGitTest(t, repositoryPath, "config", "user.name", "RepoKarta Test")
	writeFile(t, filepath.Join(repositoryPath, ".repokarta.yml"), "docs:\n  surprise: true\n")
	revision := commitAll(t, repositoryPath, "invalid steering")
	_, _, err := loadSteering(context.Background(), catalog.Repository{Path: repositoryPath}, revision)
	if err == nil || !strings.Contains(err.Error(), "field surprise") {
		t.Fatalf("unknown steering field was accepted: %v", err)
	}

	for _, markdown := range []string{
		"```mermaid\nflowchart LR\n  A --> B\n",
		"```mermaid\nflowchart LR\n  click A \"javascript:alert(1)\"\n```",
		"```mermaid\nunknownDiagram\n  A --> B\n```",
	} {
		if err := validateMermaid(markdown); err == nil {
			t.Fatalf("unsafe or invalid Mermaid was accepted: %q", markdown)
		}
	}
	if err := validateMermaid("```mermaid\nflowchart LR\n  A --> B\n```"); err != nil {
		t.Fatalf("valid Mermaid rejected: %v", err)
	}
}

func initializeDocumentationRepository(t *testing.T) string {
	t.Helper()
	repositoryPath := t.TempDir()
	runGitTest(t, repositoryPath, "init")
	runGitTest(t, repositoryPath, "config", "user.email", "test@example.com")
	runGitTest(t, repositoryPath, "config", "user.name", "RepoKarta Test")
	writeFile(t, filepath.Join(repositoryPath, ".repokarta.yml"), "docs:\n  title: Fixture handbook\n  notes:\n    overview: Focus on committed structure.\n")
	writeFile(t, filepath.Join(repositoryPath, "README.md"), "# Fixture\n")
	writeFile(t, filepath.Join(repositoryPath, "go.mod"), "module example.com/fixture\n\ngo 1.26\n")
	writeFile(t, filepath.Join(repositoryPath, "cmd", "fixture", "main.go"), "package main\n\nimport \"example.com/fixture/internal/server\"\n\nfunc main() { server.Start() }\n")
	writeFile(t, filepath.Join(repositoryPath, "internal", "server", "server.go"), "package server\n\nimport \"net/http\"\n\nfunc Start() { http.HandleFunc(\"/healthz\", func(http.ResponseWriter, *http.Request) {}) }\n")
	return repositoryPath
}

func commitAll(t *testing.T, repositoryPath, message string) string {
	t.Helper()
	runGitTest(t, repositoryPath, "add", ".")
	runGitTest(t, repositoryPath, "commit", "-m", message)
	return strings.TrimSpace(runGitTest(t, repositoryPath, "rev-parse", "HEAD"))
}

func updateRevision(storage *memoryStorage, revision string) {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	storage.repository.HeadCommit = revision
	storage.repository.IndexedCommit = revision
}

func pageBySlug(t *testing.T, site Site, slug string) Page {
	t.Helper()
	for _, page := range site.Pages {
		if page.Slug == slug {
			return page
		}
	}
	t.Fatalf("page %q not found in %+v", slug, site.Pages)
	return Page{}
}

func writeFile(t *testing.T, filePath, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGitTest(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", append([]string{"-C", directory}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}
