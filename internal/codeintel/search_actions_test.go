package codeintel

import (
	"strings"
	"testing"

	"github.com/spolnik/RepoKarta/internal/querylang"
)

func TestSearchActionsConnectEvidenceMapsAnalysisAndContextWorkflows(t *testing.T) {
	service := New(referenceTestStore{}, fixedResultSearcher{}, "https://repo.example.com")
	query, err := querylang.Parse(`Search result_type:symbol_definition symbol_kind:method`)
	if err != nil {
		t.Fatal(err)
	}
	response := SearchResponse{
		Matches: []SearchMatch{{
			ResultType:   "symbol_definition",
			RepositoryID: 42,
			Repository:   "RepoKarta",
			Revision:     strings.Repeat("a", 40),
			Path:         "internal/codeintel/service.go",
			SourceURL:    "https://repo.example.com/source/42?path=internal%2Fcodeintel%2Fservice.go",
			Lines:        []SearchLine{{Number: 805}},
		}},
	}

	service.addSearchActions(&response, query)
	actions := response.Matches[0].Actions
	if len(actions) != 7 {
		t.Fatalf("actions = %#v", actions)
	}
	byKind := make(map[string]SearchAction, len(actions))
	for _, action := range actions {
		byKind[action.Kind] = action
	}
	for _, kind := range []string{
		"source", "map", "dependencies", "references", "implementations", "conversation", "context",
	} {
		if byKind[kind].URL == "" {
			t.Fatalf("missing %s action in %#v", kind, actions)
		}
	}
	if !strings.Contains(byKind["map"].URL, "focus=Search") ||
		!strings.Contains(byKind["references"].URL, "result_type%3Areference") ||
		!strings.Contains(byKind["implementations"].URL, "result_type%3Aimplementation") ||
		!strings.Contains(byKind["conversation"].URL, "context_url=") ||
		strings.Contains(byKind["conversation"].URL, "reuse=current") ||
		!strings.Contains(byKind["context"].URL, "reuse=current") ||
		!strings.Contains(byKind["context"].URL, "symbol_kind%3Dmethod") {
		t.Fatalf("action URLs = %#v", byKind)
	}
}

func TestRepositoryResultActionsUseRepositoryContext(t *testing.T) {
	service := New(referenceTestStore{}, fixedResultSearcher{}, "https://repo.example.com")
	response := SearchResponse{
		Items: []SearchItem{{
			ResultType:   "repository",
			RepositoryID: 7,
			Repository:   "payments",
			Revision:     strings.Repeat("b", 40),
			Title:        "payments",
			SourceURL:    "https://repo.example.com/maps?repository=7",
		}},
	}

	service.addSearchActions(&response, querylang.Query{})
	actions := response.Items[0].Actions
	if len(actions) != 5 {
		t.Fatalf("repository actions = %#v", actions)
	}
	for _, action := range actions {
		if action.Kind == "context" &&
			strings.Contains(action.URL, "kind%3Drepository") &&
			strings.Contains(action.URL, "reuse=current") {
			return
		}
	}
	t.Fatalf("repository context action = %#v", actions)
}
