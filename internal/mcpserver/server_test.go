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
	handler := NewHandler(Config{Version: "test", BaseURL: "http://ui", Token: "secret"}, intelligence, tracker)
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
	if len(tools.Tools) != 4 {
		t.Fatalf("got %d tools, want 4", len(tools.Tools))
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
}
