package docs

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/graph"
)

type memoryStorage struct {
	mu         sync.Mutex
	repository catalog.Repository
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

type fakeKnowledgeGenerator struct {
	results  []agent.EphemeralResult
	requests []agent.TurnRequest
}

func (g *fakeKnowledgeGenerator) RunEphemeral(
	_ context.Context,
	request agent.TurnRequest,
	_ func(agent.Event) error,
) (agent.EphemeralResult, error) {
	g.requests = append(g.requests, request)
	if len(g.results) == 0 {
		return agent.EphemeralResult{}, errors.New("no fake knowledge result")
	}
	result := g.results[0]
	g.results = g.results[1:]
	return result, nil
}

func TestKnowledgePresetRequiresCuratedModelAndHighEffort(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		request GenerateRequest
		wantErr bool
	}{
		{
			name: "codex flagship high",
			request: GenerateRequest{
				Provider: "codex",
				Model:    "gpt-5.6-sol",
				Effort:   "high",
			},
		},
		{
			name: "codex terra max",
			request: GenerateRequest{
				Provider: "codex",
				Model:    "gpt-5.6-terra",
				Effort:   "max",
			},
		},
		{
			name: "claude opus high",
			request: GenerateRequest{
				Provider: "anthropic-api",
				Model:    "claude-opus-5",
				Effort:   "high",
			},
		},
		{
			name: "provider default rejected",
			request: GenerateRequest{
				Provider: "codex",
				Effort:   "high",
			},
			wantErr: true,
		},
		{
			name: "medium rejected",
			request: GenerateRequest{
				Provider: "codex",
				Model:    "gpt-5.6-sol",
				Effort:   "medium",
			},
			wantErr: true,
		},
		{
			name: "balanced Claude rejected",
			request: GenerateRequest{
				Provider: "anthropic-api",
				Model:    "claude-sonnet-5",
				Effort:   "high",
			},
			wantErr: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateKnowledgePreset(test.request)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateKnowledgePreset() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestProviderGroundedKnowledgePlanAndPage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repositoryPath := initializeDocumentationRepository(t)
	revision := commitAll(t, repositoryPath, "deep wiki fixture")
	storage := &memoryStorage{
		repository: catalog.Repository{
			ID:            1,
			Name:          "fixture",
			Path:          repositoryPath,
			HeadCommit:    revision,
			IndexedCommit: revision,
		},
	}
	maps, err := graph.New(storage, filepath.Join(t.TempDir(), "maps"), "http://127.0.0.1:7331")
	if err != nil {
		t.Fatal(err)
	}
	markdown, sources := fakeKnowledgeMarkdown(revision)
	surveyMarkdown, surveySources := fakeKnowledgeSurvey(revision)
	generator := &fakeKnowledgeGenerator{results: []agent.EphemeralResult{
		{
			Provider:     "codex",
			Model:        "gpt-5.6-sol",
			Text:         surveyMarkdown,
			Sources:      surveySources,
			InputTokens:  1800,
			OutputTokens: 1400,
		},
		{
			Provider: "codex",
			Model:    "gpt-5.6-sol",
			Text: `<repokarta_wiki_plan>
{"pages":[
{"slug":"architecture-overview","title":"Architecture Overview","summary":"Explain the runtime composition, process boundaries, central services, and end-to-end request paths.","number":"1","parent_slug":"","depth":0},
{"slug":"runtime-lifecycle","title":"Runtime Lifecycle","summary":"Trace startup, dependency construction, request serving, shutdown, and the failure paths around each stage.","number":"1.1","parent_slug":"architecture-overview","depth":1},
{"slug":"search-and-source-intelligence","title":"Search and Source Intelligence","summary":"Explain indexing, query coordination, source retrieval, symbol lookup, and commit-pinned citation behavior.","number":"2","parent_slug":"","depth":0},
{"slug":"search-indexing-pipeline","title":"Search Indexing Pipeline","summary":"Trace repository discovery through incremental indexing, query execution, result limits, and recovery behavior.","number":"2.1","parent_slug":"search-and-source-intelligence","depth":1},
{"slug":"operations-and-testing","title":"Operations and Testing","summary":"Explain configuration, build and packaging paths, runtime diagnostics, test layers, and operational invariants.","number":"3","parent_slug":"","depth":0},
{"slug":"glossary","title":"Glossary","summary":"Define repository-specific services, storage concepts, provider terms, graph facts, and citation terminology.","number":"4","parent_slug":"","depth":0}
]}
</repokarta_wiki_plan>`,
		},
		{
			Provider:     "codex",
			Model:        "gpt-5.6-sol",
			Text:         markdown,
			Sources:      sources,
			InputTokens:  1200,
			OutputTokens: 900,
		},
	}}
	docsDirectory := filepath.Join(t.TempDir(), "docs")
	service, err := New(storage, maps, docsDirectory)
	if err != nil {
		t.Fatal(err)
	}
	service.UseGenerator(generator)

	bootstrap, err := service.Plan(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.PlanReady || len(bootstrap.Pages) != 1 || bootstrap.Pages[0].Slug != "architecture-overview" {
		t.Fatalf("bootstrap plan = %+v", bootstrap)
	}

	surveyed, err := service.Generate(ctx, GenerateRequest{
		RepositoryID: 1,
		Provider:     "codex",
		Model:        "gpt-5.6-sol",
		Effort:       "high",
		SurveyOnly:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !surveyed.SurveyReady || surveyed.PlanReady {
		t.Fatalf("repository survey checkpoint = %+v", surveyed)
	}

	planned, err := service.Generate(ctx, GenerateRequest{
		RepositoryID: 1,
		Provider:     "codex",
		Model:        "gpt-5.6-sol",
		Effort:       "high",
		PlanOnly:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !planned.PlanReady || planned.PlanStale || len(planned.Pages) != 6 {
		t.Fatalf("knowledge plan = %+v", planned)
	}
	runtime := pageBySlug(t, planned, "runtime-lifecycle")
	if runtime.ParentSlug != "architecture-overview" || runtime.Depth != 1 || runtime.Number != "1.1" {
		t.Fatalf("hierarchical page metadata = %+v", runtime)
	}

	generated, err := service.Generate(ctx, GenerateRequest{
		RepositoryID: 1,
		Page:         "architecture-overview",
		Provider:     "codex",
		Model:        "gpt-5.6-sol",
		Effort:       "high",
		Refresh:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	page := pageBySlug(t, generated, "architecture-overview")
	if page.Status != StatusReady || page.Provider != "codex" || len(page.Citations) != 4 {
		t.Fatalf("generated knowledge page = %+v", page)
	}
	if err := validateKnowledgePage(page); err != nil {
		t.Fatal(err)
	}
	markdownPath := filepath.Join(docsDirectory, "repository-1", "architecture-overview.md")
	persistedMarkdown, err := os.ReadFile(markdownPath)
	if err != nil {
		t.Fatalf("read persisted Wiki Markdown: %v", err)
	}
	if string(persistedMarkdown) != markdown {
		t.Fatalf("persisted Markdown differs from generated page")
	}
	manifest, err := os.ReadFile(filepath.Join(docsDirectory, "repository-1", "manifest.json"))
	if err != nil {
		t.Fatalf("read persisted Wiki manifest: %v", err)
	}
	if bytes.Contains(manifest, []byte("The runtime is assembled")) {
		t.Fatal("Wiki Markdown was duplicated into metadata instead of remaining in the .md file")
	}
	if len(generator.requests) != 3 ||
		!strings.Contains(generator.requests[0].Message, "repository survey") ||
		!strings.Contains(generator.requests[1].Message, "saved_repository_survey") ||
		!strings.Contains(generator.requests[2].Message, "failure and recovery") {
		t.Fatalf("generation prompts = %#v", generator.requests)
	}
}

func fakeKnowledgeSurvey(revision string) (string, []agent.Citation) {
	source := func(path string, line int) agent.Citation {
		url := fmt.Sprintf(
			"http://127.0.0.1:7331/source/1?rev=%s&path=%s&focus=%d-%d#L%d",
			revision,
			path,
			line,
			line+4,
			line,
		)
		return agent.Citation{Label: path, URL: url}
	}
	sources := []agent.Citation{
		source("README.md", 1),
		source("cmd/fixture/main.go", 1),
		source("internal/server/server.go", 1),
		source("internal/store/store.go", 1),
		source("internal/server/server_test.go", 1),
		source("scripts/build.ps1", 1),
	}
	paragraph := strings.Repeat(
		"The repository keeps runtime construction, domain coordination, durable state, evidence, recovery, and tests behind explicit boundaries. ",
		5,
	)
	markdown := fmt.Sprintf(`# Repository Survey

## Product and domain

%s [%s](%s)

## Runtime composition

%s [%s](%s)

## Subsystems and boundaries

%s [%s](%s)

## State, persistence, and data flow

%s [%s](%s)

## Trust, failures, and recovery

%s [%s](%s)

## Build, operations, and tests

%s [%s](%s)

## Candidate Wiki hierarchy

%s The hierarchy should cover architecture, runtime lifecycle, search and source intelligence, persistence, operations, and glossary.
`,
		paragraph, sources[0].Label, sources[0].URL,
		paragraph, sources[1].Label, sources[1].URL,
		paragraph, sources[2].Label, sources[2].URL,
		paragraph, sources[3].Label, sources[3].URL,
		paragraph, sources[4].Label, sources[4].URL,
		paragraph, sources[5].Label, sources[5].URL,
		paragraph,
	)
	return markdown, sources
}

func fakeKnowledgeMarkdown(revision string) (string, []agent.Citation) {
	source := func(path string, line int) agent.Citation {
		url := fmt.Sprintf(
			"http://127.0.0.1:7331/source/1?rev=%s&path=%s&focus=%d-%d#L%d",
			revision,
			path,
			line,
			line+4,
			line,
		)
		return agent.Citation{Label: path, URL: url}
	}
	sources := []agent.Citation{
		source("README.md", 1),
		source("go.mod", 1),
		source("cmd/fixture/main.go", 1),
		source("internal/server/server.go", 1),
	}
	paragraph := "The implementation keeps construction, request coordination, durable state, and evidence boundaries explicit. Each boundary has a narrow responsibility, a commit-pinned input, and a recoverable failure path so callers can distinguish unavailable data from invalid state. "
	markdown := fmt.Sprintf(`# Architecture Overview

The runtime is assembled from small services and starts from a single command entry point. [%s](%s)

## Runtime composition

%s %s [%s](%s)

### Dependency construction

%s

## Request lifecycle

%s %s [%s](%s)

### End-to-end flow

%s

`+"```mermaid\nflowchart LR\n  CLI[\"CLI\"] --> Server[\"HTTP server\"]\n  Server --> Search[\"Search service\"]\n  Search --> Source[\"Commit-pinned source\"]\n```\n"+`

## State and evidence boundaries

%s %s [%s](%s)

### Revision invariants

%s

## Failures, recovery, and tests

%s %s

### Operational invariants

%s

See [Runtime Lifecycle](./runtime-lifecycle.md) and [Search and Source Intelligence](./search-and-source-intelligence.md).
`,
		sources[0].Label, sources[0].URL,
		paragraph, paragraph, sources[1].Label, sources[1].URL,
		paragraph,
		paragraph, paragraph, sources[2].Label, sources[2].URL,
		paragraph,
		paragraph, paragraph, sources[3].Label, sources[3].URL,
		paragraph,
		paragraph, paragraph,
		paragraph,
	)
	return markdown, sources
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
	if err := service.savePage(architecture); err != nil {
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

func TestGenerationControlsAreClampedToProviderLimits(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		value    int
		expected int
	}{
		{name: "unset", value: 0, expected: agent.DefaultTurnTimeoutSeconds},
		{name: "below minimum", value: 30, expected: agent.MinimumTurnTimeoutSeconds},
		{name: "supported", value: 900, expected: 900},
		{name: "above maximum", value: 9_000, expected: agent.MaximumTurnTimeoutSeconds},
	} {
		if actual := generationTimeout(testCase.value); actual != testCase.expected {
			t.Fatalf("%s timeout = %d, want %d", testCase.name, actual, testCase.expected)
		}
	}
	for _, testCase := range []struct {
		name     string
		value    int64
		expected int64
	}{
		{name: "unset", value: 0, expected: defaultGenerationBudget},
		{name: "supported", value: 12_000, expected: 12_000},
		{name: "above maximum", value: 1 << 20, expected: agent.MaximumTokenBudget},
	} {
		if actual := generationBudget(testCase.value); actual != testCase.expected {
			t.Fatalf("%s budget = %d, want %d", testCase.name, actual, testCase.expected)
		}
	}
}

func TestExportReportsAnEmptyWikiAsARecoverableState(t *testing.T) {
	t.Parallel()
	repositoryPath := initializeDocumentationRepository(t)
	revision := commitAll(t, repositoryPath, "initial")
	storage := &memoryStorage{
		repository: catalog.Repository{
			ID:            1,
			Name:          "fixture",
			Path:          repositoryPath,
			HeadCommit:    revision,
			IndexedCommit: revision,
		},
	}
	maps, err := graph.New(storage, filepath.Join(t.TempDir(), "maps"), "http://127.0.0.1:7331")
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(storage, maps, filepath.Join(t.TempDir(), "docs"))
	if err != nil {
		t.Fatal(err)
	}

	// Nothing has been generated, so exporting is a normal empty state that
	// must name its own reason rather than surface as an opaque failure.
	_, _, err = service.Export(context.Background(), 1)
	if !errors.Is(err, ErrNothingToExport) {
		t.Fatalf("empty export error = %v, want ErrNothingToExport", err)
	}
	if !strings.Contains(err.Error(), "generate at least one page") {
		t.Fatalf("empty export message = %q", err.Error())
	}
}
