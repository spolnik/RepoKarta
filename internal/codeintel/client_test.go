package codeintel

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/spolnik/RepoKarta/internal/contextscope"
)

func TestClientCoversReadOnlyJSONSurface(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		requests = append(requests, request.Method+" "+request.URL.RequestURI())
		mu.Unlock()
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/error" {
			response.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(response, `{"error":{"message":"bounded failure"}}`)
			return
		}
		fmt.Fprint(response, `{}`)
	}))
	defer server.Close()
	client := NewClient(server.URL + "/")
	ctx := context.Background()

	if _, err := client.Repositories(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Search(ctx, SearchRequest{Query: "needle", RepositoryID: 7, Limit: 25}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Search(ctx, SearchRequest{
		Query:    "scoped",
		Contexts: []contextscope.Selector{{Kind: contextscope.KindRepository, RepositoryID: 7}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.FindSymbol(ctx, SymbolRequest{Symbol: "Thing", Repository: "repo", Limit: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.FindReferences(ctx, ReferenceRequest{Symbol: "Thing", RepositoryID: 7, Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetFile(ctx, FileRequest{RepositoryID: 7, Revision: "abc", Path: "main.go", StartLine: 2, EndLine: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListTree(ctx, TreeRequest{Repository: "repo/name", Revision: "abc", Path: "internal"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GitLog(ctx, GitLogRequest{RepositoryID: 7, Revision: "abc", Path: "main.go", Limit: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GitDiff(ctx, GitDiffRequest{RepositoryID: 7, FromRevision: "a", ToRevision: "b", Path: "main.go", ContextLines: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RepositoryMap(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GeneratedDocument(ctx, 7, "architecture overview"); err != nil {
		t.Fatal(err)
	}
	if err := client.get(ctx, "/api/error", nil, &struct{}{}); err == nil ||
		!strings.Contains(err.Error(), "bounded failure") {
		t.Fatalf("API error = %v", err)
	}
	if len(requests) != 12 {
		t.Fatalf("requests = %#v", requests)
	}
	foundPost := false
	for _, request := range requests {
		if request == "POST /api/search" {
			foundPost = true
		}
	}
	if !foundPost {
		t.Fatalf("structured search did not use POST: %#v", requests)
	}
}
