package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	page     docs.Page
}

func (f fakeArtifacts) RepositoryMap(context.Context, int64) (graph.Snapshot, error) {
	return f.snapshot, nil
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
	if len(tools.Tools) != 9 {
		t.Fatalf("got %d tools, want 9", len(tools.Tools))
	}
	toolNames := make(map[string]bool, len(tools.Tools))
	for _, tool := range tools.Tools {
		toolNames[tool.Name] = true
	}
	for _, name := range []string{
		"list_repositories",
		"search_code",
		"find_symbol",
		"get_file",
		"list_tree",
		"git_log",
		"git_diff",
		"read_repository_map",
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
