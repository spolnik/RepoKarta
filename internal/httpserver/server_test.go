package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/codeintel"
	"github.com/spolnik/RepoKarta/internal/docs"
	"github.com/spolnik/RepoKarta/internal/graph"
	"github.com/spolnik/RepoKarta/internal/search"
	"github.com/spolnik/RepoKarta/internal/security"
)

type testStore struct {
	repositories []catalog.Repository
}

func TestAdministratorCanEnableAllowedOpenMode(t *testing.T) {
	settingsStore := &testSettingsStore{values: make(map[string]string)}
	securityManager, err := security.New(context.Background(), settingsStore, security.Config{
		Address:       "127.0.0.1:7331",
		DataDirectory: t.TempDir(),
		AllowOpen:     true,
		AdminUser:     "bootstrap-admin",
		AdminPassword: "correct horse battery staple",
		Initial:       security.Settings{Mode: security.ModeLocal},
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(
		Config{
			Address:  "127.0.0.1:7331",
			Version:  "test",
			Security: securityManager,
		},
		codeintel.New(testStore{}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/admin", nil)
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/admin/login" {
		t.Fatalf("anonymous admin response = %d, location %q", response.Code, response.Header().Get("Location"))
	}

	loginForm := url.Values{
		"username": {"bootstrap-admin"},
		"password": {"correct horse battery staple"},
	}
	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/admin/login", strings.NewReader(loginForm.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || len(response.Result().Cookies()) != 1 {
		t.Fatalf("login response = %d, cookies %d, body %q", response.Code, len(response.Result().Cookies()), response.Body.String())
	}
	adminCookie := response.Result().Cookies()[0]

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/admin", nil)
	request.AddCookie(adminCookie)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Cloudflare Access") {
		t.Fatalf("admin response = %d, body %q", response.Code, response.Body.String())
	}
	match := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(response.Body.String())
	if len(match) != 2 {
		t.Fatalf("admin page did not include CSRF token: %q", response.Body.String())
	}

	updateForm := url.Values{
		"csrf":       {match[1]},
		"mode":       {string(security.ModeOpen)},
		"public_url": {"https://repo.example.com"},
	}
	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/admin/security", strings.NewReader(updateForm.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(adminCookie)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "saved and activated") {
		t.Fatalf("update response = %d, body %q", response.Code, response.Body.String())
	}
	if securityManager.Mode() != security.ModeOpen {
		t.Fatalf("mode = %q, want %q", securityManager.Mode(), security.ModeOpen)
	}
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

type testSettingsStore struct {
	values map[string]string
}

func (store *testSettingsStore) AppSetting(_ context.Context, key string) (string, bool, error) {
	value, ok := store.values[key]
	return value, ok, nil
}

func (store *testSettingsStore) SetAppSetting(_ context.Context, key, value string) error {
	store.values[key] = value
	return nil
}

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

type testMapService struct {
	snapshot     graph.Snapshot
	repositoryID int64
	refresh      bool
}

func (s *testMapService) Snapshot(_ context.Context, repositoryID int64, refresh bool) (graph.Snapshot, error) {
	s.repositoryID = repositoryID
	s.refresh = refresh
	return s.snapshot, nil
}

type testDocumentationService struct {
	site      docs.Site
	page      docs.Page
	generated docs.GenerateRequest
}

func (s *testDocumentationService) Plan(_ context.Context, repositoryID int64) (docs.Site, error) {
	s.site.RepositoryID = repositoryID
	return s.site, nil
}

func (s *testDocumentationService) Generate(_ context.Context, request docs.GenerateRequest) (docs.Site, error) {
	s.generated = request
	return s.site, nil
}

func (s *testDocumentationService) Page(_ context.Context, repositoryID int64, slug string) (docs.Page, error) {
	s.page.RepositoryID = repositoryID
	s.page.Slug = slug
	return s.page, nil
}

func (*testDocumentationService) Export(context.Context, int64) ([]byte, string, error) {
	return []byte("PK fixture"), "repokarta-wiki-fixture.zip", nil
}

func TestMCPSetupPageProvidesCopyableReadOnlyConfiguration(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	transportCalled := false
	server, err := New(
		Config{
			Address:    "127.0.0.1:7331",
			Version:    "test",
			MCPToken:   token,
			MCPBaseURL: "http://127.0.0.1:7331",
			MCPCommand: `C:\Tools\RepoKarta\repokarta.exe`,
			MCPHandler: http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				transportCalled = true
				response.WriteHeader(http.StatusNoContent)
			}),
		},
		codeintel.New(testStore{}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/mcp/setup", nil)
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("MCP setup status = %d, body = %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", response.Header().Get("Cache-Control"))
	}
	for _, expected := range []string{
		`aria-current="page">MCP`,
		`data-mcp-secret`,
		`data-mcp-secret-toggle`,
		`http://127.0.0.1:7331/mcp`,
		`Authorization`,
		`Bearer ` + token,
		`C:\\Tools\\RepoKarta\\repokarta.exe`,
		`read_repository_map`,
		`read_generated_document`,
		`9 tools · no writes`,
	} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("MCP setup page does not contain %q", expected)
		}
	}

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/mcp", nil)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || !transportCalled {
		t.Fatalf("MCP transport status = %d, called = %v", response.Code, transportCalled)
	}
}

func TestDocumentationPageAPIGenerationAndExport(t *testing.T) {
	repository := catalog.Repository{ID: 6, Name: "Documented Repo", IndexState: "ready"}
	documents := &testDocumentationService{
		site: docs.Site{
			Version:    1,
			Repository: repository.Name,
			Revision:   strings.Repeat("b", 40),
			Pages: []docs.Page{{
				RepositoryID: repository.ID,
				Slug:         "overview",
				Title:        "Overview",
				Status:       docs.StatusReady,
			}},
			Ready: 1,
		},
		page: docs.Page{
			Title:    "Overview",
			Status:   docs.StatusReady,
			Revision: strings.Repeat("b", 40),
			Markdown: "# Overview",
		},
	}
	server, err := New(
		Config{Address: "127.0.0.1:7331", Docs: documents},
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

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/wiki", nil)
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("wiki page status = %d, body = %s", response.Code, response.Body.String())
	}
	for _, expected := range []string{
		`aria-current="page">Wiki`,
		`data-wiki-workspace`,
		`data-wiki-repository-picker`,
		`data-wiki-repository-search`,
		`data-wiki-repository-option`,
		`data-wiki-pages`,
		`data-wiki-page-count`,
		`data-wiki-content`,
	} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("wiki page does not contain %q", expected)
		}
	}

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/wiki?repository=6", nil)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"repository_id":6`) {
		t.Fatalf("wiki plan status = %d, body = %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:7331/api/wiki/generate",
		bytes.NewBufferString(`{"repository_id":6,"page":"overview","refresh":true,"survey_only":true,"plan_only":true,"preset":"quality","provider":"codex","model":"gpt-test","effort":"high","timeout_seconds":600,"token_budget":32000}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		documents.generated.RepositoryID != 6 ||
		documents.generated.Page != "overview" ||
		!documents.generated.Refresh ||
		!documents.generated.SurveyOnly ||
		!documents.generated.PlanOnly ||
		documents.generated.Preset != "quality" ||
		documents.generated.Provider != "codex" ||
		documents.generated.Model != "gpt-test" ||
		documents.generated.Effort != "high" ||
		documents.generated.Timeout != 600 ||
		documents.generated.TokenBudget != 32000 {
		t.Fatalf("generate status = %d, request = %+v, body = %s", response.Code, documents.generated, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/wiki/6/overview", nil)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"markdown":"# Overview"`) {
		t.Fatalf("wiki page API status = %d, body = %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/wiki/export?repository=6", nil)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("wiki export status = %d, headers = %v", response.Code, response.Header())
	}
	if got := response.Header().Get("Content-Disposition"); got != `attachment; filename="repokarta-wiki-fixture.zip"` {
		t.Fatalf("content disposition = %q", got)
	}
}

func TestRepositoryMapPageAPIAndExport(t *testing.T) {
	repository := catalog.Repository{ID: 4, Name: "Mapped Repo", IndexState: "ready"}
	maps := &testMapService{snapshot: graph.Snapshot{
		Version: 1,
		ID:      "snapshot-1",
		Scope: graph.Scope{
			Kind:                 "collection",
			Complete:             false,
			TotalRepositories:    120,
			AnalyzedRepositories: 40,
			OmittedRepositories:  80,
			RepositoryLimit:      40,
		},
		Repositories: []graph.Repository{{
			ID:       repository.ID,
			Name:     repository.Name,
			Revision: strings.Repeat("a", 40),
		}},
		Nodes: []graph.Node{{
			ID:       "repository:4",
			Kind:     "repository",
			Label:    repository.Name,
			Layer:    "Repositories",
			Evidence: []graph.Evidence{{Path: "README.md", Line: 1}},
		}},
	}}
	server, err := New(
		Config{Address: "127.0.0.1:7331", Maps: maps},
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

	pageRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/maps", nil)
	pageResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(pageResponse, pageRequest)
	if pageResponse.Code != http.StatusOK {
		t.Fatalf("map page status = %d, body = %s", pageResponse.Code, pageResponse.Body.String())
	}
	for _, expected := range []string{
		`aria-current="page">Maps`,
		`data-map-canvas`,
		`data-map-inspector-content`,
		`data-map-export`,
		`data-map-repository-picker`,
		`data-map-repository-backdrop`,
		`role="listbox"`,
		`data-label="Fleet view"`,
	} {
		if !strings.Contains(pageResponse.Body.String(), expected) {
			t.Fatalf("map page does not contain %q", expected)
		}
	}

	apiRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/maps?repository=4&refresh=true", nil)
	apiResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(apiResponse, apiRequest)
	if apiResponse.Code != http.StatusOK || maps.repositoryID != 4 || !maps.refresh {
		t.Fatalf("map API status = %d, repository = %d, refresh = %v", apiResponse.Code, maps.repositoryID, maps.refresh)
	}
	for _, expected := range []string{
		`"scope":`,
		`"complete":false`,
		`"total_repositories":120`,
		`"analyzed_repositories":40`,
		`"omitted_repositories":80`,
	} {
		if !strings.Contains(apiResponse.Body.String(), expected) {
			t.Fatalf("map API body does not contain %q: %s", expected, apiResponse.Body.String())
		}
	}

	exportRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/maps/export?repository=4", nil)
	exportResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(exportResponse, exportRequest)
	if exportResponse.Code != http.StatusOK {
		t.Fatalf("map export status = %d, body = %s", exportResponse.Code, exportResponse.Body.String())
	}
	if got := exportResponse.Header().Get("Content-Disposition"); got != `attachment; filename="repokarta-map-mapped-repo.json"` {
		t.Fatalf("content disposition = %q", got)
	}
	if !strings.Contains(exportResponse.Body.String(), `"snapshot-1"`) {
		t.Fatalf("map export = %s", exportResponse.Body.String())
	}
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

func TestGitAPIRejectsInvalidBoundsBeforeRepositoryAccess(t *testing.T) {
	server, err := New(
		Config{Address: "127.0.0.1:7331"},
		codeintel.New(testStore{}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		"http://127.0.0.1:7331/api/git/log/repo?limit=201",
		"http://127.0.0.1:7331/api/git/diff/repo?context=21",
	} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		response := httptest.NewRecorder()
		server.server.Handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s returned %d: %s", target, response.Code, response.Body.String())
		}
	}
}

type testRefresher struct{}

func (testRefresher) Refresh(context.Context) error { return nil }

type testConversations struct {
	interrupted string
	lastRequest agent.TurnRequest
}

func (*testConversations) Statuses(context.Context) []agent.Status {
	return []agent.Status{{
		ID:            "test",
		Name:          "Test",
		Available:     true,
		Authenticated: true,
		ImageInput:    true,
		ImageOutput:   true,
		Interrupt:     true,
		ContextUsage:  true,
	}}
}

func (s *testConversations) Send(_ context.Context, request agent.TurnRequest, emit func(agent.Event) error) error {
	s.lastRequest = request
	if err := emit(agent.Event{Type: agent.EventMeta, ConversationID: "conversation"}); err != nil {
		return err
	}
	if err := emit(agent.Event{Type: agent.EventDelta, Text: "answer:" + request.Message}); err != nil {
		return err
	}
	if len(request.Images) > 0 {
		if err := emit(agent.Event{Type: agent.EventImages, Images: request.Images}); err != nil {
			return err
		}
	}
	if err := emit(agent.Event{
		Type: agent.EventContext,
		Context: &agent.ContextUsage{
			UsedTokens: 1200,
			MaxTokens:  200000,
			Percentage: 0.6,
			Model:      "test-model",
		},
	}); err != nil {
		return err
	}
	return emit(agent.Event{Type: agent.EventDone, ConversationID: "conversation"})
}

func (s *testConversations) Interrupt(_ context.Context, conversationID string) error {
	s.interrupted = conversationID
	return nil
}

type testHistoryConversations struct {
	testConversations
	conversation agent.Conversation
	deleted      bool
}

func (s *testHistoryConversations) ListConversations(context.Context) ([]agent.Conversation, error) {
	if s.deleted {
		return []agent.Conversation{}, nil
	}
	summary := s.conversation
	summary.Messages = nil
	return []agent.Conversation{summary}, nil
}

func (s *testHistoryConversations) GetConversation(context.Context, string) (agent.Conversation, error) {
	if s.deleted {
		return agent.Conversation{}, agent.ErrConversationNotFound
	}
	return s.conversation, nil
}

func (s *testHistoryConversations) RenameConversation(_ context.Context, _ string, title string) error {
	s.conversation.Title = strings.TrimSpace(title)
	return nil
}

func (s *testHistoryConversations) DeleteConversation(context.Context, string) error {
	s.deleted = true
	return nil
}

func TestConversationHistoryCRUDAPI(t *testing.T) {
	conversations := &testHistoryConversations{conversation: agent.Conversation{
		ID:           "saved",
		Title:        "Saved chat",
		Provider:     "test",
		ResumeCursor: "opaque-provider-cursor",
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
		MessageCount: 1,
		Messages: []agent.Message{{
			ID:             1,
			ConversationID: "saved",
			Role:           agent.RoleUser,
			Text:           "hello",
		}},
	}}
	server, err := New(
		Config{Address: "127.0.0.1:7331", Conversations: conversations},
		codeintel.New(testStore{}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/conversations", nil)
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"title":"Saved chat"`) {
		t.Fatalf("list status = %d, body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "opaque-provider-cursor") {
		t.Fatal("provider resume cursor leaked through conversation list API")
	}

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/conversations/saved", nil)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"text":"hello"`) {
		t.Fatalf("get status = %d, body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "opaque-provider-cursor") {
		t.Fatal("provider resume cursor leaked through conversation detail API")
	}

	request = httptest.NewRequest(
		http.MethodPatch,
		"http://127.0.0.1:7331/api/conversations/saved",
		bytes.NewBufferString(`{"title":"Renamed chat"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || conversations.conversation.Title != "Renamed chat" {
		t.Fatalf("rename status = %d, title = %q", response.Code, conversations.conversation.Title)
	}

	request = httptest.NewRequest(http.MethodDelete, "http://127.0.0.1:7331/api/conversations/saved", nil)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || !conversations.deleted {
		t.Fatalf("delete status = %d, deleted = %v", response.Code, conversations.deleted)
	}
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
			Conversations:  &testConversations{},
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
	for _, expected := range []string{
		`aria-current="page">Search`,
		`href="/chat"`,
		`action="/search"`,
		`data-repository-drawer`,
		`data-expanded="false"`,
		`aria-label="Open repositories"`,
		`id="repository-drawer-panel" class="repository-drawer-panel" aria-hidden="true" inert`,
	} {
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
		`id="conversation-image-input"`,
		`data-image-attach`,
		`id="conversation-runtime"`,
		`id="conversation-context-value"`,
		`id="conversation-usage-value"`,
		`id="conversation-history"`,
		`id="conversation-timeout"`,
		`id="conversation-token-budget"`,
		`id="conversation-interrupt"`,
		`id="conversation-title"`,
		`id="conversation-inspector"`,
		`id="conversation-evidence-list"`,
		`data-conversation-filter`,
		`data-mermaid-viewer-download`,
		`aria-label="Ask RepoKarta"`,
	} {
		if !strings.Contains(chatBody, expected) {
			t.Fatalf("chat page does not contain %q", expected)
		}
	}
	if strings.Contains(chatBody, `action="/search"`) {
		t.Fatal("chat page unexpectedly contains the search form")
	}
	if strings.Contains(chatBody, `data-repository-drawer`) {
		t.Fatal("chat page unexpectedly contains the global repository drawer")
	}
}

func TestChatStreamsNDJSON(t *testing.T) {
	conversations := &testConversations{}
	server, err := New(
		Config{
			Address:       "127.0.0.1:7331",
			Conversations: conversations,
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
		bytes.NewBufferString(`{"provider":"test","message":"hello","timeout_seconds":300,"token_budget":32000,"images":[{"name":"pixel.png","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="}]}`),
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
		`"type":"images"`,
		`"media_type":"image/png"`,
		`"type":"context"`,
		`"max_tokens":200000`,
		`"type":"done"`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("body does not contain %q: %s", expected, body)
		}
	}
	if conversations.lastRequest.TimeoutSeconds != 300 || conversations.lastRequest.TokenBudget != 32000 {
		t.Fatalf("turn controls = %#v", conversations.lastRequest)
	}
}

func TestChatInterruptDelegatesToConversation(t *testing.T) {
	conversations := &testConversations{}
	server, err := New(
		Config{
			Address:       "127.0.0.1:7331",
			Conversations: conversations,
		},
		codeintel.New(testStore{}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:7331/api/chat/conversation/interrupt",
		nil,
	)
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if conversations.interrupted != "conversation" {
		t.Fatalf("interrupted conversation = %q", conversations.interrupted)
	}
}

func TestChatRejectsInvalidImageBeforeStreaming(t *testing.T) {
	server, err := New(
		Config{
			Address:       "127.0.0.1:7331",
			Conversations: &testConversations{},
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
		bytes.NewBufferString(`{"provider":"test","images":[{"name":"fake.png","media_type":"image/png","data":"aGVsbG8="}]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "does not match file content") {
		t.Fatalf("unexpected body: %s", response.Body.String())
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
