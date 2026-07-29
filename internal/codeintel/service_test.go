package codeintel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spolnik/RepoKarta/internal/access"
	"github.com/spolnik/RepoKarta/internal/analysis"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/contextscope"
	"github.com/spolnik/RepoKarta/internal/graph"
	"github.com/spolnik/RepoKarta/internal/scipindex"
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

func TestRepositoryAPIExposesEmptyTerminalReason(t *testing.T) {
	repository := catalog.Repository{
		ID:         31,
		Name:       "empty",
		ScanState:  "empty",
		ScanError:  catalog.EmptyRepositoryReason,
		IndexState: "empty",
		IndexError: catalog.EmptyRepositoryReason,
	}
	service := New(referenceTestStore{repository: repository}, fixedResultSearcher{}, "")
	result, err := service.Repositories(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Repositories) != 1 ||
		result.Repositories[0].ScanState != "empty" ||
		result.Repositories[0].ScanError != catalog.EmptyRepositoryReason ||
		result.Repositories[0].IndexState != "empty" ||
		result.Repositories[0].IndexError != catalog.EmptyRepositoryReason {
		t.Fatalf("empty repository API = %#v", result)
	}
}

func TestServiceCommittedFileTreeAndHistoryAPIs(t *testing.T) {
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, directory, "init", "-q")
	runGit(t, directory, "config", "user.name", "RepoKarta Test")
	runGit(t, directory, "config", "user.email", "test@repokarta.local")
	filePath := filepath.Join(directory, "internal", "service.go")
	if err := os.WriteFile(filePath, []byte("package internal\n\nfunc Service() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, directory, "add", ".")
	runGit(t, directory, "commit", "-qm", "first")
	firstRevision := strings.TrimSpace(runGit(t, directory, "rev-parse", "HEAD"))
	if err := os.WriteFile(filePath, []byte("package internal\n\nfunc Service() int { return 2 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, directory, "add", ".")
	runGit(t, directory, "commit", "-qm", "second")
	secondRevision := strings.TrimSpace(runGit(t, directory, "rev-parse", "HEAD"))
	repository := catalog.Repository{
		ID: 19, Name: "service", Path: directory,
		OriginURL:  "https://github.com/example/service.git",
		HeadCommit: secondRevision, IndexedCommit: secondRevision, IndexState: "ready",
	}
	service := New(referenceTestStore{repository: repository}, fixedResultSearcher{}, "")
	service.SetBaseURL("http://127.0.0.1:7331/")

	repositories, err := service.Repositories(context.Background())
	if err != nil || len(repositories.Repositories) != 1 || repositories.Repositories[0].ID != repository.ID {
		t.Fatalf("repositories = %#v, %v", repositories, err)
	}
	catalogue, err := service.CatalogRepositories(context.Background())
	if err != nil || len(catalogue) != 1 {
		t.Fatalf("catalogue = %#v, %v", catalogue, err)
	}
	selected, err := service.RepositoryByID(context.Background(), repository.ID)
	if err != nil || selected.Path != directory {
		t.Fatalf("repository = %#v, %v", selected, err)
	}
	file, err := service.GetFile(context.Background(), FileRequest{
		RepositoryID: repository.ID, Revision: secondRevision,
		Path: "internal/service.go", StartLine: 1, EndLine: 3,
	})
	if err != nil || len(file.Lines) != 3 || !strings.Contains(file.Lines[2].Text, "return 2") || file.SourceURL == "" {
		t.Fatalf("file = %#v, %v", file, err)
	}
	tree, err := service.ListTree(context.Background(), TreeRequest{
		RepositoryID: repository.ID, Revision: secondRevision, Path: "internal",
	})
	if err != nil || len(tree.Entries) != 1 || tree.Entries[0].Path != "internal/service.go" {
		t.Fatalf("tree = %#v, %v", tree, err)
	}
	log, err := service.GitLog(context.Background(), GitLogRequest{
		RepositoryID: repository.ID, Revision: secondRevision, Path: "internal/service.go", Limit: 2,
	})
	if err != nil || len(log.Commits) != 2 {
		t.Fatalf("log = %#v, %v", log, err)
	}
	diff, err := service.GitDiff(context.Background(), GitDiffRequest{
		RepositoryID: repository.ID, FromRevision: firstRevision,
		ToRevision: secondRevision, Path: "internal/service.go", ContextLines: 3,
	})
	if err != nil || !strings.Contains(diff.Patch, "return 2") {
		t.Fatalf("diff = %#v, %v", diff, err)
	}
	if _, err := service.GetFile(context.Background(), FileRequest{
		RepositoryID: repository.ID, Path: "../secret",
	}); err == nil {
		t.Fatal("unsafe file path was accepted")
	}
}

func TestListTreePaginatesEveryCommittedDirectoryEntry(t *testing.T) {
	directory := t.TempDir()
	runGit(t, directory, "init", "-q")
	runGit(t, directory, "config", "user.name", "RepoKarta Test")
	runGit(t, directory, "config", "user.email", "test@repokarta.local")
	for index := 0; index < MaximumTreeEntries+2; index++ {
		name := filepath.Join(directory, fmt.Sprintf("file-%03d.txt", index))
		if err := os.WriteFile(name, []byte(strconv.Itoa(index)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, directory, "add", ".")
	runGit(t, directory, "commit", "-qm", "large tree")
	revision := strings.TrimSpace(runGit(t, directory, "rev-parse", "HEAD"))
	repository := catalog.Repository{
		ID: 29, Name: "large-tree", Path: directory,
		HeadCommit: revision, IndexedCommit: revision, IndexState: "ready",
	}
	service := New(referenceTestStore{repository: repository}, fixedResultSearcher{}, "")

	first, err := service.ListTree(context.Background(), TreeRequest{
		RepositoryID: repository.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Entries) != MaximumTreeEntries || !first.Truncated ||
		first.Offset != 0 || first.NextOffset != MaximumTreeEntries {
		t.Fatalf("first tree page = %#v", first)
	}
	last, err := service.ListTree(context.Background(), TreeRequest{
		RepositoryID: repository.ID,
		Offset:       first.NextOffset,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(last.Entries) != 2 || last.Truncated || last.Offset != MaximumTreeEntries ||
		last.NextOffset != 0 || last.Entries[0].Path != "file-500.txt" {
		t.Fatalf("last tree page = %#v", last)
	}
	pastEnd, err := service.ListTree(context.Background(), TreeRequest{
		RepositoryID: repository.ID,
		Offset:       MaximumTreeEntries * 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pastEnd.Entries) != 0 || pastEnd.Offset != MaximumTreeEntries+2 ||
		pastEnd.Truncated || pastEnd.NextOffset != 0 {
		t.Fatalf("past-end tree page = %#v", pastEnd)
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
	query  search.Query
	result search.Result
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

func TestStructuredFileContextPinsAndScopesSearch(t *testing.T) {
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "internal", "context.go"), []byte("package internal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, directory, "init", "-q")
	runGit(t, directory, "add", ".")
	runGit(t, directory, "-c", "user.name=RepoKarta Test", "-c", "user.email=test@repokarta.local", "commit", "-qm", "fixture")
	revision := strings.TrimSpace(runGit(t, directory, "rev-parse", "HEAD"))
	repository := catalog.Repository{
		ID:            7,
		Name:          "context repo",
		Path:          directory,
		HeadCommit:    revision,
		IndexedCommit: revision,
		IndexState:    "ready",
	}
	searcher := &capturingSearcher{}
	service := New(referenceTestStore{repository: repository}, searcher, "http://localhost")
	viewerContext := access.WithViewer(context.Background(), access.Viewer{ID: "saml:alice"})
	result, err := service.Search(viewerContext, SearchRequest{
		Query: "package",
		Contexts: []contextscope.Selector{{
			Kind:         contextscope.KindFile,
			RepositoryID: repository.ID,
			Revision:     revision,
			Path:         "internal/context.go",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(searcher.query.Scopes) != 1 ||
		searcher.query.Scopes[0].Repository != filepath.ToSlash(directory) ||
		searcher.query.Scopes[0].Path != "internal/context.go" {
		t.Fatalf("structured scopes = %#v", searcher.query.Scopes)
	}
	if len(searcher.query.RepositoryIDs) != 0 {
		t.Fatalf("structured contexts duplicated repository allow-list: %#v", searcher.query.RepositoryIDs)
	}
	if len(result.Contexts) != 1 ||
		result.Contexts[0].RepositoryID != repository.ID ||
		result.Contexts[0].Revision != revision ||
		result.Contexts[0].Label != "@context repo:internal/context.go" {
		t.Fatalf("resolved contexts = %#v", result.Contexts)
	}
	suggestions, err := service.SuggestContexts(context.Background(), ContextSuggestionRequest{
		Kind: contextscope.KindFile, Query: "context", RepositoryID: repository.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(suggestions.Suggestions) != 1 ||
		suggestions.Suggestions[0].Context.Path != "internal/context.go" ||
		suggestions.Suggestions[0].Context.Revision != revision {
		t.Fatalf("file suggestions = %#v", suggestions)
	}
}

func TestFileContextSuggestionsCacheImmutableGitTree(t *testing.T) {
	revision := strings.Repeat("a", 40)
	repository := catalog.Repository{
		ID: 17, Name: "cached", Path: t.TempDir(),
		IndexedCommit: revision, IndexState: "ready",
	}
	service := New(referenceTestStore{repository: repository}, &capturingSearcher{}, "http://localhost")
	loads := 0
	service.contextFileLoader = func(context.Context, catalog.Repository, string) ([]string, bool, error) {
		loads++
		return []string{"internal/cache.go", "README.md"}, false, nil
	}
	for _, query := range []string{"cache", "readme"} {
		suggestions, err := service.SuggestContexts(context.Background(), ContextSuggestionRequest{
			Kind: contextscope.KindFile, Query: query, RepositoryID: repository.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(suggestions.Suggestions) != 1 {
			t.Fatalf("suggestions for %q = %#v", query, suggestions)
		}
	}
	if loads != 1 {
		t.Fatalf("immutable Git tree loads = %d, want 1", loads)
	}
}

func TestDirectoryAndSymbolContextsResolveAndScopeSearch(t *testing.T) {
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, "internal", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for filePath, content := range map[string]string{
		"internal/service.go":       "package internal\n\nfunc Run() {}\n",
		"internal/nested/worker.go": "package nested\n\nfunc Run() {}\n",
		"README.md":                 "outside\n",
	} {
		fullPath := filepath.Join(directory, filepath.FromSlash(filePath))
		if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, directory, "init", "-q")
	runGit(t, directory, "add", ".")
	runGit(t, directory, "-c", "user.name=RepoKarta Test", "-c", "user.email=test@repokarta.local", "commit", "-qm", "fixture")
	revision := strings.TrimSpace(runGit(t, directory, "rev-parse", "HEAD"))
	repository := catalog.Repository{
		ID: 23, Name: "contexts", Path: directory, HeadCommit: revision,
		IndexedCommit: revision, IndexState: "ready",
	}
	structure := referenceTestStructure{index: graph.StructuralIndex{
		Scope: graph.Scope{Complete: true, TotalRepositories: 1, AnalyzedRepositories: 1},
		Structure: []graph.StructuralDocument{
			{
				RepositoryID: repository.ID, Repository: repository.Name,
				Revision: revision, Path: "internal/service.go",
				Symbols: []analysis.Symbol{{
					Name: "Run", Kind: "function",
					Range: analysis.Range{StartLine: 10, EndLine: 20},
				}},
			},
			{
				RepositoryID: repository.ID, Repository: repository.Name,
				Revision: revision, Path: "internal/nested/worker.go",
				Symbols: []analysis.Symbol{{
					Name: "Run", Kind: "function",
					Range: analysis.Range{StartLine: 30, EndLine: 35},
				}},
			},
		},
	}}
	searcher := &capturingSearcher{result: search.Result{
		Matches: []search.FileMatch{
			{
				RepositoryID: repository.ID, Repository: filepath.ToSlash(directory),
				Revision: revision, Path: "internal/service.go",
				Lines: []search.LineMatch{
					{Number: 5, Text: "before"},
					{Number: 12, Text: "inside"},
					{Number: 21, Text: "after"},
				},
			},
			{
				RepositoryID: repository.ID, Repository: filepath.ToSlash(directory),
				Revision: revision, Path: "README.md",
				Lines: []search.LineMatch{{Number: 1, Text: "outside"}},
			},
		},
		MatchCount: 4, FileCount: 2, EstimatedFiles: 2, ReturnedFiles: 2,
		TotalFilesExact: true,
	}}
	service := New(referenceTestStore{repository: repository}, searcher, "http://localhost").UseStructure(structure)

	directorySuggestions, err := service.SuggestContexts(t.Context(), ContextSuggestionRequest{
		Kind: contextscope.KindDirectory, Query: "nested", RepositoryID: repository.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(directorySuggestions.Suggestions) != 1 ||
		directorySuggestions.Suggestions[0].Context.Path != "internal/nested" ||
		directorySuggestions.Suggestions[0].Label != "@contexts:internal/nested/" {
		t.Fatalf("directory suggestions = %#v", directorySuggestions)
	}
	directoryContexts, err := service.ResolveContexts(t.Context(), []contextscope.Selector{{
		Kind: contextscope.KindDirectory, RepositoryID: repository.ID,
		Revision: revision, Path: "internal",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(directoryContexts) != 1 || directoryContexts[0].Kind != contextscope.KindDirectory {
		t.Fatalf("directory contexts = %#v", directoryContexts)
	}

	symbolSuggestions, err := service.SuggestContexts(t.Context(), ContextSuggestionRequest{
		Kind: contextscope.KindSymbol, Query: "service", RepositoryID: repository.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(symbolSuggestions.Suggestions) != 1 {
		t.Fatalf("symbol suggestions = %#v", symbolSuggestions)
	}
	symbolSelector := symbolSuggestions.Suggestions[0].Context
	if symbolSelector.Symbol != "Run" || symbolSelector.SymbolKind != "function" ||
		symbolSelector.Line != 10 || symbolSelector.Path != "internal/service.go" {
		t.Fatalf("symbol selector = %#v", symbolSelector)
	}
	symbolContexts, err := service.ResolveContexts(t.Context(), []contextscope.Selector{symbolSelector})
	if err != nil {
		t.Fatal(err)
	}
	if len(symbolContexts) != 1 ||
		symbolContexts[0].StartLine != 10 ||
		symbolContexts[0].EndLine != 20 ||
		symbolContexts[0].Label != "@contexts:internal/service.go#Run:10" {
		t.Fatalf("symbol contexts = %#v", symbolContexts)
	}

	_, err = service.ResolveContexts(t.Context(), []contextscope.Selector{{
		Kind: contextscope.KindSymbol, RepositoryID: repository.ID,
		Revision: revision, Symbol: "Run",
	}})
	var resolutionError *contextscope.ResolutionError
	if !errors.As(err, &resolutionError) ||
		len(resolutionError.Issues) != 1 ||
		resolutionError.Issues[0].Code != "ambiguous_symbol" {
		t.Fatalf("ambiguous symbol error = %#v, %v", resolutionError, err)
	}

	result, err := service.Search(t.Context(), SearchRequest{
		Query: "needle", Contexts: []contextscope.Selector{symbolSelector},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(searcher.query.Scopes) != 1 ||
		searcher.query.Scopes[0].Kind != search.ScopeKindSymbol ||
		searcher.query.Scopes[0].StartLine != 10 ||
		searcher.query.Scopes[0].EndLine != 20 {
		t.Fatalf("symbol search scope = %#v", searcher.query.Scopes)
	}
	if len(result.Matches) != 1 ||
		len(result.Matches[0].Lines) != 1 ||
		result.Matches[0].Lines[0].Number != 12 ||
		result.MatchCount != 1 {
		t.Fatalf("symbol-scoped result = %#v", result)
	}
}

func TestStructuredContextErrorsNeverBroadenSearch(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "present.go"), []byte("package fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, directory, "init", "-q")
	runGit(t, directory, "add", ".")
	runGit(t, directory, "-c", "user.name=RepoKarta Test", "-c", "user.email=test@repokarta.local", "commit", "-qm", "fixture")
	revision := strings.TrimSpace(runGit(t, directory, "rev-parse", "HEAD"))
	repository := catalog.Repository{
		ID: 9, Name: "fixture", Path: directory, HeadCommit: revision,
		IndexedCommit: revision, IndexState: "ready",
	}
	for _, testCase := range []struct {
		name     string
		selector contextscope.Selector
		code     string
	}{
		{
			name: "stale revision",
			selector: contextscope.Selector{
				Kind: contextscope.KindRepository, RepositoryID: repository.ID,
				Revision: strings.Repeat("b", 40),
			},
			code: "stale",
		},
		{
			name: "missing file",
			selector: contextscope.Selector{
				Kind: contextscope.KindFile, RepositoryID: repository.ID,
				Revision: revision, Path: "missing.go",
			},
			code: "missing_file",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			searcher := &capturingSearcher{}
			service := New(referenceTestStore{repository: repository}, searcher, "http://localhost")
			_, err := service.Search(context.Background(), SearchRequest{
				Query: "package", Contexts: []contextscope.Selector{testCase.selector},
			})
			var resolutionError *contextscope.ResolutionError
			if !errors.As(err, &resolutionError) ||
				len(resolutionError.Issues) != 1 ||
				resolutionError.Issues[0].Code != testCase.code {
				t.Fatalf("resolution error = %#v, error = %v", resolutionError, err)
			}
			if searcher.query.Text != "" {
				t.Fatalf("invalid context reached search engine: %#v", searcher.query)
			}
		})
	}
}

func (s *capturingSearcher) Search(_ context.Context, query search.Query) (search.Result, error) {
	s.query = query
	return s.result, nil
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
		len(searcher.query.RepositoryIDs) != 1 ||
		searcher.query.RepositoryIDs[0] != uint32(service.store.(symbolTestStore).repository.ID) ||
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

type referenceFleetStore struct {
	repositories []catalog.Repository
}

func (s referenceFleetStore) ListRepositories(context.Context) ([]catalog.Repository, error) {
	return append([]catalog.Repository(nil), s.repositories...), nil
}

func (s referenceFleetStore) RepositoryByID(_ context.Context, id int64) (catalog.Repository, error) {
	for _, repository := range s.repositories {
		if repository.ID == id {
			return repository, nil
		}
	}
	return catalog.Repository{}, context.Canceled
}

func (s referenceTestStore) RepositoryByID(_ context.Context, id int64) (catalog.Repository, error) {
	if id == s.repository.ID {
		return s.repository, nil
	}
	return catalog.Repository{}, context.Canceled
}

type referenceTestStructure struct {
	index graph.StructuralIndex
}

func (s referenceTestStructure) ReadStructure(context.Context, int64) (graph.StructuralIndex, error) {
	return s.index, nil
}

type referenceTestSCIP struct {
	artifact scipindex.Artifact
	err      error
}

func (s referenceTestSCIP) Read(
	_ context.Context,
	repositoryID int64,
	revision string,
) (scipindex.Artifact, bool, error) {
	if s.err != nil {
		return scipindex.Artifact{}, false, s.err
	}
	if s.artifact.RepositoryID != repositoryID || s.artifact.Revision != revision {
		return scipindex.Artifact{}, false, nil
	}
	return s.artifact, true, nil
}

type referenceFleetSCIP struct {
	artifacts map[int64]scipindex.Artifact
}

func (s referenceFleetSCIP) Read(
	_ context.Context,
	repositoryID int64,
	revision string,
) (scipindex.Artifact, bool, error) {
	artifact, ok := s.artifacts[repositoryID]
	if !ok || artifact.Revision != revision {
		return scipindex.Artifact{}, false, nil
	}
	return artifact, true, nil
}

func TestReferenceSearchUsesPersistedASTRelationsAndPinnedSource(t *testing.T) {
	directory := t.TempDir()
	sourceText := `package com.acme;
import com.acme.store.PaymentStore;
public class PaymentService extends BaseService {
    public void charge() { store.save(); }
    public boolean accepted() { return PaymentStatus.APPROVED.isTerminal(); }
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
	structure := referenceTestStructure{index: graph.StructuralIndex{
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
					Target:     "com.acme.store.PaymentStore",
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
				{
					Kind:       "member",
					Target:     "APPROVED",
					Receiver:   "PaymentStatus",
					Confidence: "syntax",
					Range:      analysis.Range{StartLine: 5, EndLine: 5},
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
	if result.ReferenceIndex == nil || result.ReferenceIndex.State != "ready" ||
		result.ReferenceIndex.ReadyRepositories != 1 {
		t.Fatalf("reference index = %#v", result.ReferenceIndex)
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

	for _, symbol := range []string{"PaymentStore", "BaseService", "PaymentStatus"} {
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

	implementations, err := service.Search(context.Background(), SearchRequest{
		Query: "BaseService result_type:implementation repository:payments",
		Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if implementations.ResultType != "implementation" ||
		implementations.SearchKind != "implementations" ||
		implementations.MatchCount != 1 ||
		len(implementations.Matches) != 1 ||
		implementations.Matches[0].ResultType != "implementation" ||
		implementations.Matches[0].Lines[0].ReferenceKind != "extends" {
		t.Fatalf("implementation results = %#v", implementations)
	}

	references, err := service.Search(context.Background(), SearchRequest{
		Query: "save result_type:reference repository:payments language:java",
		Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if references.ResultType != "reference" ||
		references.MatchCount != 1 ||
		len(references.Matches) != 1 ||
		references.Matches[0].ResultType != "reference" {
		t.Fatalf("typed reference results = %#v", references)
	}

	const semanticSave = "scip-java maven com.acme:payments 1.0.0 com/acme/store/PaymentStore#save()."
	precise := New(referenceTestStore{repository: repository}, searcher, "http://localhost").
		UseStructure(structure).
		UseSCIP(referenceTestSCIP{artifact: scipindex.Artifact{
			RepositoryID: 7,
			Revision:     revision,
			Symbols: []scipindex.Symbol{{
				ID:          semanticSave,
				DisplayName: "save",
			}},
			Documents: []scipindex.Document{{
				Path:     "src/PaymentService.java",
				Language: "java",
				Occurrences: []scipindex.Occurrence{{
					Symbol:    semanticSave,
					StartLine: 3,
				}},
			}},
		}})
	preciseResult, err := precise.FindReferences(context.Background(), ReferenceRequest{
		Symbol:       "save",
		RepositoryID: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if preciseResult.ReferenceResolution != "scip-unique-name" ||
		preciseResult.ReferenceIndex == nil ||
		preciseResult.ReferenceIndex.Provider != "scip" ||
		preciseResult.MatchCount != 1 ||
		preciseResult.Matches[0].Lines[0].ReferenceTarget != semanticSave ||
		preciseResult.Matches[0].Lines[0].ReferenceConfidence != "compiler" {
		t.Fatalf("precise references = %#v", preciseResult)
	}

	ambiguous := precise.UseSCIP(referenceTestSCIP{artifact: scipindex.Artifact{
		RepositoryID: 7,
		Revision:     revision,
		Symbols: []scipindex.Symbol{
			{ID: semanticSave, DisplayName: "save"},
			{ID: "scip-java maven com.acme:payments 1.0.0 com/acme/other/OtherStore#save().", DisplayName: "save"},
		},
	}})
	ambiguousResult, err := ambiguous.FindReferences(context.Background(), ReferenceRequest{
		Symbol:       "save",
		RepositoryID: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ambiguousResult.ReferenceResolution != "syntax-target-name" ||
		ambiguousResult.ReferenceIndex == nil ||
		ambiguousResult.ReferenceIndex.Provider != "tree-sitter" {
		t.Fatalf("ambiguous SCIP fallback = %#v", ambiguousResult)
	}
}

func TestFleetSCIPCoverageIgnoresExplicitlyNonJavaRepositories(t *testing.T) {
	const (
		revision     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		semanticSave = "scip-java maven com.acme:payments 1.0.0 com/acme/PaymentService#save()."
	)
	ready := &catalog.SCIPIndexStatus{
		Provider: "scip-java", State: "ready", Applicable: true, Revision: revision,
	}
	skipped := &catalog.SCIPIndexStatus{
		Provider: "scip-java", State: "skipped", Applicable: false, Revision: revision,
	}
	repositories := []catalog.Repository{
		{ID: 7, Name: "payments", IndexedCommit: revision, IndexState: "ready", SCIPJava: ready},
		{ID: 8, Name: "frontend", IndexedCommit: revision, IndexState: "ready", SCIPJava: skipped},
	}
	artifact := scipindex.Artifact{
		RepositoryID: 7, Revision: revision,
		Symbols: []scipindex.Symbol{{ID: semanticSave, DisplayName: "save"}},
		Documents: []scipindex.Document{{
			Path: "src/PaymentService.java", Language: "java",
			Occurrences: []scipindex.Occurrence{{Symbol: semanticSave, StartLine: 3}},
		}},
	}
	service := New(referenceFleetStore{repositories: repositories}, fixedResultSearcher{}, "").
		UseSCIP(referenceFleetSCIP{artifacts: map[int64]scipindex.Artifact{7: artifact}})
	syntax := graph.StructuralIndex{
		Scope: graph.Scope{
			Kind: "collection", Complete: true,
			TotalRepositories: 2, AnalyzedRepositories: 2,
		},
		Structure: []graph.StructuralDocument{
			{RepositoryID: 7, Language: "java"},
			{RepositoryID: 8, Language: "typescript"},
		},
	}
	index, resolution, semantic, warnings, ok, err := service.scipReferenceIndex(
		context.Background(), 0, "save", syntax, nil,
	)
	if err != nil || len(warnings) != 0 || !ok || semantic == nil || resolution != semanticSave ||
		index.Scope.TotalRepositories != 1 ||
		index.Scope.AnalyzedRepositories != 1 {
		t.Fatalf("Java-aware SCIP index = %#v, %q, %v, %v", index, resolution, ok, err)
	}

	failed := *ready
	failed.State = "failed"
	repositories[0].SCIPJava = &failed
	service = New(referenceFleetStore{repositories: repositories}, fixedResultSearcher{}, "").
		UseSCIP(referenceFleetSCIP{artifacts: map[int64]scipindex.Artifact{7: artifact}})
	if _, _, _, _, ok, err := service.scipReferenceIndex(context.Background(), 0, "save", syntax, nil); err != nil || ok {
		t.Fatalf("incomplete Java SCIP coverage = %v, %v", ok, err)
	}
}

func TestReferenceSearchFallsBackWithWarningWhenSCIPArtifactIsUnusable(t *testing.T) {
	revision := strings.Repeat("d", 40)
	repository := catalog.Repository{
		ID: 7, Name: "payments", IndexedCommit: revision, IndexState: "ready",
		SCIPJava: &catalog.SCIPIndexStatus{
			Provider: "scip-java", State: "ready", Applicable: true, Revision: revision,
		},
	}
	service := New(referenceTestStore{repository: repository}, &capturingSearcher{}, "").
		UseStructure(referenceTestStructure{index: graph.StructuralIndex{
			Scope: graph.Scope{
				Kind: "repository", Complete: true,
				TotalRepositories: 1, AnalyzedRepositories: 1,
				RequestedRepositoryID: repository.ID,
			},
			Structure: []graph.StructuralDocument{{
				RepositoryID: repository.ID, Repository: repository.Name,
				Revision: revision, Path: "PaymentService.java", Language: "java",
				ParseComplete: true,
				Relations: []analysis.Relation{{
					Kind: "call", Target: "save", Confidence: "syntax",
					Range: analysis.Range{StartLine: 4, EndLine: 4},
				}},
			}},
		}}).
		UseSCIP(referenceTestSCIP{
			err: errors.New("SCIP artifact identity does not match its requested repository revision"),
		})

	result, err := service.FindReferences(t.Context(), ReferenceRequest{
		Symbol: "save", RepositoryID: repository.ID, Compact: true,
	})
	if err != nil {
		t.Fatalf("stale SCIP fallback returned an error: %v", err)
	}
	if result.ReferenceResolution != "syntax-target-name" ||
		result.ReferenceIndex == nil ||
		result.ReferenceIndex.Provider != "tree-sitter" ||
		result.MatchCount != 1 {
		t.Fatalf("stale SCIP fallback = %#v", result)
	}
	if len(result.Warnings) != 1 ||
		result.Warnings[0].Code != "scip_artifact_unusable" ||
		!strings.Contains(result.Warnings[0].Message, "Tree-sitter") {
		t.Fatalf("stale SCIP warnings = %#v", result.Warnings)
	}
}

func TestReferenceSearchReportsPartialASTCoverage(t *testing.T) {
	service := New(symbolTestStore{}, &capturingSearcher{}, "http://localhost").UseStructure(
		referenceTestStructure{index: graph.StructuralIndex{
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
	if result.ReferenceIndex == nil || result.ReferenceIndex.State != "building" ||
		result.ReferenceIndex.ReadyRepositories != 4 ||
		result.ReferenceIndex.PendingRepositories != 6 {
		t.Fatalf("partial index progress = %#v", result.ReferenceIndex)
	}
}

func TestCompactReferenceSearchUsesCachedRelationsWithoutOpeningSource(t *testing.T) {
	revision := strings.Repeat("c", 40)
	repository := catalog.Repository{
		ID:            17,
		Name:          "cached-only",
		Path:          filepath.Join(t.TempDir(), "missing-checkout"),
		HeadCommit:    revision,
		IndexedCommit: revision,
		IndexState:    "ready",
	}
	service := New(
		referenceTestStore{repository: repository},
		&capturingSearcher{},
		"http://localhost",
	).UseStructure(referenceTestStructure{index: graph.StructuralIndex{
		Structure: []graph.StructuralDocument{{
			RepositoryID:  repository.ID,
			Repository:    repository.Name,
			Revision:      revision,
			Path:          "src/Consumer.java",
			Language:      "java",
			ParseComplete: true,
			Relations: []analysis.Relation{{
				Kind:       "type",
				Target:     "JobTimeGuard",
				Confidence: "syntax",
				Range:      analysis.Range{StartLine: 42, EndLine: 42},
			}},
		}},
		Scope: graph.Scope{
			Kind:                  "repository",
			Complete:              true,
			TotalRepositories:     1,
			AnalyzedRepositories:  1,
			RequestedRepositoryID: repository.ID,
		},
	}})

	result, err := service.FindReferences(t.Context(), ReferenceRequest{
		Symbol:       "JobTimeGuard",
		RepositoryID: repository.ID,
		Compact:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Compact || result.MatchCount != 1 || result.ReturnedFiles != 1 ||
		len(result.Matches) != 1 || len(result.Matches[0].Lines) != 1 {
		t.Fatalf("compact reference result = %#v", result)
	}
	match := result.Matches[0]
	line := match.Lines[0]
	if match.Path != "src/Consumer.java" || line.Number != 42 ||
		line.ReferenceKind != "type" || line.ReferenceTarget != "JobTimeGuard" ||
		line.Text != "" || line.Before != "" || line.After != "" ||
		len(line.Fragments) != 0 || match.Citation == "" || match.SourceURL == "" {
		t.Fatalf("compact cached evidence = %#v", match)
	}
}

func TestReferenceSearchPreservesTypedRecallAcrossTwoThousandJavaFiles(t *testing.T) {
	const (
		fileCount     = 2_000
		consumerCount = 6
	)
	root := t.TempDir()
	sourceDirectory := filepath.Join(root, "src", "main", "java", "com", "acme")
	if err := os.MkdirAll(sourceDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < fileCount-consumerCount-1; index++ {
		fields := strings.Builder{}
		for relation := 0; relation < 16; relation++ {
			fmt.Fprintf(&fields, "    Type%02d field%02d;\n", relation, relation)
		}
		content := fmt.Sprintf("package com.acme;\nclass Filler%04d {\n%s}\n", index, fields.String())
		name := filepath.Join(sourceDirectory, fmt.Sprintf("A%04d.java", index))
		if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(sourceDirectory, "MJobTimeGuard.java"),
		[]byte("package com.acme;\npublic class JobTimeGuard {}\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	expected := make(map[string]struct{}, consumerCount)
	for index := 0; index < consumerCount; index++ {
		base := fmt.Sprintf("ZConsumer%02d.java", index)
		expected["src/main/java/com/acme/"+base] = struct{}{}
		content := fmt.Sprintf(
			"package com.acme;\nclass Consumer%02d {\n    JobTimeGuard guard;\n    void run(JobTimeGuard input) {}\n}\n",
			index,
		)
		if err := os.WriteFile(filepath.Join(sourceDirectory, base), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, root, "init", "-q")
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=RepoKarta Test", "-c", "user.email=test@repokarta.local", "commit", "-qm", "fixture")
	revision := strings.TrimSpace(runGit(t, root, "rev-parse", "HEAD"))
	repository := catalog.Repository{
		ID:            27,
		Name:          "payment-service",
		Path:          root,
		HeadCommit:    revision,
		IndexedCommit: revision,
		IndexState:    "ready",
	}
	store := symbolTestStore{repository: repository}
	structure, err := graph.New(store, filepath.Join(t.TempDir(), "maps"), "http://localhost")
	if err != nil {
		t.Fatal(err)
	}
	if err := structure.PrepareStructure(t.Context(), repository.ID); err != nil {
		t.Fatal(err)
	}
	service := New(store, &capturingSearcher{}, "http://localhost").UseStructure(structure)
	result, err := service.FindReferences(t.Context(), ReferenceRequest{
		Symbol:       "JobTimeGuard",
		RepositoryID: repository.ID,
		Limit:        100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.MatchingFiles != consumerCount || len(result.Matches) != consumerCount {
		t.Fatalf("typed reference recall = %d files (%d returned), want %d: %#v",
			result.MatchingFiles, len(result.Matches), consumerCount, result.Warnings)
	}
	for _, match := range result.Matches {
		if _, ok := expected[match.Path]; !ok {
			t.Fatalf("unexpected typed reference file %q", match.Path)
		}
		delete(expected, match.Path)
	}
	if len(expected) != 0 {
		t.Fatalf("missing typed reference consumers: %#v", expected)
	}
}

func TestASTSearchUsesNodeKindCandidatesAndStablePagination(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"a.go": "package fixture\n\nfunc Alpha() {}\n",
		"b.go": "package fixture\n\nfunc Beta() {}\n",
		"c.go": "package fixture\n\nvar Value = 1\n",
	}
	for filePath, content := range files {
		if err := os.WriteFile(filepath.Join(root, filePath), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, root, "init", "-q")
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=RepoKarta Test", "-c", "user.email=test@repokarta.local", "commit", "-qm", "fixture")
	revision := strings.TrimSpace(runGit(t, root, "rev-parse", "HEAD"))
	repository := catalog.Repository{
		ID: 31, Name: "ast-fixture", Path: root,
		HeadCommit: revision, IndexedCommit: revision, IndexState: "ready",
	}
	documents := make([]graph.StructuralDocument, 0, len(files))
	for _, filePath := range []string{"a.go", "b.go", "c.go"} {
		analyzed, supported := analysis.Analyze(filePath, []byte(files[filePath]))
		if !supported {
			t.Fatalf("%s was not analyzed", filePath)
		}
		documents = append(documents, graph.StructuralDocument{
			RepositoryID:  repository.ID,
			Repository:    repository.Name,
			Revision:      revision,
			Path:          filePath,
			Language:      analyzed.Language,
			Parser:        analyzed.Parser,
			ParseComplete: analyzed.ParseComplete,
			NodeKinds:     analyzed.NodeKinds,
		})
	}
	structure := referenceTestStructure{index: graph.StructuralIndex{
		ID: "ast-index-v1",
		Scope: graph.Scope{
			Complete: true, TotalRepositories: 1, AnalyzedRepositories: 1,
		},
		Structure: documents,
	}}
	service := New(referenceTestStore{repository: repository}, &capturingSearcher{}, "http://localhost").
		UseStructure(structure)
	request := ASTSearchRequest{
		RepositoryID: repository.ID,
		Language:     "go",
		Query:        `(function_declaration name: (identifier) @name) @function`,
		Limit:        1,
	}
	first, err := service.SearchAST(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.CandidateFiles != 2 || first.ScannedFiles != 1 ||
		len(first.Matches) != 1 || !slices.ContainsFunc(first.Matches[0].Captures, func(capture analysis.QueryCapture) bool {
		return capture.Name == "name" && capture.Text == "Alpha"
	}) ||
		first.NextCursor == "" || !first.Complete {
		t.Fatalf("first AST page = %#v", first)
	}
	request.Cursor = first.NextCursor
	second, err := service.SearchAST(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if second.CandidateFiles != 2 || len(second.Matches) != 1 ||
		!slices.ContainsFunc(second.Matches[0].Captures, func(capture analysis.QueryCapture) bool {
			return capture.Name == "name" && capture.Text == "Beta"
		}) || second.NextCursor != "" ||
		!second.Complete {
		t.Fatalf("second AST page = %#v", second)
	}
	request.Query = `(identifier) @identifier`
	if _, err := service.SearchAST(t.Context(), request); err == nil ||
		!strings.Contains(err.Error(), "stale") {
		t.Fatalf("changed query cursor error = %v, want stale cursor", err)
	}
}

func TestASTSearchReportsBuildingArtifactAsIncomplete(t *testing.T) {
	service := New(symbolTestStore{}, &capturingSearcher{}, "http://localhost").UseStructure(
		referenceTestStructure{index: graph.StructuralIndex{
			ID: "building",
			Scope: graph.Scope{
				Complete: false, TotalRepositories: 3,
				AnalyzedRepositories: 1, OmittedRepositories: 2,
			},
		}},
	)
	result, err := service.SearchAST(t.Context(), ASTSearchRequest{
		Language: "go",
		Query:    `(function_declaration) @function`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Index.State != "building" || result.Index.PendingRepositories != 2 || result.Complete {
		t.Fatalf("building AST result = %#v", result)
	}
}

func runGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	command.Env = append(os.Environ(),
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=gc.auto",
		"GIT_CONFIG_VALUE_0=0",
		"GIT_CONFIG_KEY_1=maintenance.auto",
		"GIT_CONFIG_VALUE_1=false",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return string(output)
}
