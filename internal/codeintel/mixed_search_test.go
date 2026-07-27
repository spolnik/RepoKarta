package codeintel

import (
	"context"
	"strings"
	"testing"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/contextscope"
	"github.com/spolnik/RepoKarta/internal/querylang"
	"github.com/spolnik/RepoKarta/internal/search"
)

type mixedSearchEngine struct {
	revision string
}

func (e mixedSearchEngine) Search(_ context.Context, query search.Query) (search.Result, error) {
	path := "internal/search_worker.go"
	score := 10000.0
	if query.FileNameOnly {
		path = "Search"
		score = 1
	} else if query.Mode == "zoekt" {
		path = "internal/service.go"
		score = 1
	}
	return search.Result{
		Matches: []search.FileMatch{{
			RepositoryID: 9,
			Repository:   "repo",
			Revision:     e.revision,
			Path:         path,
			Score:        score,
			Lines:        []search.LineMatch{{Number: 7, Text: "Search"}},
		}},
		MatchCount:      1,
		FileCount:       1,
		EstimatedFiles:  1,
		ReturnedFiles:   1,
		Limit:           query.Limit,
		TotalFilesExact: true,
	}, nil
}

func TestSimpleSearchMixesExactPathAndSymbolCandidatesAheadOfFuzzyContent(t *testing.T) {
	revision := strings.Repeat("a", 40)
	repository := catalog.Repository{
		ID: 9, Name: "repo", IndexedCommit: revision, HeadCommit: revision, IndexState: "ready",
	}
	service := New(
		referenceTestStore{repository: repository},
		mixedSearchEngine{revision: revision},
		"https://repo.example.com",
	)

	result, err := service.Search(t.Context(), SearchRequest{
		Query:        "Search",
		RepositoryID: repository.ID,
		Limit:        10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ResultType != "mixed" || len(result.Matches) != 3 {
		t.Fatalf("mixed result = %#v", result)
	}
	if result.Matches[0].ResultType != "file_path" ||
		result.Matches[1].ResultType != "symbol_definition" ||
		result.Matches[2].ResultType != "content" {
		t.Fatalf("mixed order = %#v", result.Matches)
	}
	if result.Matches[2].Score != 10000 ||
		result.Matches[0].Ranking[0].Name != "exact_path" ||
		result.Matches[1].Ranking[0].Name != "exact_symbol" {
		t.Fatalf("mixed ranking = %#v", result.Matches)
	}
}

func TestExplicitAndStructuredSearchesKeepOneRequestedResultType(t *testing.T) {
	parsedExplicit := querylangMustParse(t, "Search result_type:content")
	if mixedSourceSearchEligible(SearchRequest{}, parsedExplicit) {
		t.Fatal("explicit result type unexpectedly enabled mixed search")
	}
	parsedSimple := querylangMustParse(t, "Search")
	if mixedSourceSearchEligible(SearchRequest{
		Contexts: []contextscope.Selector{{Kind: contextscope.KindRepository, RepositoryID: 9}},
	}, parsedSimple) {
		t.Fatal("structured context unexpectedly enabled mixed search")
	}
}

func querylangMustParse(t *testing.T, value string) querylang.Query {
	t.Helper()
	parsed, err := querylang.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
