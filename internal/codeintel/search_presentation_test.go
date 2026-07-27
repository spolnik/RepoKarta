package codeintel

import (
	"testing"

	"github.com/spolnik/RepoKarta/internal/querylang"
)

func TestSearchPresentationRanksExactPathsAndExplainsEverySignal(t *testing.T) {
	response := SearchResponse{
		ResultType:      "file_path",
		TotalFilesExact: true,
		Matches: []SearchMatch{
			{ResultType: "file_path", Repository: "repo", Path: "internal/payment_service.go", Language: "Go", Score: 2},
			{ResultType: "file_path", Repository: "repo", Path: "service.go", Language: "Go", Score: 1},
			{ResultType: "file_path", Repository: "repo", Path: "service_worker.go", Language: "Go", Score: 50},
		},
	}
	finalizeSearchResponse(&response, querylang.Query{Text: "service.go"})
	if response.Matches[0].Path != "service.go" ||
		len(response.Matches[0].Ranking) == 0 ||
		response.Matches[0].Ranking[0].Name != "exact_path" {
		t.Fatalf("ranked matches = %#v", response.Matches)
	}
	if len(response.Matches[1].Ranking) == 0 {
		t.Fatalf("secondary ranking signal = %#v", response.Matches[1])
	}
	if response.FacetCoverage.Scope != "all_results" || !response.FacetCoverage.Complete {
		t.Fatalf("facet coverage = %#v", response.FacetCoverage)
	}
}

func TestSearchPresentationKeepsExactSymbolsAheadOfHighScoringFuzzyContent(t *testing.T) {
	response := SearchResponse{
		ResultType:      "mixed",
		TotalFilesExact: true,
		Matches: []SearchMatch{
			{ResultType: "content", Repository: "repo", Path: "internal/search_worker.go", Score: 10000},
			{ResultType: "symbol_definition", Repository: "repo", Path: "internal/service.go", Score: 1},
		},
	}
	finalizeSearchResponse(&response, querylang.Query{Text: "Search"})
	if response.Matches[0].ResultType != "symbol_definition" ||
		response.Matches[0].Ranking[0].Name != "exact_symbol" {
		t.Fatalf("ranked mixed matches = %#v", response.Matches)
	}
}

func TestSearchPresentationPrefersTypedExactPathOverContentFromTheSameFile(t *testing.T) {
	response := SearchResponse{
		ResultType:      "mixed",
		TotalFilesExact: true,
		Matches: []SearchMatch{
			{ResultType: "content", Repository: "repo", Path: "SCOPE.md", Score: 10000},
			{ResultType: "file_path", Repository: "repo", Path: "SCOPE.md", Score: 1},
		},
	}
	finalizeSearchResponse(&response, querylang.Query{Text: "SCOPE.md"})
	if response.Matches[0].ResultType != "file_path" ||
		response.Matches[0].Ranking[1].Name != "filename_only_match" {
		t.Fatalf("typed exact path order = %#v", response.Matches)
	}
}

func TestSearchPresentationBuildsGrammarCompatiblePartialFacets(t *testing.T) {
	response := SearchResponse{
		ResultType:      "dependency",
		Truncated:       true,
		TotalFilesExact: false,
		Items: []SearchItem{
			{ResultType: "dependency", Repository: "payments", Path: "internal/go.mod", Title: "mux"},
			{ResultType: "dependency", Repository: "payments", Path: "internal/go.sum", Title: "x/text"},
			{ResultType: "dependency", Repository: "orders", Path: "package.json", Title: "vite"},
		},
	}
	finalizeSearchResponse(&response, querylang.Query{Text: "mux"})
	if response.Items[0].Title != "mux" ||
		len(response.Items[0].Ranking) != 1 ||
		response.Items[0].Ranking[0].Name != "exact_title" {
		t.Fatalf("ranked items = %#v", response.Items)
	}
	if response.FacetCoverage.Scope != "returned_results" || response.FacetCoverage.Complete {
		t.Fatalf("facet coverage = %#v", response.FacetCoverage)
	}
	assertFacet := func(field, value string, count int) {
		t.Helper()
		for _, facet := range response.Facets {
			if facet.Field == field && facet.Value == value && facet.Count == count {
				return
			}
		}
		t.Fatalf("facet %s:%s=%d missing from %#v", field, value, count, response.Facets)
	}
	assertFacet(querylang.FieldRepository, "payments", 2)
	assertFacet(querylang.FieldPath, "internal", 2)
	assertFacet(querylang.FieldResultType, "dependency", 3)
}
