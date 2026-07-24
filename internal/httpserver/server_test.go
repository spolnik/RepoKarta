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
		`data-search-highlight-language="Go"`,
		`data-match-ranges="9:15"`,
		`if &lt;tag&gt; `,
		`/assets/repokarta-mark-192.png`,
		`/assets/favicon.ico`,
		`/source/1?rev=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`,
		`lines=1-200&focus=7-7#L7`,
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

func TestSearchAndChatRenderAsSeparatePages(t *testing.T) {
	server, err := New(
		Config{
			Address:        "127.0.0.1:7331",
			RepositoryRoot: `C:\code`,
			Version:        "test",
			Conversations:  testConversations{},
		},
		codeintel.New(testStore{}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}

	searchRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/", nil)
	searchResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(searchResponse, searchRequest)
	if searchResponse.Code != http.StatusOK {
		t.Fatalf("search status = %d, body = %s", searchResponse.Code, searchResponse.Body.String())
	}
	searchBody := searchResponse.Body.String()
	for _, expected := range []string{`aria-current="page">Search`, `href="/chat"`, `action="/search"`} {
		if !strings.Contains(searchBody, expected) {
			t.Fatalf("search page does not contain %q", expected)
		}
	}
	if strings.Contains(searchBody, `id="conversation-form"`) {
		t.Fatal("search page unexpectedly contains the conversation form")
	}

	chatRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/chat", nil)
	chatResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(chatResponse, chatRequest)
	if chatResponse.Code != http.StatusOK {
		t.Fatalf("chat status = %d, body = %s", chatResponse.Code, chatResponse.Body.String())
	}
	chatBody := chatResponse.Body.String()
	for _, expected := range []string{
		`aria-current="page">Chat`,
		`id="conversation-form"`,
		`data-chat-prompt=`,
		`id="conversation-debug"`,
		`data-debug-copy`,
	} {
		if !strings.Contains(chatBody, expected) {
			t.Fatalf("chat page does not contain %q", expected)
		}
	}
	if strings.Contains(chatBody, `action="/search"`) {
		t.Fatal("chat page unexpectedly contains the search form")
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

func TestFragmentRangesConvertsUTF8ByteOffsetsToBrowserUTF16Offsets(t *testing.T) {
	line := search.LineMatch{
		Text:      "🐉 needle",
		Fragments: []search.Fragment{{Start: 5, End: 11}},
	}
	if ranges := fragmentRanges(line); ranges != "3:9" {
		t.Fatalf("fragmentRanges() = %q, want 3:9", ranges)
	}
}

func TestFocusRangeParsing(t *testing.T) {
	for _, testCase := range []struct {
		value string
		start int
		end   int
	}{
		{value: "120", start: 120, end: 120},
		{value: "120-125", start: 120, end: 125},
		{value: "", start: 0, end: 0},
		{value: "125-120", start: 0, end: 0},
	} {
		start, end := parseFocusRange(testCase.value)
		if start != testCase.start || end != testCase.end {
			t.Fatalf("parseFocusRange(%q) = %d-%d, want %d-%d", testCase.value, start, end, testCase.start, testCase.end)
		}
	}
}
