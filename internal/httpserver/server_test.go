package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/acquisition"
	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/audit"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/codeintel"
	"github.com/spolnik/RepoKarta/internal/contextscope"
	"github.com/spolnik/RepoKarta/internal/docs"
	"github.com/spolnik/RepoKarta/internal/graph"
	"github.com/spolnik/RepoKarta/internal/maintenance"
	"github.com/spolnik/RepoKarta/internal/search"
	"github.com/spolnik/RepoKarta/internal/security"
	"github.com/spolnik/RepoKarta/internal/store"
)

func TestReaderPermissionDenialIsAudited(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "repokarta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	securityManager, err := security.New(context.Background(), database, security.Config{
		Address:       "0.0.0.0:7331",
		DataDirectory: t.TempDir(),
		AllowOpen:     true,
		AdminUser:     "admin",
		AdminPassword: "reader-test-password",
		Initial: security.Settings{
			Mode: security.ModeOpen, PublicURL: "https://repo.example.com",
		},
		Audit: database,
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{
		Address: "0.0.0.0:7331", Version: "test",
		Security: securityManager, Enterprise: database,
	}, codeintel.New(testStore{}, testSearcher{}, "https://repo.example.com"), testRefresher{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "https://repo.example.com/repositories/refresh", nil)
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("reader refresh status = %d, body = %q", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "https://repo.example.com/api/whoami", nil)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"role":"reader"`) {
		t.Fatalf("whoami status = %d, body = %q", response.Code, response.Body.String())
	}
	page, err := database.AuditEvents(context.Background(), audit.Filter{Action: "authorization.denied", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].Outcome != "denied" ||
		page.Events[0].Metadata["permission"] != "repositories.acquire" {
		t.Fatalf("denial audit = %#v", page.Events)
	}
}

func TestAdministratorCanPreviewCleanupAndExportDiagnostics(t *testing.T) {
	root := t.TempDir()
	dataDirectory := filepath.Join(root, "data")
	repositoryRoot := filepath.Join(root, "repositories")
	if err := os.MkdirAll(filepath.Join(dataDirectory, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repositoryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dataDirectory, "logs", "repokarta.log")
	if err := os.WriteFile(logPath, []byte("bounded diagnostic log"), 0o600); err != nil {
		t.Fatal(err)
	}
	operationsStore := maintenanceTestStore{}
	accessStore := &accessTestStore{policies: []store.RepositoryAccess{{
		RepositoryID:   7,
		Repository:     "private-repository",
		RepositoryPath: `C:\repositories\private-repository`,
		OwnerID:        "local:admin",
		Visibility:     "private",
	}}}
	acquisitionStore := &acquisitionTestService{}
	operations, err := maintenance.New(maintenance.Config{
		DataDirectory:   dataDirectory,
		RepositoryRoot:  repositoryRoot,
		Version:         "test",
		Address:         "127.0.0.1:7331",
		DatabaseVersion: 8,
		MapVersion:      4,
		WikiVersion:     3,
	}, operationsStore)
	if err != nil {
		t.Fatal(err)
	}
	settingsStore := &testSettingsStore{values: make(map[string]string)}
	securityManager, err := security.New(context.Background(), settingsStore, security.Config{
		Address:       "127.0.0.1:7331",
		DataDirectory: dataDirectory,
		AdminUser:     "admin",
		AdminPassword: "maintenance-password",
		Initial:       security.Settings{Mode: security.ModeLocal},
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(
		Config{
			Address:               "127.0.0.1:7331",
			Version:               "test",
			Security:              securityManager,
			Maintenance:           operations,
			RepositoryAccess:      accessStore,
			RepositoryAcquisition: acquisitionStore,
		},
		codeintel.New(testStore{}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}

	loginForm := url.Values{"username": {"admin"}, "password": {"maintenance-password"}}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/admin/login", strings.NewReader(loginForm.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("login response = %d, body %q", response.Code, response.Body.String())
	}
	adminCookie := response.Result().Cookies()[0]

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/admin", nil)
	request.AddCookie(adminCookie)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "Storage and diagnostics") ||
		!strings.Contains(response.Body.String(), "Repository acquisition") ||
		!strings.Contains(response.Body.String(), "private-repository") ||
		!strings.Contains(response.Body.String(), "logs/repokarta.log") {
		t.Fatalf("admin storage response = %d, body %q", response.Code, response.Body.String())
	}
	csrfMatch := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(response.Body.String())
	targetMatch := regexp.MustCompile(`name="target" value="([^"]+)"`).FindStringSubmatch(response.Body.String())
	if len(csrfMatch) != 2 || len(targetMatch) != 2 {
		t.Fatalf("storage controls missing: %q", response.Body.String())
	}

	discoveryForm := url.Values{
		"csrf":            {csrfMatch[1]},
		"provider":        {"github"},
		"location":        {"acme"},
		"include_private": {"true"},
		"team":            {"platform"},
	}
	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/admin/repositories/discover", strings.NewReader(discoveryForm.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(adminCookie)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "github.com/acme/preview-example") ||
		acquisitionStore.lastDiscovery.Team != "platform" {
		t.Fatalf("repository discovery response = %d, discovery = %#v, body %q", response.Code, acquisitionStore.lastDiscovery, response.Body.String())
	}

	acquireForm := url.Values{
		"csrf":                   {csrfMatch[1]},
		"provider":               {"github"},
		"provider_repository_id": {"42"},
		"canonical_id":           {"github.com/acme/preview-example"},
		"name":                   {"preview-example"},
		"namespace":              {"acme"},
		"remote_url":             {"https://github.com/acme/preview-example.git"},
		"default_branch":         {"main"},
		"visibility":             {"private"},
		"inclusion_policy":       {"approved; team=platform"},
	}
	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/admin/repositories/acquire", strings.NewReader(acquireForm.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(adminCookie)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || len(acquisitionStore.repositories) != 1 ||
		!strings.Contains(response.Body.String(), "queued for commit-pinned indexing") {
		t.Fatalf("repository acquisition response = %d, repositories = %#v, body %q", response.Code, acquisitionStore.repositories, response.Body.String())
	}

	accessForm := url.Values{
		"csrf":          {csrfMatch[1]},
		"repository_id": {"7"},
		"owner_id":      {"saml:owner"},
		"visibility":    {"private"},
		"users":         {"saml:alice"},
		"groups":        {"engineering"},
	}
	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/admin/repositories/access", strings.NewReader(accessForm.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(adminCookie)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		accessStore.policies[0].OwnerID != "saml:owner" ||
		len(accessStore.policies[0].Groups) != 1 {
		t.Fatalf("repository access response = %d, policy = %#v", response.Code, accessStore.policies)
	}

	previewForm := url.Values{"csrf": {csrfMatch[1]}, "target": {targetMatch[1]}}
	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/admin/storage/preview", strings.NewReader(previewForm.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(adminCookie)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Cleanup preview") {
		t.Fatalf("preview response = %d, body %q", response.Code, response.Body.String())
	}
	tokenMatch := regexp.MustCompile(`name="plan_token" value="([^"]+)"`).FindStringSubmatch(response.Body.String())
	if len(tokenMatch) != 2 {
		t.Fatalf("cleanup token missing: %q", response.Body.String())
	}

	cleanupForm := url.Values{
		"csrf":       {csrfMatch[1]},
		"target":     {targetMatch[1]},
		"plan_token": {tokenMatch[1]},
		"confirm":    {"remove"},
	}
	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/admin/storage/cleanup", strings.NewReader(cleanupForm.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(adminCookie)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Cleanup completed") {
		t.Fatalf("cleanup response = %d, body %q", response.Code, response.Body.String())
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("cleaned log still exists: %v", err)
	}

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/admin/diagnostics", nil)
	request.AddCookie(adminCookie)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		response.Header().Get("Content-Type") != "application/zip" ||
		response.Body.Len() == 0 {
		t.Fatalf("diagnostics response = %d, type %q, bytes %d", response.Code, response.Header().Get("Content-Type"), response.Body.Len())
	}
}

type maintenanceTestStore struct{}

type accessTestStore struct {
	policies []store.RepositoryAccess
}

type acquisitionTestService struct {
	lastDiscovery acquisition.DiscoverRequest
	repositories  []acquisition.Repository
}

func (s *acquisitionTestService) List(context.Context) ([]acquisition.Repository, error) {
	return append([]acquisition.Repository(nil), s.repositories...), nil
}

func (s *acquisitionTestService) Discover(_ context.Context, request acquisition.DiscoverRequest) ([]acquisition.Candidate, error) {
	s.lastDiscovery = request
	return []acquisition.Candidate{{
		Provider:             acquisition.ProviderGitHub,
		ProviderRepositoryID: "42",
		CanonicalID:          "github.com/acme/preview-example",
		Name:                 "preview-example",
		Namespace:            "acme",
		RemoteURL:            "https://github.com/acme/preview-example.git",
		DefaultBranch:        "main",
		Visibility:           "private",
		InclusionPolicy:      "approved; team=platform",
	}}, nil
}

func (s *acquisitionTestService) Acquire(_ context.Context, candidate acquisition.Candidate, _ string) (acquisition.Repository, error) {
	repository := acquisition.Repository{
		ID:                   1,
		Provider:             candidate.Provider,
		ProviderRepositoryID: candidate.ProviderRepositoryID,
		CanonicalID:          candidate.CanonicalID,
		Name:                 candidate.Name,
		Namespace:            candidate.Namespace,
		RemoteURL:            candidate.RemoteURL,
		CheckoutPath:         `C:\RepoKarta\repositories\github\acme\preview-example`,
		DefaultBranch:        candidate.DefaultBranch,
		InclusionPolicy:      candidate.InclusionPolicy,
		Owned:                true,
		State:                acquisition.StateReady,
		HeadCommit:           strings.Repeat("a", 40),
	}
	s.repositories = []acquisition.Repository{repository}
	return repository, nil
}

func (s *acquisitionTestService) Sync(context.Context, int64) (acquisition.Repository, error) {
	return s.repositories[0], nil
}

func (s *acquisitionTestService) Remove(context.Context, int64) (string, error) {
	s.repositories = nil
	return `C:\RepoKarta\repository-trash\preview-example`, nil
}

func (s *accessTestStore) ListRepositoryAccess(context.Context) ([]store.RepositoryAccess, error) {
	return append([]store.RepositoryAccess(nil), s.policies...), nil
}

func (s *accessTestStore) SetRepositoryAccess(_ context.Context, policy store.RepositoryAccess) error {
	for index := range s.policies {
		if s.policies[index].RepositoryID == policy.RepositoryID {
			policy.Repository = s.policies[index].Repository
			policy.RepositoryPath = s.policies[index].RepositoryPath
			s.policies[index] = policy
			return nil
		}
	}
	return context.Canceled
}

func (maintenanceTestStore) ListRepositories(context.Context) ([]catalog.Repository, error) {
	return []catalog.Repository{}, nil
}

func (maintenanceTestStore) ConversationImagePaths(context.Context) (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

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
	request = httptest.NewRequest(http.MethodGet, "https://repo.example.com/api/whoami", nil)
	request.Host = "repo.example.com"
	request.Header.Set("Origin", "https://repo.example.com")
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"id":"open:anonymous"`) ||
		!strings.Contains(response.Body.String(), `"groups":[]`) {
		t.Fatalf("open whoami response = %d, body %q", response.Code, response.Body.String())
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
		`find_references`,
		`read_dependency_inventory`,
		`list_deep_wiki_pages`,
		`read_generated_document`,
		`14 tools · no writes`,
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

func TestDependencyWorkspaceAndAPIExposeNormalizedDeclarations(t *testing.T) {
	repository := catalog.Repository{
		ID:         4,
		Name:       "acme/service",
		IndexState: "ready",
	}
	maps := &testMapService{snapshot: graph.Snapshot{
		Scope: graph.Scope{
			Kind:                 "repository",
			Complete:             true,
			TotalRepositories:    1,
			AnalyzedRepositories: 1,
		},
		Repositories: []graph.Repository{{
			ID:       repository.ID,
			Name:     repository.Name,
			Revision: strings.Repeat("a", 40),
		}},
		Manifests: []graph.Manifest{{
			RepositoryID: repository.ID,
			Repository:   repository.Name,
			Kind:         "npm package",
			Path:         "web/package.json",
			Declarations: []graph.DependencyDeclaration{{
				Ecosystem:  "npm",
				Package:    "marked",
				Declared:   "^16.4.1",
				Resolution: "constraint",
				Evidence: graph.Evidence{
					RepositoryID: repository.ID,
					Repository:   repository.Name,
					Revision:     strings.Repeat("a", 40),
					Path:         "web/package.json",
					Line:         14,
					URL:          "http://127.0.0.1:7331/source/4#L14",
				},
			}},
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

	pageRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/dependencies?repository=4", nil)
	pageResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(pageResponse, pageRequest)
	if pageResponse.Code != http.StatusOK || maps.repositoryID != 4 {
		t.Fatalf("dependency page status = %d, repository = %d", pageResponse.Code, maps.repositoryID)
	}
	for _, expected := range []string{
		`aria-current="page">Dependencies`,
		`Dependency management`,
		`marked`,
		`^16.4.1`,
		`Not checked`,
		`web/package.json:14`,
	} {
		if !strings.Contains(pageResponse.Body.String(), expected) {
			t.Fatalf("dependency page does not contain %q: %s", expected, pageResponse.Body.String())
		}
	}

	apiRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/dependencies?repository=4", nil)
	apiResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(apiResponse, apiRequest)
	if apiResponse.Code != http.StatusOK {
		t.Fatalf("dependency API status = %d, body = %s", apiResponse.Code, apiResponse.Body.String())
	}
	for _, expected := range []string{
		`"dependency_count":1`,
		`"ecosystem":"npm"`,
		`"package":"marked"`,
		`"declared":"^16.4.1"`,
		`"resolution":"constraint"`,
		`"check_status":"unchecked"`,
	} {
		if !strings.Contains(apiResponse.Body.String(), expected) {
			t.Fatalf("dependency API does not contain %q: %s", expected, apiResponse.Body.String())
		}
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

type pendingReferenceStructure struct{}

func (pendingReferenceStructure) ReadStructure(context.Context, int64) (graph.StructuralIndex, error) {
	return graph.StructuralIndex{Scope: graph.Scope{
		Kind:                 "collection",
		Complete:             false,
		TotalRepositories:    3,
		AnalyzedRepositories: 1,
		OmittedRepositories:  2,
	}}, nil
}

func TestAPIReferenceSearchReturnsAcceptedWithIndexProgress(t *testing.T) {
	intelligence := codeintel.New(testStore{}, testSearcher{}, "http://127.0.0.1:7331").
		UseStructure(pendingReferenceStructure{})
	server, err := New(
		Config{Address: "127.0.0.1:7331"},
		intelligence,
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:7331/api/search?q=JobTimeGuard&mode=references",
		nil,
	)
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || response.Header().Get("Retry-After") != "2" {
		t.Fatalf("status = %d, retry = %q, body = %s", response.Code, response.Header().Get("Retry-After"), response.Body.String())
	}
	var result codeintel.SearchResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ReferenceIndex == nil || result.ReferenceIndex.State != "building" ||
		result.ReferenceIndex.ReadyRepositories != 1 || result.ReferenceIndex.PendingRepositories != 2 {
		t.Fatalf("reference progress = %#v", result.ReferenceIndex)
	}
}

func TestStructuredContextsFlowThroughJSONSearchAndChat(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runHTTPGit(t, directory, "init", "-q")
	runHTTPGit(t, directory, "add", ".")
	runHTTPGit(t, directory, "-c", "user.name=RepoKarta Test", "-c", "user.email=test@repokarta.local", "commit", "-qm", "fixture")
	revision := strings.TrimSpace(runHTTPGit(t, directory, "rev-parse", "HEAD"))
	repository := catalog.Repository{
		ID: 42, Name: "fixture", Path: directory, HeadCommit: revision,
		IndexedCommit: revision, IndexState: "ready",
	}
	conversations := &testConversations{}
	server, err := New(
		Config{Address: "127.0.0.1:7331", Conversations: conversations},
		codeintel.New(testStore{repositories: []catalog.Repository{repository}}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}
	selector := contextscope.Selector{
		Kind: contextscope.KindFile, RepositoryID: repository.ID,
		Revision: revision, Path: "main.go",
	}
	searchBody, err := json.Marshal(codeintel.SearchRequest{
		Query: "package", Contexts: []contextscope.Selector{selector},
	})
	if err != nil {
		t.Fatal(err)
	}
	searchRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/api/search", bytes.NewReader(searchBody))
	searchResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(searchResponse, searchRequest)
	if searchResponse.Code != http.StatusOK {
		t.Fatalf("structured search status = %d, body = %s", searchResponse.Code, searchResponse.Body.String())
	}
	var searchResult codeintel.SearchResponse
	if err := json.Unmarshal(searchResponse.Body.Bytes(), &searchResult); err != nil {
		t.Fatal(err)
	}
	if len(searchResult.Contexts) != 1 || searchResult.Contexts[0].Path != "main.go" {
		t.Fatalf("structured search contexts = %#v", searchResult.Contexts)
	}
	staleSelector := selector
	staleSelector.Revision = strings.Repeat("b", 40)
	staleBody, _ := json.Marshal(codeintel.SearchRequest{
		Query: "package", Contexts: []contextscope.Selector{staleSelector},
	})
	staleRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/api/search", bytes.NewReader(staleBody))
	staleResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(staleResponse, staleRequest)
	if staleResponse.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(staleResponse.Body.String(), `"code":"stale"`) {
		t.Fatalf("stale context response = %d, body = %s", staleResponse.Code, staleResponse.Body.String())
	}

	chatBody, err := json.Marshal(map[string]any{
		"provider": "test", "message": "inspect startup", "contexts": []contextscope.Selector{selector},
	})
	if err != nil {
		t.Fatal(err)
	}
	chatRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/api/chat", bytes.NewReader(chatBody))
	chatResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(chatResponse, chatRequest)
	if chatResponse.Code != http.StatusOK {
		t.Fatalf("structured chat status = %d, body = %s", chatResponse.Code, chatResponse.Body.String())
	}
	if len(conversations.lastRequest.Contexts) != 1 ||
		conversations.lastRequest.Contexts[0].Revision != revision ||
		conversations.lastRequest.Contexts[0].Label != "@fixture:main.go" {
		t.Fatalf("chat contexts = %#v", conversations.lastRequest.Contexts)
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

func runHTTPGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return string(output)
}

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
	lastFilter   agent.ConversationFilter
}

func (s *testHistoryConversations) ListConversations(_ context.Context, filter agent.ConversationFilter) ([]agent.Conversation, error) {
	s.lastFilter = filter
	if s.deleted {
		return []agent.Conversation{}, nil
	}
	if !filter.All && s.conversation.Author.ID != filter.AuthorID {
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
		ID:       "saved",
		Title:    "Saved chat",
		Provider: "test",
		Author: agent.ConversationAuthor{
			ID:       "local:admin",
			Name:     "Local administrator",
			Provider: "local",
		},
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

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/conversations?scope=all", nil)
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"title":"Saved chat"`) {
		t.Fatalf("list status = %d, body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "opaque-provider-cursor") {
		t.Fatal("provider resume cursor leaked through conversation list API")
	}
	if !conversations.lastFilter.All ||
		!strings.Contains(response.Body.String(), `"can_view_all":true`) ||
		!strings.Contains(response.Body.String(), `"scope":"all"`) ||
		!strings.Contains(response.Body.String(), `"name":"Local administrator"`) {
		t.Fatalf("administrator conversation scope was not exposed correctly: %s", response.Body.String())
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

func TestSharedNonAdminCanOnlyAccessOwnConversations(t *testing.T) {
	settingsStore := &testSettingsStore{values: make(map[string]string)}
	securityManager, err := security.New(context.Background(), settingsStore, security.Config{
		Address:       "127.0.0.1:7331",
		DataDirectory: t.TempDir(),
		AllowOpen:     true,
		AdminUser:     "bootstrap-admin",
		AdminPassword: "correct horse battery staple",
		Initial: security.Settings{
			Mode:      security.ModeOpen,
			PublicURL: "https://repo.example.com",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	conversations := &testHistoryConversations{conversation: agent.Conversation{
		ID:       "alice-chat",
		Title:    "Alice's chat",
		Provider: "test",
		Author: agent.ConversationAuthor{
			ID:       "saml:alice",
			Name:     "Alice",
			Provider: "saml",
		},
	}}
	server, err := New(
		Config{
			Address:       "127.0.0.1:7331",
			Conversations: conversations,
			Security:      securityManager,
		},
		codeintel.New(testStore{}, testSearcher{}, "https://repo.example.com"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}

	listRequest := httptest.NewRequest(http.MethodGet, "https://repo.example.com/api/conversations?scope=all", nil)
	listRequest.Host = "repo.example.com"
	listResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(listResponse, listRequest)
	if listResponse.Code != http.StatusOK ||
		!strings.Contains(listResponse.Body.String(), `"conversations":[]`) ||
		!strings.Contains(listResponse.Body.String(), `"can_view_all":false`) ||
		conversations.lastFilter.All ||
		conversations.lastFilter.AuthorID != "open:anonymous" {
		t.Fatalf("non-admin list status = %d, body = %s, filter = %#v",
			listResponse.Code, listResponse.Body.String(), conversations.lastFilter)
	}

	getRequest := httptest.NewRequest(http.MethodGet, "https://repo.example.com/api/conversations/alice-chat", nil)
	getRequest.Host = "repo.example.com"
	getResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusForbidden {
		t.Fatalf("cross-author get status = %d, body = %s", getResponse.Code, getResponse.Body.String())
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
		`<option value="references"`,
		`>AST references</option>`,
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
		`id="conversation-contexts"`,
		`id="conversation-context-suggestions"`,
		`data-context-add`,
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
		`data-conversation-scope="own"`,
		`data-conversation-scope="all"`,
		`data-conversation-author-filter`,
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
