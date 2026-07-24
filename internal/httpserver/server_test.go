package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/search"
)

type testStore struct {
	repositories []catalog.Repository
}

func (s testStore) ListRepositories(context.Context) ([]catalog.Repository, error) {
	return s.repositories, nil
}

func (s testStore) RepositoryByID(_ context.Context, id int64) (catalog.Repository, error) {
	for _, repository := range s.repositories {
		if repository.ID == id {
			return repository, nil
		}
	}
	return catalog.Repository{}, context.Canceled
}

type testSearcher struct{}

func (testSearcher) Search(context.Context, search.Query) (search.Result, error) {
	return search.Result{
		Duration:   2 * time.Millisecond,
		MatchCount: 1,
		Matches: []search.FileMatch{{
			Repository: "github.com/example/repo",
			Revision:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Path:       "internal/example.go",
			Language:   "Go",
			Lines: []search.LineMatch{{
				Number:    7,
				Text:      "if <tag> needle",
				Fragments: []search.Fragment{{Start: 9, End: 15}},
			}},
		}},
	}, nil
}

type testRefresher struct{}

func (testRefresher) Refresh(context.Context) error { return nil }

func TestSearchRendersSafeHighlightedCommitPinnedResult(t *testing.T) {
	repository := catalog.Repository{
		ID:            1,
		Name:          "repo",
		HeadCommit:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		IndexedCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		IndexState:    "ready",
	}
	server, err := New(Config{
		Address:        "127.0.0.1:7331",
		RepositoryRoot: `C:\code`,
		Version:        "test",
	}, testStore{repositories: []catalog.Repository{repository}}, testSearcher{}, testRefresher{})
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/search?q=needle", nil)
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, expected := range []string{
		`<mark class="search-highlight">needle</mark>`,
		`if &lt;tag&gt; `,
		`/source/1?rev=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`,
		`internal%2Fexample.go`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected response to contain %q\n%s", expected, body)
		}
	}
	if strings.Contains(body, "<tag>") {
		t.Fatal("source line was not HTML escaped")
	}
}

func TestServerRejectsUnexpectedHostAndOrigin(t *testing.T) {
	server, err := New(
		Config{Address: "127.0.0.1:7331"},
		testStore{},
		testSearcher{},
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, testCase := range []struct {
		name   string
		host   string
		origin string
	}{
		{name: "host", host: "attacker.example"},
		{name: "origin", host: "127.0.0.1:7331", origin: "https://attacker.example"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/repositories/refresh", nil)
			request.Host = testCase.host
			if testCase.origin != "" {
				request.Header.Set("Origin", testCase.origin)
			}
			response := httptest.NewRecorder()
			server.server.Handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d", response.Code)
			}
		})
	}
}
