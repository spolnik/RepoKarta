package codeintel

import (
	"context"
	"strings"
	"testing"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/search"
)

func TestSourceWindowKeepsFocusInsideUsefulContext(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		start      int
		end        int
		windowFrom int
		windowTo   int
	}{
		{name: "near start", start: 7, end: 7, windowFrom: 1, windowTo: 200},
		{name: "middle", start: 300, end: 300, windowFrom: 220, windowTo: 419},
		{name: "wide bounded range", start: 100, end: 700, windowFrom: 201, windowTo: 700},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			start, end := SourceWindow(testCase.start, testCase.end)
			if start != testCase.windowFrom || end != testCase.windowTo {
				t.Fatalf("SourceWindow(%d, %d) = %d-%d, want %d-%d", testCase.start, testCase.end, start, end, testCase.windowFrom, testCase.windowTo)
			}
			if end-start+1 > MaximumSourceLines {
				t.Fatalf("window has %d lines, maximum is %d", end-start+1, MaximumSourceLines)
			}
		})
	}
}

type symbolTestStore struct{}

func (symbolTestStore) ListRepositories(context.Context) ([]catalog.Repository, error) {
	return []catalog.Repository{}, nil
}

func (symbolTestStore) RepositoryByID(context.Context, int64) (catalog.Repository, error) {
	return catalog.Repository{}, nil
}

type capturingSearcher struct {
	query search.Query
}

func (s *capturingSearcher) Search(_ context.Context, query search.Query) (search.Result, error) {
	s.query = query
	return search.Result{}, nil
}

func TestFindSymbolUsesBoundedZoektSymbolQuery(t *testing.T) {
	searcher := &capturingSearcher{}
	service := New(symbolTestStore{}, searcher, "http://localhost")
	if _, err := service.FindSymbol(context.Background(), SymbolRequest{
		Symbol:     `Handle"Request`,
		Repository: "api",
		Language:   "Go",
		Limit:      17,
	}); err != nil {
		t.Fatal(err)
	}
	if searcher.query.Text != `sym:"Handle\"Request"` ||
		searcher.query.Mode != "zoekt" ||
		searcher.query.Repository != "api" ||
		searcher.query.Language != "Go" ||
		searcher.query.Limit != 17 {
		t.Fatalf("symbol query = %#v", searcher.query)
	}
}

func TestFindSymbolRejectsInvalidInput(t *testing.T) {
	service := New(symbolTestStore{}, &capturingSearcher{}, "http://localhost")
	for _, symbol := range []string{"", "line\nbreak", strings.Repeat("x", 201)} {
		if _, err := service.FindSymbol(context.Background(), SymbolRequest{Symbol: symbol}); err == nil {
			t.Fatalf("FindSymbol(%q) unexpectedly succeeded", symbol)
		}
	}
}
