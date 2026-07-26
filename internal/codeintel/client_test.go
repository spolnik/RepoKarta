package codeintel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spolnik/RepoKarta/internal/contextscope"
)

func TestClientUsesJSONSearchContractAndOmitsUnsetLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/search" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		if request.URL.Query().Get("q") != "needle" {
			t.Fatalf("query = %q", request.URL.RawQuery)
		}
		if _, present := request.URL.Query()["limit"]; present {
			t.Fatalf("unset limit should be omitted: %q", request.URL.RawQuery)
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(SearchResponse{
			ReturnedFiles:   2,
			MatchingFiles:   9,
			Truncated:       true,
			TotalFilesExact: true,
			Limit:           DefaultSearchLimit,
		})
	}))
	defer server.Close()

	result, err := NewClient(server.URL).Search(context.Background(), SearchRequest{Query: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ReturnedFiles != 2 || result.MatchingFiles != 9 || !result.Truncated {
		t.Fatalf("result = %#v", result)
	}
}

func TestClientPostsStructuredSearchContexts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/search" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		var input SearchRequest
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		if len(input.Contexts) != 1 ||
			input.Contexts[0].RepositoryID != 42 ||
			input.Contexts[0].Path != "internal/app/app.go" {
			t.Fatalf("contexts = %#v", input.Contexts)
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(SearchResponse{Limit: DefaultSearchLimit})
	}))
	defer server.Close()

	_, err := NewClient(server.URL).Search(context.Background(), SearchRequest{
		Query: "New",
		Contexts: []contextscope.Selector{{
			Kind: contextscope.KindFile, RepositoryID: 42, Path: "internal/app/app.go",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestClientReturnsStructuredAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(response).Encode(map[string]any{
			"error": map[string]string{"message": "limit must be an integer from 1 to 500"},
		})
	}))
	defer server.Close()

	_, err := NewClient(server.URL).Search(context.Background(), SearchRequest{Query: "needle"})
	if err == nil || err.Error() != "RepoKarta API: limit must be an integer from 1 to 500" {
		t.Fatalf("error = %v", err)
	}
}

func TestClientUsesGitHistoryContracts(t *testing.T) {
	requestNumber := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestNumber++
		response.Header().Set("Content-Type", "application/json")
		switch requestNumber {
		case 1:
			if request.URL.Path != "/api/git/log/RepoKarta" ||
				request.URL.Query().Get("rev") != "abc1234" ||
				request.URL.Query().Get("path") != "internal/source" ||
				request.URL.Query().Get("limit") != "12" {
				t.Fatalf("log request = %s?%s", request.URL.Path, request.URL.RawQuery)
			}
			_ = json.NewEncoder(response).Encode(GitLogResponse{Limit: 12})
		case 2:
			if request.URL.Path != "/api/git/diff/RepoKarta" ||
				request.URL.Query().Get("from") != "abc1234" ||
				request.URL.Query().Get("to") != "def5678" ||
				request.URL.Query().Get("path") != "main.go" ||
				request.URL.Query().Get("context") != "5" {
				t.Fatalf("diff request = %s?%s", request.URL.Path, request.URL.RawQuery)
			}
			_ = json.NewEncoder(response).Encode(GitDiffResponse{ContextLines: 5})
		default:
			t.Fatalf("unexpected request %d", requestNumber)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL)
	logResult, err := client.GitLog(context.Background(), GitLogRequest{
		Repository: "RepoKarta",
		Revision:   "abc1234",
		Path:       "internal/source",
		Limit:      12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if logResult.Limit != 12 {
		t.Fatalf("log result = %#v", logResult)
	}
	diffResult, err := client.GitDiff(context.Background(), GitDiffRequest{
		Repository:   "RepoKarta",
		FromRevision: "abc1234",
		ToRevision:   "def5678",
		Path:         "main.go",
		ContextLines: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if diffResult.ContextLines != 5 {
		t.Fatalf("diff result = %#v", diffResult)
	}
}

func TestClientUsesMapAndGeneratedDocumentContracts(t *testing.T) {
	requestNumber := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestNumber++
		response.Header().Set("Content-Type", "application/json")
		switch requestNumber {
		case 1:
			if request.URL.Path != "/api/maps" || request.URL.Query().Get("repository") != "8" {
				t.Fatalf("map request = %s?%s", request.URL.Path, request.URL.RawQuery)
			}
			_, _ = response.Write([]byte(`{"id":"snapshot-8","nodes":[],"edges":[]}`))
		case 2:
			if request.URL.Path != "/api/wiki/8/architecture" {
				t.Fatalf("document request = %s?%s", request.URL.Path, request.URL.RawQuery)
			}
			_, _ = response.Write([]byte(`{"repository_id":8,"slug":"architecture","status":"ready","markdown":"# Architecture"}`))
		default:
			t.Fatalf("unexpected request %d", requestNumber)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL)
	snapshot, err := client.RepositoryMap(context.Background(), 8)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ID != "snapshot-8" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	page, err := client.GeneratedDocument(context.Background(), 8, "architecture")
	if err != nil {
		t.Fatal(err)
	}
	if page.Slug != "architecture" || page.Markdown != "# Architecture" {
		t.Fatalf("page = %+v", page)
	}
}
