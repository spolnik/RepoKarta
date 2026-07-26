package codeintel

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spolnik/RepoKarta/internal/access"
	"github.com/spolnik/RepoKarta/internal/analysis"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/graph"
	"github.com/spolnik/RepoKarta/internal/search"
)

func TestSourceWindowKeepsFocusInsideUsefulContext(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		start      int
		end        int
		windowFrom int
		windowTo   int
	}{
		{name: "near start", start: 7, end: 7, windowFrom: 1, windowTo: 200},
		{name: "middle", start: 300, end: 300, windowFrom: 220, windowTo: 419},
		{name: "wide bounded range", start: 100, end: 700, windowFrom: 201, windowTo: 700},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			start, end := SourceWindow(testCase.start, testCase.end)
			if start != testCase.windowFrom || end != testCase.windowTo {
				t.Fatalf("SourceWindow(%d, %d) = %d-%d, want %d-%d", testCase.start, testCase.end, start, end, testCase.windowFrom, testCase.windowTo)
			}
			if end-start+1 > MaximumSourceLines {
				t.Fatalf("window has %d lines, maximum is %d", end-start+1, MaximumSourceLines)
			}
		})
	}
}

type symbolTestStore struct {
	repository catalog.Repository
}

func (s symbolTestStore) ListRepositories(context.Context) ([]catalog.Repository, error) {
	if s.repository.ID == 0 {
		return []catalog.Repository{}, nil
	}
	return []catalog.Repository{s.repository}, nil
}

func (s symbolTestStore) RepositoryByID(context.Context, int64) (catalog.Repository, error) {
	return s.repository, nil
}

type capturingSearcher struct {
	query search.Query
}

type fixedResultSearcher struct {
	result search.Result
}

func (s fixedResultSearcher) Search(context.Context, search.Query) (search.Result, error) {
	return s.result, nil
}

type visibleRepositoryStore struct {
	repository catalog.Repository
}

func (s visibleRepositoryStore) ListRepositories(context.Context) ([]catalog.Repository, error) {
	return []catalog.Repository{s.repository}, nil
}

func (s visibleRepositoryStore) RepositoryByID(_ context.Context, id int64) (catalog.Repository, error) {
	if id == s.repository.ID {
		return s.repository, nil
	}
	return catalog.Repository{}, context.Canceled
}

func TestSearchDropsMatchesWithoutAnExactAuthorizedRepositoryIdentity(t *testing.T) {
	revision := strings.Repeat("a", 40)
	visible := catalog.Repository{
		ID: 1, Name: "same-name", Path: filepath.Join(t.TempDir(), "allowed", "same-name"),
		IndexedCommit: revision,
	}
	privatePath := filepath.Join(t.TempDir(), "private", "same-name")
	service := New(visibleRepositoryStore{repository: visible}, fixedResultSearcher{result: search.Result{
		Matches: []search.FileMatch{{
			Repository: privatePath,
			Revision:   revision,
			Path:       "secret.go",
			Lines:      []search.LineMatch{{Number: 1, Text: "private content"}},
		}},
		MatchCount: 1, FileCount: 1, EstimatedFiles: 1, ReturnedFiles: 1,
		TotalFilesExact: true,
	}}, "http://localhost")
	ctx := access.WithViewer(context.Background(), access.Viewer{ID: "saml:alice"})
	result, err := service.Search(ctx, SearchRequest{Query: "private"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 0 || result.MatchCount != 0 || result.MatchingFiles != 0 ||
		result.EstimatedTotalFiles != 0 {
		t.Fatalf("unauthorized search metadata leaked: %#v", result)
	}
}

func (s *capturingSearcher) Search(_ context.Context, query search.Query) (search.Result, error) {
	s.query = query
	return search.Result{}, nil
}

func TestFindSymbolUsesBoundedZoektSymbolQuery(t *testing.T) {
	searcher := &capturingSearcher{}
	service := New(symbolTestStore{repository: catalog.Repository{
		ID: 1, Name: "api", Path: filepath.Join(t.TempDir(), "api"),
	}}, searcher, "http://localhost")
	if _, err := service.FindSymbol(context.Background(), SymbolRequest{
		Symbol:     `Handle"Request`,
		Repository: "api",
		Language:   "Go",
		Limit:      17,
	}); err != nil {
		t.Fatal(err)
	}
	if searcher.query.Text != `sym:"Handle\"Request"` ||
		searcher.query.Mode != "zoekt" ||
		searcher.query.Repository != filepath.ToSlash(service.store.(symbolTestStore).repository.Path) ||
		searcher.query.Language != "Go" ||
		searcher.query.Limit != 17 {
		t.Fatalf("symbol query = %#v", searcher.query)
	}
}

func TestFindSymbolRejectsInvalidInput(t *testing.T) {
	service := New(symbolTestStore{}, &capturingSearcher{}, "http://localhost")
	for _, symbol := range []string{"", "line\nbreak", strings.Repeat("x", 201)} {
		if _, err := service.FindSymbol(context.Background(), SymbolRequest{Symbol: symbol}); err == nil {
			t.Fatalf("FindSymbol(%q) unexpectedly succeeded", symbol)
		}
	}
}

type referenceTestStore struct {
	repository catalog.Repository
}

func (s referenceTestStore) ListRepositories(context.Context) ([]catalog.Repository, error) {
	return []catalog.Repository{s.repository}, nil
}

func (s referenceTestStore) RepositoryByID(_ context.Context, id int64) (catalog.Repository, error) {
	if id == s.repository.ID {
		return s.repository, nil
	}
	return catalog.Repository{}, context.Canceled
}

type referenceTestStructure struct {
	snapshot graph.Snapshot
}

func (s referenceTestStructure) Snapshot(context.Context, int64, bool) (graph.Snapshot, error) {
	return s.snapshot, nil
}

func TestReferenceSearchUsesPersistedASTRelationsAndPinnedSource(t *testing.T) {
	directory := t.TempDir()
	sourceText := `package com.acme;
import com.acme.store.PaymentStore;
public class PaymentService extends BaseService {
    public void charge() { store.save(); }
}
`
	if err := os.MkdirAll(filepath.Join(directory, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "src", "PaymentService.java"), []byte(sourceText), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, directory, "init", "-q")
	runGit(t, directory, "add", ".")
	runGit(t, directory, "-c", "user.name=RepoKarta Test", "-c", "user.email=test@repokarta.local", "commit", "-qm", "fixture")
	revision := strings.TrimSpace(runGit(t, directory, "rev-parse", "HEAD"))
	repository := catalog.Repository{
		ID:            7,
		Name:          "payments",
		Path:          directory,
		HeadCommit:    revision,
		IndexedCommit: revision,
		IndexState:    "ready",
	}
	structure := referenceTestStructure{snapshot: graph.Snapshot{
		Repositories: []graph.Repository{{ID: 7, Name: "payments", Revision: revision}},
		Structure: []graph.StructuralDocument{{
			RepositoryID:  7,
			Repository:    "payments",
			Revision:      revision,
			Path:          "src/PaymentService.java",
			Language:      "java",
			ParseComplete: true,
			Relations: []analysis.Relation{
				{
					Kind:       "import",
					Target:     "import com.acme.store.PaymentStore;",
					Confidence: "syntax",
					Range:      analysis.Range{StartLine: 2, EndLine: 2},
				},
				{
					Kind:       "extends",
					Target:     "BaseService",
					Receiver:   "PaymentService",
					Confidence: "syntax",
					Range:      analysis.Range{StartLine: 3, EndLine: 3},
				},
				{
					Kind:       "call",
					Target:     "save",
					Receiver:   "store",
					Confidence: "syntax",
					Range:      analysis.Range{StartLine: 4, EndLine: 4},
				},
			},
		}},
		Scope: graph.Scope{
			Kind:                  "repository",
			Complete:              true,
			TotalRepositories:     1,
			AnalyzedRepositories:  1,
			RequestedRepositoryID: 7,
		},
	}}
	searcher := &capturingSearcher{}
	service := New(referenceTestStore{repository: repository}, searcher, "http://localhost").UseStructure(structure)

	result, err := service.Search(context.Background(), SearchRequest{
		Query:        "save",
		RepositoryID: 7,
		Mode:         "references",
		Limit:        10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if searcher.query.Text != "" {
		t.Fatalf("reference search unexpectedly reached Zoekt: %#v", searcher.query)
	}
	if result.SearchKind != "references" || result.ReferenceResolution != "syntax-target-name" ||
		result.MatchCount != 1 || result.MatchingFiles != 1 || result.Truncated || !result.TotalFilesExact {
		t.Fatalf("reference completeness = %#v", result)
	}
	if len(result.Matches) != 1 || len(result.Matches[0].Lines) != 1 {
		t.Fatalf("reference matches = %#v", result.Matches)
	}
	line := result.Matches[0].Lines[0]
	if line.Number != 4 || line.ReferenceKind != "call" || line.ReferenceTarget != "save" ||
		line.ReferenceReceiver != "store" || line.ReferenceConfidence != "syntax" ||
		!strings.Contains(line.Text, "store.save()") || len(line.Fragments) != 1 {
		t.Fatalf("reference line = %#v", line)
	}
	if result.Matches[0].Citation != "payments@"+revision[:8]+":src/PaymentService.java#L4-L4" ||
		!strings.Contains(result.Matches[0].SourceURL, "focus=4-4") {
		t.Fatalf("reference citation = %#v", result.Matches[0])
	}

	for _, symbol := range []string{"PaymentStore", "BaseService"} {
		found, findErr := service.FindReferences(context.Background(), ReferenceRequest{
			Symbol:       symbol,
			RepositoryID: 7,
		})
		if findErr != nil {
			t.Fatal(findErr)
		}
		if found.MatchCount != 1 || len(found.Matches) != 1 {
			t.Fatalf("FindReferences(%q) = %#v", symbol, found)
		}
	}
}

func TestReferenceSearchReportsPartialASTCoverage(t *testing.T) {
	service := New(symbolTestStore{}, &capturingSearcher{}, "http://localhost").UseStructure(
		referenceTestStructure{snapshot: graph.Snapshot{
			StructureTruncated: true,
			Scope: graph.Scope{
				Complete:             false,
				TotalRepositories:    10,
				AnalyzedRepositories: 4,
				OmittedRepositories:  6,
			},
		}},
	)
	result, err := service.FindReferences(context.Background(), ReferenceRequest{Symbol: "save"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || result.TotalFilesExact || len(result.Warnings) != 2 {
		t.Fatalf("partial coverage = %#v", result)
	}
}

func runGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return string(output)
}
