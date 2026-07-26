package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/codeintel"
	"github.com/spolnik/RepoKarta/internal/docs"
	"github.com/spolnik/RepoKarta/internal/graph"
	"github.com/spolnik/RepoKarta/internal/search"
)

type fakeStore struct {
	repositories []catalog.Repository
}

func (s fakeStore) ListRepositories(context.Context) ([]catalog.Repository, error) {
	return s.repositories, nil
}

func (s fakeStore) RepositoryByID(_ context.Context, id int64) (catalog.Repository, error) {
	for _, repository := range s.repositories {
		if repository.ID == id {
			return repository, nil
		}
	}
	return catalog.Repository{}, context.Canceled
}

type fakeSearcher struct {
	result search.Result
	query  search.Query
}

func (s *fakeSearcher) Search(_ context.Context, query search.Query) (search.Result, error) {
	s.query = query
	return s.result, nil
}

type fakeArtifacts struct {
	snapshot graph.Snapshot
	site     docs.Site
	page     docs.Page
}

func (f fakeArtifacts) RepositoryMap(context.Context, int64) (graph.Snapshot, error) {
	return f.snapshot, nil
}

func (f fakeArtifacts) GeneratedDocuments(context.Context, int64) (docs.Site, error) {
	return f.site, nil
}

func (f fakeArtifacts) GeneratedDocument(context.Context, int64, string) (docs.Page, error) {
	return f.page, nil
}

type bearerTransport struct {
	token string
}

func (t bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(clone)
}

func TestMCPRequiresBearerToken(t *testing.T) {
	intelligence := codeintel.New(fakeStore{}, &fakeSearcher{}, "http://ui")
	handler := NewHandler(Config{Version: "test", BaseURL: "http://ui", Token: "secret"}, intelligence)
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestMCPSearchReturnsPinnedCitation(t *testing.T) {
	revision := strings.Repeat("a", 40)
	store := fakeStore{repositories: []catalog.Repository{{
		ID:            7,
		Name:          "RepoKarta",
		IndexedCommit: revision,
		IndexState:    "ready",
	}}}
	searcher := &fakeSearcher{result: search.Result{
		MatchCount: 1,
		Matches: []search.FileMatch{{
			Repository: "RepoKarta",
			Revision:   revision,
			Path:       "internal/source/source.go",
			Language:   "go",
			Lines: []search.LineMatch{{
				Number: 21,
				Text:   "func OpenFile() {}",
			}},
		}},
	}}
	tracker := NewCitationTracker()
	intelligence := codeintel.New(store, searcher, "http://ui")
	artifacts := fakeArtifacts{
		snapshot: graph.Snapshot{
			ID: "map-1",
			Repositories: []graph.Repository{{
				ID:       7,
				Name:     "RepoKarta",
				Revision: revision,
			}},
			Manifests: []graph.Manifest{{
				RepositoryID: 7,
				Repository:   "RepoKarta",
				Kind:         "Gradle build",
				Path:         "build.gradle",
				Name:         "RepoKarta",
				Dependencies: []string{"org.springframework:spring-web:6.1.2"},
				Evidence: graph.Evidence{
					RepositoryID: 7,
					Repository:   "RepoKarta",
					Revision:     revision,
					Path:         "build.gradle",
					Line:         1,
					Label:        "Gradle build",
					URL:          "http://ui/source/7?rev=" + revision + "&path=build.gradle",
				},
			}},
			Nodes: []graph.Node{{
				ID:   "repository:7",
				Kind: "repository",
				Evidence: []graph.Evidence{{
					Repository: "RepoKarta",
					Revision:   revision,
					Path:       "README.md",
					Line:       1,
					URL:        "http://ui/source/7?rev=" + revision + "&path=README.md",
				}},
			}},
			Edges: []graph.Edge{
				{
					ID:     "dependency",
					Source: "manifest:7:build-gradle",
					Target: "dependency:spring-web",
					Kind:   "dependency",
					Label:  "declares",
					Evidence: []graph.Evidence{{
						RepositoryID: 7,
						Repository:   "RepoKarta",
						Revision:     revision,
						Path:         "build.gradle",
						Line:         12,
						Label:        "org.springframework:spring-web:6.1.2",
						URL:          "http://ui/source/7?rev=" + revision + "&path=build.gradle&line=12",
					}},
				},
				{
					ID:         "service-call",
					Source:     "repository:7",
					Target:     "repository:9",
					Kind:       "service_call",
					Label:      "calls over HTTP",
					Confidence: "high",
					Evidence: []graph.Evidence{{
						RepositoryID: 7,
						Repository:   "RepoKarta",
						Revision:     revision,
						Path:         "src/main/java/Client.java",
						Line:         21,
						Label:        "business-api",
						URL:          "http://ui/source/7?rev=" + revision + "&path=src/main/java/Client.java&line=21",
					}},
				},
			},
			Scope: graph.Scope{
				Kind:                  "repository",
				Complete:              true,
				TotalRepositories:     1,
				AnalyzedRepositories:  1,
				RequestedRepositoryID: 7,
			},
		},
		site: docs.Site{
			Version:      2,
			RepositoryID: 7,
			Repository:   "RepoKarta",
			Revision:     revision,
			PlanReady:    true,
			PlanRevision: revision,
			Ready:        1,
			Pages: []docs.Page{{
				RepositoryID:    7,
				Slug:            "overview",
				Title:           "Overview",
				Summary:         "System boundaries.",
				Number:          "1",
				Order:           1,
				Status:          docs.StatusReady,
				Revision:        revision,
				SupportingFiles: []string{"README.md"},
				Citations: []graph.Evidence{{
					Path: "README.md",
				}},
				Markdown: "# Must not be returned by the index",
			}},
		},
		page: docs.Page{
			RepositoryID: 7,
			Slug:         "overview",
			Status:       docs.StatusReady,
			Revision:     revision,
			Markdown:     "# Overview",
			Citations: []graph.Evidence{{
				Repository: "RepoKarta",
				Revision:   revision,
				Path:       "README.md",
				Line:       1,
				URL:        "http://ui/source/7?rev=" + revision + "&path=README.md",
			}},
		},
	}
	handler := NewHandler(Config{
		Version:   "test",
		BaseURL:   "http://ui",
		Token:     "secret",
		Artifacts: artifacts,
	}, intelligence, tracker)
	server := httptest.NewServer(handler)
	defer server.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             server.URL + "?conversation_id=conversation",
		HTTPClient:           &http.Client{Transport: bearerTransport{token: "secret"}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 12 {
		t.Fatalf("got %d tools, want 12", len(tools.Tools))
	}
	toolNames := make(map[string]bool, len(tools.Tools))
	for _, tool := range tools.Tools {
		toolNames[tool.Name] = true
	}
	for _, name := range []string{
		"list_repositories",
		"search_code",
		"find_symbol",
		"find_references",
		"get_file",
		"list_tree",
		"git_log",
		"git_diff",
		"read_repository_map",
		"read_dependency_inventory",
		"list_deep_wiki_pages",
		"read_generated_document",
	} {
		if !toolNames[name] {
			t.Fatalf("missing MCP tool %q: %#v", name, toolNames)
		}
	}
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "search_code",
		Arguments: map[string]any{"query": "OpenFile", "limit": 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("tool error: %#v", result.Content)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var output searchCodeOutput
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	if searcher.query.Limit != 100 {
		t.Fatalf("search limit = %d, want %d", searcher.query.Limit, 100)
	}
	if len(output.Matches) != 1 {
		t.Fatalf("matches = %#v", output.Matches)
	}
	match := output.Matches[0]
	if match.Citation != "RepoKarta@aaaaaaaa:internal/source/source.go#L21-L21" {
		t.Fatalf("citation = %q", match.Citation)
	}
	if match.SourceURL != "http://ui/source/7?focus=21-21&lines=1-200&path=internal%2Fsource%2Fsource.go&rev="+revision+"#L21" {
		t.Fatalf("source url = %q", match.SourceURL)
	}
	citations := tracker.List("conversation")
	if len(citations) != 1 || citations[0].URL != match.SourceURL || citations[0].Label != match.Citation {
		t.Fatalf("tracked citations = %#v", citations)
	}

	result, err = session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "read_repository_map",
		Arguments: map[string]any{"repository_id": 7},
	})
	if err != nil || result.IsError {
		t.Fatalf("map tool error: %v %#v", err, result.Content)
	}
	encoded, err = json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var mapOutput readRepositoryMapOutput
	if err := json.Unmarshal(encoded, &mapOutput); err != nil {
		t.Fatal(err)
	}
	if mapOutput.ID != "map-1" {
		t.Fatalf("map output = %+v", mapOutput)
	}

	result, err = session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "read_dependency_inventory",
		Arguments: map[string]any{"repository_id": 7},
	})
	if err != nil || result.IsError {
		t.Fatalf("dependency tool error: %v %#v", err, result.Content)
	}
	encoded, err = json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var dependencyOutput readDependencyInventoryOutput
	if err := json.Unmarshal(encoded, &dependencyOutput); err != nil {
		t.Fatal(err)
	}
	if dependencyOutput.DependencyCount != 1 ||
		dependencyOutput.Dependencies[0].Coordinate != "org.springframework:spring-web:6.1.2" ||
		dependencyOutput.Dependencies[0].Evidence[0].Line != 12 ||
		dependencyOutput.ServiceCallCount != 1 ||
		!dependencyOutput.Scope.Complete {
		t.Fatalf("dependency output = %+v", dependencyOutput)
	}

	result, err = session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_deep_wiki_pages",
		Arguments: map[string]any{"repository_id": 7},
	})
	if err != nil || result.IsError {
		t.Fatalf("Wiki index tool error: %v %#v", err, result.Content)
	}
	encoded, err = json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var wikiOutput listDeepWikiPagesOutput
	if err := json.Unmarshal(encoded, &wikiOutput); err != nil {
		t.Fatal(err)
	}
	if len(wikiOutput.Pages) != 1 || wikiOutput.Pages[0].Slug != "overview" ||
		wikiOutput.Pages[0].CitationCount != 1 {
		t.Fatalf("Wiki index output = %+v", wikiOutput)
	}
	if strings.Contains(string(encoded), "Must not be returned") ||
		strings.Contains(string(encoded), `"markdown"`) {
		t.Fatalf("Wiki index leaked page Markdown: %s", encoded)
	}

	result, err = session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "read_generated_document",
		Arguments: map[string]any{"repository_id": 7, "page": "overview"},
	})
	if err != nil || result.IsError {
		t.Fatalf("document tool error: %v %#v", err, result.Content)
	}
	encoded, err = json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var pageOutput readGeneratedDocumentOutput
	if err := json.Unmarshal(encoded, &pageOutput); err != nil {
		t.Fatal(err)
	}
	if pageOutput.Slug != "overview" || pageOutput.Markdown != "# Overview" {
		t.Fatalf("document output = %+v", pageOutput)
	}
}

func TestMCPToolsSelectRepositoriesByIDOnly(t *testing.T) {
	revision := strings.Repeat("b", 40)
	store := fakeStore{repositories: []catalog.Repository{{
		ID:            42,
		Name:          "RepoKarta",
		Path:          t.TempDir(),
		IndexedCommit: revision,
		IndexState:    "ready",
	}}}
	searcher := &fakeSearcher{}
	handler := NewHandler(Config{
		Version: "test",
		BaseURL: "http://ui",
		Token:   "secret",
	}, codeintel.New(store, searcher, "http://ui"))
	server := httptest.NewServer(handler)
	defer server.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             server.URL,
		HTTPClient:           &http.Client{Transport: bearerTransport{token: "secret"}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	required := map[string]bool{
		"get_file":                  true,
		"list_tree":                 true,
		"git_log":                   true,
		"git_diff":                  true,
		"read_repository_map":       true,
		"read_dependency_inventory": true,
		"list_deep_wiki_pages":      true,
		"read_generated_document":   true,
	}
	optional := map[string]bool{"search_code": true, "find_symbol": true, "find_references": true}
	for _, tool := range tools.Tools {
		encoded, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(encoded, &schema); err != nil {
			t.Fatal(err)
		}
		if _, ok := schema.Properties["repository"]; ok {
			t.Fatalf("tool %q still advertises a repository name parameter", tool.Name)
		}
		_, hasID := schema.Properties["repository_id"]
		if (required[tool.Name] || optional[tool.Name]) != hasID {
			t.Fatalf("tool %q repository_id presence = %v", tool.Name, hasID)
		}
		if required[tool.Name] && !slices.Contains(schema.Required, "repository_id") {
			t.Fatalf("tool %q does not require repository_id: %v", tool.Name, schema.Required)
		}
	}

	// A repository-scoped search must reach the engine as the resolved name.
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "search_code",
		Arguments: map[string]any{"query": "OpenFile", "repository_id": 42},
	}); err != nil {
		t.Fatal(err)
	}
	if searcher.query.Repository != "RepoKarta" {
		t.Fatalf("search repository filter = %q", searcher.query.Repository)
	}

	// An unknown repository ID must fail instead of silently searching all.
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_tree",
		Arguments: map[string]any{"repository_id": 999},
	})
	if err == nil && !result.IsError {
		t.Fatal("unknown repository_id was accepted")
	}
}
