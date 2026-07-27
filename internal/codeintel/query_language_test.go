package codeintel

import (
	"context"
	"strings"
	"testing"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

func TestSearchCompilesDocumentedQueryFilters(t *testing.T) {
	revision := strings.Repeat("a", 40)
	searcher := &capturingSearcher{}
	service := New(referenceTestStore{repository: catalog.Repository{
		ID:            7,
		Name:          "payments",
		IndexedCommit: revision,
	}}, searcher, "")

	response, err := service.Search(context.Background(), SearchRequest{
		Query: "needle content:exact -content:debug language:Go -language:Java " +
			"path:internal -path:vendor file:.go -file:_test.go " +
			"repository:payments revision:" + revision[:12] + " result_type:content",
	})
	if err != nil {
		t.Fatal(err)
	}
	query := searcher.query
	if query.Text != "needle" ||
		len(query.IncludeText) != 1 || query.IncludeText[0] != "exact" ||
		len(query.ExcludeText) != 1 || query.ExcludeText[0] != "debug" ||
		len(query.RepositoryIDs) != 1 || query.RepositoryIDs[0] != 7 ||
		len(query.Languages) != 1 || query.Languages[0] != "Go" ||
		len(query.ExcludeLanguages) != 1 || query.ExcludeLanguages[0] != "Java" ||
		len(query.Paths) != 1 || query.Paths[0] != "internal" ||
		len(query.ExcludePaths) != 1 || query.ExcludePaths[0] != "vendor" ||
		len(query.Files) != 1 || query.Files[0] != ".go" ||
		len(query.ExcludeFiles) != 1 || query.ExcludeFiles[0] != "_test.go" {
		t.Fatalf("compiled query = %#v", query)
	}
	if response.QueryLanguage == nil ||
		response.QueryLanguage.Text != "needle" ||
		len(response.QueryLanguage.Filters) != 11 {
		t.Fatalf("query provenance = %#v", response.QueryLanguage)
	}
}

func TestSearchFailsClosedForUnindexedQueryEvidence(t *testing.T) {
	searcher := &capturingSearcher{}
	service := New(symbolTestStore{}, searcher, "")
	for _, raw := range []string{
		"owner:platform",
		"symbol_kind:method",
		"result_type:dependency",
		"-result_type:content",
	} {
		if _, err := service.Search(context.Background(), SearchRequest{Query: raw}); err == nil {
			t.Fatalf("Search(%q) unexpectedly succeeded", raw)
		}
		if searcher.query.Text != "" {
			t.Fatalf("Search(%q) reached engine: %#v", raw, searcher.query)
		}
	}
}

func TestRepositoryFilterIntersectionCanResolveToNoResults(t *testing.T) {
	base, scopes, empty := applyQueryRepositoryFilters(
		[]uint32{1},
		nil,
		[]uint32{2},
		true,
		nil,
	)
	if !empty || len(base) != 0 || len(scopes) != 0 {
		t.Fatalf("intersection = base %v scopes %v empty %v", base, scopes, empty)
	}
}
