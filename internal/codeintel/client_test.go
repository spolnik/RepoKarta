package codeintel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
