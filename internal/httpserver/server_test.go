package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/codeintel"
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
		Duration:        2 * time.Millisecond,
		MatchCount:      250,
		FileCount:       250,
		EstimatedFiles:  250,
		ReturnedFiles:   1,
		Limit:           100,
		Truncated:       true,
		TotalFilesExact: true,
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

func TestAPISearchReturnsCompletenessAndPinnedEvidence(t *testing.T) {
	repository := catalog.Repository{
		ID:            1,
		Name:          "repo",
		HeadCommit:    strings.Repeat("a", 40),
		IndexedCommit: strings.Repeat("a", 40),
		IndexState:    "ready",
	}
	server, err := New(
		Config{Address: "127.0.0.1:7331"},
		codeintel.New(
			testStore{repositories: []catalog.Repository{repository}},
			testSearcher{},
			"http://127.0.0.1:7331",
		),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/search?q=needle&limit=100", nil)
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var result codeintel.SearchResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || result.ReturnedFiles != 1 || result.MatchingFiles != 250 || !result.TotalFilesExact {
		t.Fatalf("completeness = %#v", result)
	}
	if len(result.Matches) != 1 || result.Matches[0].Citation == "" || result.Matches[0].SourceURL == "" {
		t.Fatalf("evidence = %#v", result.Matches)
	}
}

type testRefresher struct{}

func (testRefresher) Refresh(context.Context) error { return nil }

type testConversations struct{}

func (testConversations) Statuses(context.Context) []agent.Status {
	return []agent.Status{{ID: "test", Name: "Test", Available: true, Authenticated: true}}
}

func (testConversations) Send(_ context.Context, request agent.TurnRequest, emit func(agent.Event) error) error {
	if err := emit(agent.Event{Type: agent.EventMeta, ConversationID: "conversation"}); err != nil {
		return err
	}
	if err := emit(agent.Event{Type: agent.EventDelta, Text: "answer:" + request.Message}); err != nil {
		return err
	}
	return emit(agent.Event{Type: agent.EventDone, ConversationID: "conversation"})
}

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
	}, codeintel.New(
		testStore{repositories: []catalog.Repository{repository}},
		testSearcher{},
		"http://127.0.0.1:7331",
	), testRefresher{})
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
		`/assets/repokarta-mark-192.png`,
		`/assets/favicon.ico`,
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

func TestChatStreamsNDJSON(t *testing.T) {
	server, err := New(
		Config{
			Address:       "127.0.0.1:7331",
			Conversations: testConversations{},
		},
		codeintel.New(testStore{}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:7331/api/chat",
		bytes.NewBufferString(`{"provider":"test","message":"hello"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/x-ndjson") {
		t.Fatalf("content type = %q", contentType)
	}
	body := response.Body.String()
	for _, expected := range []string{
		`"type":"meta"`,
		`"conversation_id":"conversation"`,
		`"type":"delta"`,
		`"text":"answer:hello"`,
		`"type":"done"`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("body does not contain %q: %s", expected, body)
		}
	}
}

func TestServerRejectsUnexpectedHostAndOrigin(t *testing.T) {
	server, err := New(
		Config{Address: "127.0.0.1:7331"},
		codeintel.New(testStore{}, testSearcher{}, "http://127.0.0.1:7331"),
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
