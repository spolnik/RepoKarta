package codeintel

import (
	"strings"
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

func TestSearchPresentationUsesIdentifierPathsAndNormalizedSourceRanks(t *testing.T) {
	response := SearchResponse{
		ResultType:      "content",
		TotalFilesExact: true,
		Matches: []SearchMatch{
			{
				ResultType: "content",
				Repository: "repo",
				Path:       "internal/helpers.go",
				Score:      10000,
				Lines:      []SearchLine{{Number: 1}},
			},
			{
				ResultType: "content",
				Repository: "repo",
				Path:       "internal/AuthService.go",
				Score:      1,
				Lines:      []SearchLine{{Number: 1}},
			},
		},
	}
	finalizeSearchResponse(&response, querylang.Query{Text: "authenticate user service"})
	if response.Matches[0].Path != "internal/AuthService.go" {
		t.Fatalf("identifier-aware ranking = %#v", response.Matches)
	}
	signals := rankingSignalsByName(response.Matches[0].Ranking)
	if signals["identifier_path_match"].Weight <= 0 ||
		signals["source_index_score"].Weight > maximumSourceIndexRankingWeight {
		t.Fatalf("normalized ranking signals = %#v", response.Matches[0].Ranking)
	}
	if !strings.Contains(signals["source_index_score"].Detail, "normalized weight") {
		t.Fatalf("source score explanation = %#v", signals["source_index_score"])
	}
}

func TestSearchPresentationRewardsCoherentFiles(t *testing.T) {
	response := SearchResponse{
		ResultType:      "content",
		TotalFilesExact: true,
		Matches: []SearchMatch{
			{
				ResultType: "content",
				Repository: "repo",
				Path:       "internal/first.go",
				Score:      10,
				Lines:      []SearchLine{{Number: 1}},
			},
			{
				ResultType: "content",
				Repository: "repo",
				Path:       "internal/coherent.go",
				Score:      9,
				Lines: []SearchLine{
					{Number: 1},
					{Number: 3},
					{Number: 8},
					{Number: 13},
				},
			},
		},
	}
	finalizeSearchResponse(&response, querylang.Query{Text: "needle"})
	if response.Matches[0].Path != "internal/coherent.go" {
		t.Fatalf("coherence ranking = %#v", response.Matches)
	}
	if signal := rankingSignalsByName(response.Matches[0].Ranking)["file_match_coherence"]; signal.Weight != 30 {
		t.Fatalf("coherence signal = %#v", signal)
	}
}

func TestSearchPresentationDemotesNoiseUnlessTheQueryRequestsIt(t *testing.T) {
	response := SearchResponse{
		ResultType:      "content",
		TotalFilesExact: true,
		Matches: []SearchMatch{
			{
				ResultType: "content",
				Repository: "repo",
				Path:       "internal/service_test.go",
				Score:      10,
				Lines:      []SearchLine{{Number: 1}},
			},
			{
				ResultType: "content",
				Repository: "repo",
				Path:       "internal/service.go",
				Score:      9,
				Lines:      []SearchLine{{Number: 1}},
			},
		},
	}
	finalizeSearchResponse(&response, querylang.Query{Text: "service behavior"})
	if response.Matches[0].Path != "internal/service.go" {
		t.Fatalf("noise-aware ranking = %#v", response.Matches)
	}
	testSignals := rankingSignalsByName(response.Matches[1].Ranking)
	if testSignals["test_path_penalty"].Weight != -30 {
		t.Fatalf("test penalty = %#v", response.Matches[1].Ranking)
	}

	explicit := SearchResponse{
		ResultType:      "content",
		TotalFilesExact: true,
		Matches: []SearchMatch{
			{
				ResultType: "content",
				Repository: "repo",
				Path:       "internal/service_test.go",
				Score:      10,
				Lines:      []SearchLine{{Number: 1}},
			},
			{
				ResultType: "content",
				Repository: "repo",
				Path:       "internal/service.go",
				Score:      9,
				Lines:      []SearchLine{{Number: 1}},
			},
		},
	}
	finalizeSearchResponse(&explicit, querylang.Query{
		Text: "service behavior",
		Filters: []querylang.Filter{{
			Field: querylang.FieldFile,
			Value: "_test.go",
		}},
	})
	if explicit.Matches[0].Path != "internal/service_test.go" {
		t.Fatalf("explicit file intent changed source order = %#v", explicit.Matches)
	}
	if _, penalized := rankingSignalsByName(explicit.Matches[0].Ranking)["test_path_penalty"]; penalized {
		t.Fatalf("explicit file result retained test penalty = %#v", explicit.Matches[0].Ranking)
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

func TestCompactSearchPresentationPreservesLocationsWithoutPayloadBodies(t *testing.T) {
	response := SearchResponse{
		Compact:         true,
		TotalFilesExact: true,
		Matches: []SearchMatch{{
			ResultType: "reference",
			Repository: "repo",
			Revision:   "abc",
			Path:       "internal/service.go",
			Score:      99,
			Lines: []SearchLine{{
				Number:          12,
				Text:            "service.Run()",
				Before:          "before",
				After:           "after",
				ReferenceKind:   "call",
				ReferenceTarget: "Run",
			}},
			Actions: []SearchAction{{Kind: "open", Label: "Open", URL: "/source"}},
		}},
	}
	finalizeSearchResponse(&response, querylang.Query{Text: "Run"})
	match := response.Matches[0]
	line := match.Lines[0]
	if match.Path != "internal/service.go" || line.Number != 12 ||
		line.ReferenceKind != "call" || line.ReferenceTarget != "Run" {
		t.Fatalf("compact location metadata = %#v", match)
	}
	if line.Text != "" || line.Before != "" || line.After != "" ||
		len(match.Ranking) != 0 || len(match.Actions) != 0 || len(response.Facets) != 0 ||
		response.FacetCoverage.Scope != "" || response.FacetCoverage.Complete {
		t.Fatalf("compact response retained rich payload = %#v", response)
	}
}

func rankingSignalsByName(signals []RankingSignal) map[string]RankingSignal {
	output := make(map[string]RankingSignal, len(signals))
	for _, signal := range signals {
		output[signal.Name] = signal
	}
	return output
}
