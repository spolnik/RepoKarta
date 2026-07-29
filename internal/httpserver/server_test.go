package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/acquisition"
	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/analysis"
	"github.com/spolnik/RepoKarta/internal/audit"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/codeintel"
	"github.com/spolnik/RepoKarta/internal/contextscope"
	"github.com/spolnik/RepoKarta/internal/dependencies"
	"github.com/spolnik/RepoKarta/internal/docs"
	"github.com/spolnik/RepoKarta/internal/graph"
	"github.com/spolnik/RepoKarta/internal/identity"
	"github.com/spolnik/RepoKarta/internal/maintenance"
	"github.com/spolnik/RepoKarta/internal/scipjava"
	"github.com/spolnik/RepoKarta/internal/search"
	"github.com/spolnik/RepoKarta/internal/security"
	"github.com/spolnik/RepoKarta/internal/source"
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

func TestNamedContextJSONAndCanonicalPageExposeEffectiveScope(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "repokarta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	revision := "0123456789012345678901234567890123456789"
	if err := database.ReplaceRepositories(t.Context(), []catalog.Repository{{
		Name:          "payments",
		Path:          t.TempDir(),
		ScanState:     "ready",
		IndexState:    "ready",
		HeadCommit:    revision,
		IndexedCommit: revision,
	}}); err != nil {
		t.Fatal(err)
	}
	repositories, err := database.ListRepositories(t.Context())
	if err != nil || len(repositories) != 1 {
		t.Fatalf("repositories = %#v, %v", repositories, err)
	}
	repositoryID := repositories[0].ID
	if err := database.UpdateIndexState(t.Context(), repositoryID, "ready", revision, ""); err != nil {
		t.Fatal(err)
	}
	intelligence := codeintel.New(database, testSearcher{}, "https://repo.example.com").
		UseNamedContexts(database)
	server, err := New(
		Config{Address: "127.0.0.1:7331", Version: "test"},
		intelligence,
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}
	input := contextscope.NamedContextInput{
		Title:        "Payments release",
		Description:  "Pinned release repositories",
		Category:     contextscope.CategoryRelease,
		Visibility:   contextscope.VisibilityShared,
		DefaultScope: contextscope.DefaultAdministrator,
		Selectors: []contextscope.Selector{{
			Kind: contextscope.KindRepository, RepositoryID: repositoryID, Revision: revision,
		}},
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/api/contexts/named", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", response.Code, response.Body.String())
	}
	var created contextscope.NamedContext
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.State != "ready" || len(created.Contexts) != 1 ||
		created.Contexts[0].URL == "" || created.URL == "" {
		t.Fatalf("created context = %#v", created)
	}

	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/api/contexts/resolve", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"administrator_default"`) ||
		!strings.Contains(response.Body.String(), `"https://repo.example.com/contexts?`) {
		t.Fatalf("effective response status = %d, body = %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/contexts/"+created.ID, nil)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "Payments release") ||
		!strings.Contains(response.Body.String(), "Copy context URL") {
		t.Fatalf("context page status = %d, body = %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:7331/api/search/query-completions?q=repository%3Apay&cursor=14",
		nil,
	)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"insert_text":"repository:payments"`) {
		t.Fatalf("query completion status = %d, body = %s", response.Code, response.Body.String())
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

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/admin?section=storage", nil)
	request.AddCookie(adminCookie)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "Storage and diagnostics") ||
		!strings.Contains(response.Body.String(), "logs/repokarta.log") {
		t.Fatalf("admin storage response = %d, body %q", response.Code, response.Body.String())
	}
	csrfMatch := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(response.Body.String())
	targetMatch := regexp.MustCompile(`name="target" value="([^"]+)"`).FindStringSubmatch(response.Body.String())
	if len(csrfMatch) != 2 || len(targetMatch) != 2 {
		t.Fatalf("storage controls missing: %q", response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/admin?section=repositories", nil)
	request.AddCookie(adminCookie)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "Repository acquisition") {
		t.Fatalf("admin repositories response = %d, body %q", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/admin?section=access", nil)
	request.AddCookie(adminCookie)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "Repository and artifact access") ||
		!strings.Contains(response.Body.String(), "private-repository") {
		t.Fatalf("admin access response = %d, body %q", response.Code, response.Body.String())
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
		"code_enabled":  {"enabled"},
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
		!accessStore.policies[0].CodeEnabled ||
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

type testSCIPJavaService struct {
	retried int64
}

func (service *testSCIPJavaService) ProviderStatus() scipjava.ProviderStatus {
	return scipjava.ProviderStatus{
		Mode: "auto", Enabled: true, Available: true,
		Command: "scip-java", Version: "v-test", Configuration: "fixture",
	}
}

func (service *testSCIPJavaService) Retry(_ context.Context, repositoryID int64) error {
	service.retried = repositoryID
	return nil
}

func TestJavaSCIPStatusAndRetryAPI(t *testing.T) {
	javaSCIP := &testSCIPJavaService{}
	repository := catalog.Repository{
		ID: 7, Name: "payments", IndexState: "ready", IndexedCommit: "abc123",
		SCIPJava: &catalog.SCIPIndexStatus{
			Provider: "scip-java", State: "failed", Applicable: true,
			Revision:            "abc123",
			GradleVersion:       "8.4",
			RequestedJDKVersion: 21,
			JDKVersion:          17,
			JDKSource:           "compatible-configured",
			FailureCategory:     scipjava.FailureJDKIncompatibleWrapper,
			FailureSummary:      "The selected JDK cannot run this repository's Gradle wrapper.",
			Error:               "fixture failure",
		},
	}
	server, err := New(
		Config{
			Address: "127.0.0.1:7331", Version: "test", SCIPJava: javaSCIP,
		},
		codeintel.New(testStore{repositories: []catalog.Repository{repository}}, testSearcher{}, ""),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/scip/java", nil)
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"available":true`) ||
		!strings.Contains(response.Body.String(), `"version":"v-test"`) {
		t.Fatalf("provider response = %d, %q", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/repositories", nil)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"failure_category":"jdk_incompatible_wrapper"`) ||
		!strings.Contains(response.Body.String(), `"failure_summary":"The selected JDK cannot run`) ||
		!strings.Contains(response.Body.String(), `"requested_jdk_version":21`) ||
		!strings.Contains(response.Body.String(), `"jdk_version":17`) {
		t.Fatalf("repository SCIP response = %d, %q", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/api/scip/java/retry/7", nil)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || javaSCIP.retried != 7 {
		t.Fatalf("retry response = %d, retried = %d, body = %q", response.Code, javaSCIP.retried, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/", nil)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "Java SCIP") ||
		!strings.Contains(response.Body.String(), "JDK / Gradle compatibility") ||
		!strings.Contains(response.Body.String(), "The selected JDK cannot run") {
		t.Fatalf("home Java SCIP status = %d, %q", response.Code, response.Body.String())
	}
}

func TestEnterpriseAdministrationAPILifecycle(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "repokarta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	user, err := database.SaveUser(context.Background(), identity.User{
		UserName: "alice@example.com", Email: "alice@example.com", Active: true,
		Role: identity.RoleReader,
	})
	if err != nil {
		t.Fatal(err)
	}
	group, err := database.SaveGroup(context.Background(), identity.Group{
		DisplayName: "Developers", Role: identity.RoleReader, Members: []string{user.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	securityManager, err := security.New(context.Background(), database, security.Config{
		Address: "127.0.0.1:7331", DataDirectory: t.TempDir(),
		Initial: security.Settings{Mode: security.ModeLocal},
		Audit:   database,
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(
		Config{
			Address: "127.0.0.1:7331", Version: "test",
			Security: securityManager, Enterprise: database,
		},
		codeintel.New(testStore{}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, target, body string) *httptest.ResponseRecorder {
		t.Helper()
		httpRequest := httptest.NewRequest(method, "http://127.0.0.1:7331"+target, strings.NewReader(body))
		httpRequest.Header.Set("X-Request-ID", "enterprise-test")
		if body != "" {
			httpRequest.Header.Set("Content-Type", "application/json")
		}
		response := httptest.NewRecorder()
		server.server.Handler.ServeHTTP(response, httpRequest)
		return response
	}

	response := request(http.MethodGet, "/api/admin/identities", "")
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "alice@example.com") ||
		!strings.Contains(response.Body.String(), "Developers") {
		t.Fatalf("identities = %d: %s", response.Code, response.Body.String())
	}
	response = request(http.MethodPatch, "/api/admin/identities/"+user.ID+"/role", `{"role":"developer"}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "developer") {
		t.Fatalf("user role = %d: %s", response.Code, response.Body.String())
	}
	response = request(http.MethodPatch, "/api/admin/groups/"+group.ID+"/role", `{"role":"administrator"}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "administrator") {
		t.Fatalf("group role = %d: %s", response.Code, response.Body.String())
	}
	response = request(http.MethodPost, "/api/admin/role-mappings", `{"provider":"saml","group":"platform","role":"administrator"}`)
	if response.Code != http.StatusNoContent {
		t.Fatalf("create mapping = %d: %s", response.Code, response.Body.String())
	}
	response = request(http.MethodGet, "/api/admin/role-mappings", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"group":"platform"`) {
		t.Fatalf("list mappings = %d: %s", response.Code, response.Body.String())
	}
	mappings, err := database.ListRoleMappings(context.Background())
	if err != nil || len(mappings) != 1 {
		t.Fatalf("stored mappings = %#v, %v", mappings, err)
	}
	response = request(http.MethodDelete, "/api/admin/role-mappings/"+strconv.FormatInt(mappings[0].ID, 10), "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete mapping = %d: %s", response.Code, response.Body.String())
	}

	response = request(http.MethodGet, "/api/admin/security", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"mode":"local"`) {
		t.Fatalf("security settings = %d: %s", response.Code, response.Body.String())
	}
	response = request(http.MethodPut, "/api/admin/security", `{"mode":"local"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("update security = %d: %s", response.Code, response.Body.String())
	}
	response = request(http.MethodPut, "/api/admin/audit/retention", `{"days":30,"max_events":10000}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"removed_events"`) {
		t.Fatalf("update retention = %d: %s", response.Code, response.Body.String())
	}
	response = request(http.MethodGet, "/api/admin/audit/retention", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"days":30`) {
		t.Fatalf("audit retention = %d: %s", response.Code, response.Body.String())
	}
	response = request(http.MethodGet, "/api/admin/audit?limit=20&action=role.user.assign", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"events"`) {
		t.Fatalf("audit search = %d: %s", response.Code, response.Body.String())
	}
	for _, target := range []string{"/api/admin/audit/export", "/api/admin/audit/export?format=csv"} {
		response = request(http.MethodGet, target, "")
		if response.Code != http.StatusOK ||
			response.Header().Get("Content-Disposition") == "" ||
			response.Header().Get("X-RepoKarta-Audit-Export-Truncated") != "false" {
			t.Fatalf("audit export %s = %d: %s", target, response.Code, response.Body.String())
		}
	}

	for _, target := range []string{
		"/api/admin/audit?limit=nope",
		"/api/admin/audit?before=nope",
		"/api/admin/audit?since=yesterday",
	} {
		response = request(http.MethodGet, target, "")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid audit filter %s = %d: %s", target, response.Code, response.Body.String())
		}
	}
}

func TestBootstrapAdministratorFormLifecycle(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "repokarta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	user, err := database.SaveUser(context.Background(), identity.User{
		UserName: "form-user@example.com", Active: true, Role: identity.RoleReader,
	})
	if err != nil {
		t.Fatal(err)
	}
	group, err := database.SaveGroup(context.Background(), identity.Group{
		DisplayName: "Form group", Role: identity.RoleReader, Members: []string{user.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	securityManager, err := security.New(context.Background(), database, security.Config{
		Address: "127.0.0.1:7331", DataDirectory: t.TempDir(),
		Initial: security.Settings{Mode: security.ModeLocal}, Audit: database,
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(
		Config{
			Address: "127.0.0.1:7331", Version: "test",
			Security: securityManager, Enterprise: database,
		},
		codeintel.New(testStore{}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}
	pageRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/admin", nil)
	pageResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(pageResponse, pageRequest)
	if pageResponse.Code != http.StatusOK || len(pageResponse.Result().Cookies()) == 0 {
		t.Fatalf("admin page = %d: %s", pageResponse.Code, pageResponse.Body.String())
	}
	adminCookie := pageResponse.Result().Cookies()[0]
	match := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(pageResponse.Body.String())
	if len(match) != 2 {
		t.Fatalf("admin CSRF token missing: %s", pageResponse.Body.String())
	}
	csrf := match[1]
	postForm := func(target string, values url.Values) *httptest.ResponseRecorder {
		t.Helper()
		values.Set("csrf", csrf)
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331"+target, strings.NewReader(values.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.AddCookie(adminCookie)
		response := httptest.NewRecorder()
		server.server.Handler.ServeHTTP(response, request)
		return response
	}
	for _, testCase := range []struct {
		target   string
		values   url.Values
		contains string
	}{
		{"/admin/identities/role", url.Values{"user_id": {user.ID}, "role": {"developer"}}, "User role saved"},
		{"/admin/groups/role", url.Values{"group_id": {group.ID}, "role": {"administrator"}}, "SCIM group role saved"},
		{"/admin/role-mappings", url.Values{"provider": {"saml"}, "group": {"operators"}, "role": {"administrator"}}, "group mapping saved"},
		{"/admin/audit/retention", url.Values{"days": {"14"}, "max_events": {"5000"}}, "Audit retention saved"},
	} {
		response := postForm(testCase.target, testCase.values)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), testCase.contains) {
			t.Fatalf("%s = %d: %s", testCase.target, response.Code, response.Body.String())
		}
	}
	mappings, err := database.ListRoleMappings(context.Background())
	if err != nil || len(mappings) != 1 {
		t.Fatalf("mappings = %#v, %v", mappings, err)
	}
	response := postForm("/admin/role-mappings/delete", url.Values{
		"mapping_id": {strconv.FormatInt(mappings[0].ID, 10)},
	})
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "mapping removed") {
		t.Fatalf("delete mapping = %d: %s", response.Code, response.Body.String())
	}

	exportRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/admin/audit/export?format=csv", nil)
	exportRequest.AddCookie(adminCookie)
	exportResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(exportResponse, exportRequest)
	if exportResponse.Code != http.StatusOK || !strings.Contains(exportResponse.Body.String(), "correlation_id") {
		t.Fatalf("bootstrap export = %d: %s", exportResponse.Code, exportResponse.Body.String())
	}
	logoutResponse := postForm("/admin/logout", url.Values{})
	if logoutResponse.Code != http.StatusSeeOther {
		t.Fatalf("admin logout = %d: %s", logoutResponse.Code, logoutResponse.Body.String())
	}
	authLogout := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/auth/logout", nil)
	authLogoutResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(authLogoutResponse, authLogout)
	if authLogoutResponse.Code != http.StatusSeeOther {
		t.Fatalf("auth logout = %d: %s", authLogoutResponse.Code, authLogoutResponse.Body.String())
	}
}

func TestLocalAdministratorOpensAdminConsoleWithoutBootstrapCredentials(t *testing.T) {
	settingsStore := &testSettingsStore{values: make(map[string]string)}
	securityManager, err := security.New(context.Background(), settingsStore, security.Config{
		Address:       "127.0.0.1:7331",
		DataDirectory: t.TempDir(),
		Initial:       security.Settings{Mode: security.ModeLocal},
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(
		Config{Address: "127.0.0.1:7331", Version: "test", Security: securityManager},
		codeintel.New(testStore{}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/admin", nil)
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || len(response.Result().Cookies()) != 1 ||
		strings.Contains(response.Body.String(), security.ErrAdminUnavailable.Error()) ||
		!strings.Contains(response.Body.String(), "Authentication") {
		t.Fatalf("default local admin status = %d, cookies = %d, body = %s",
			response.Code, len(response.Result().Cookies()), response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/", nil)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `href="/admin"`) {
		t.Fatalf("local administrator navigation status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestEmptyRepositoryIsTerminalAndExcludedFromIndexProgress(t *testing.T) {
	repositories := []catalog.Repository{
		{
			ID: 1, Name: "ready", ScanState: "ready", IndexState: "ready",
			HeadCommit: "aaaaaaaa", IndexedCommit: "aaaaaaaa",
		},
		{ID: 2, Name: "pending", ScanState: "ready", IndexState: "pending", HeadCommit: "bbbbbbbb"},
		{
			ID: 3, Name: "empty", ScanState: "empty", ScanError: catalog.EmptyRepositoryReason,
			IndexState: "empty", IndexError: catalog.EmptyRepositoryReason,
		},
		{
			ID: 4, Name: "broken", ScanState: "error", ScanError: "cannot read HEAD",
			IndexState: "error", IndexError: "cannot read HEAD",
		},
	}
	server, err := New(
		Config{Address: "127.0.0.1:7331", Version: "test"},
		codeintel.New(testStore{repositories: repositories}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}
	data, err := server.pageData(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if data.ReadyCount != 1 ||
		data.PendingCount != 1 ||
		data.ErrorCount != 1 ||
		data.EmptyCount != 1 ||
		data.IndexableCount != 3 {
		t.Fatalf("repository state counts = %#v", data)
	}

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/", nil)
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	body := response.Body.String()
	for _, expected := range []string{
		`data-total="3"`,
		"Indexing 1 of 3 indexable repositories",
		"1 empty with nothing to index",
		"status-badge status-empty",
		"Nothing to index: repository has no commits.",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("empty repository page is missing %q: %s", expected, body)
		}
	}

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/repositories", nil)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"scan_state":"empty"`) ||
		!strings.Contains(response.Body.String(), `"index_state":"empty"`) ||
		!strings.Contains(response.Body.String(), catalog.EmptyRepositoryReason) {
		t.Fatalf("empty repository API = %d: %s", response.Code, response.Body.String())
	}

	emptyServer, err := New(
		Config{Address: "127.0.0.1:7331", Version: "test"},
		codeintel.New(testStore{repositories: repositories[2:3]}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/", nil)
	response = httptest.NewRecorder()
	emptyServer.server.Handler.ServeHTTP(response, request)
	if !strings.Contains(response.Body.String(), "No repositories are searchable") ||
		strings.Contains(response.Body.String(), "Search is ready") {
		t.Fatalf("all-empty catalogue page = %d: %s", response.Code, response.Body.String())
	}
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
	if response.Code != http.StatusOK || len(response.Result().Cookies()) != 1 ||
		!strings.Contains(response.Body.String(), "Cloudflare Access") {
		t.Fatalf("local admin response = %d, cookies %d, body %q", response.Code, len(response.Result().Cookies()), response.Body.String())
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

type streamingTestSearcher struct {
	count int
}

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
			RepositoryID: 12,
			Repository:   "github.com/example/repo",
			Revision:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Path:         "internal/example.go",
			Language:     "Go",
			Lines: []search.LineMatch{{
				Number:    7,
				Text:      "if <tag> needle",
				Fragments: []search.Fragment{{Start: 9, End: 15}},
			}},
		}},
	}, nil
}

func (s streamingTestSearcher) Search(context.Context, search.Query) (search.Result, error) {
	matches := make([]search.FileMatch, 0, s.count)
	for index := 0; index < s.count; index++ {
		matches = append(matches, search.FileMatch{
			RepositoryID: 1,
			Repository:   "repo",
			Revision:     strings.Repeat("a", 40),
			Path:         fmt.Sprintf("internal/result-%03d.go", index),
			Language:     "Go",
			Lines: []search.LineMatch{{
				Number: 1,
				Text:   "var needle = true",
			}},
		})
	}
	return search.Result{
		MatchCount:      s.count,
		FileCount:       s.count,
		EstimatedFiles:  s.count,
		ReturnedFiles:   s.count,
		Limit:           100,
		TotalFilesExact: true,
		Matches:         matches,
	}, nil
}

type testMapService struct {
	snapshot     graph.Snapshot
	repositoryID int64
	refresh      bool
	progress     graph.ArtifactProgress
}

type testDependencyService struct {
	inventory        dependencies.Inventory
	progress         dependencies.RefreshProgress
	findings         dependencies.FindingResponse
	advisoryProgress dependencies.AdvisoryRefreshProgress
	options          dependencies.Options
	advisoryOptions  dependencies.AdvisoryOptions
	started          bool
	advisoryStarted  bool
	force            bool
}

func (s *testDependencyService) Topology(
	_ context.Context,
	snapshot graph.Snapshot,
	progress graph.ArtifactProgress,
	options dependencies.TopologyOptions,
) (dependencies.Topology, error) {
	service := dependencies.NewService(context.Background(), nil, nil)
	return service.Topology(context.Background(), snapshot, progress, options)
}

func (s *testDependencyService) ImportTopologyObservations(
	_ context.Context,
	_ dependencies.TopologyImportRequest,
) (dependencies.TopologyImportResult, error) {
	return dependencies.TopologyImportResult{}, nil
}

func (s *testDependencyService) Inventory(
	_ context.Context,
	_ graph.Snapshot,
	options dependencies.Options,
) (dependencies.Inventory, error) {
	s.options = options
	return s.inventory, nil
}

func (s *testDependencyService) StartRefresh(
	_ graph.Snapshot,
	options dependencies.Options,
	force bool,
) (dependencies.RefreshProgress, error) {
	s.started = true
	s.force = force
	s.options = options
	s.progress = dependencies.RefreshProgress{State: "running", Total: 2}
	return s.progress, nil
}

func (s *testDependencyService) Progress() dependencies.RefreshProgress {
	return s.progress
}

func (s *testDependencyService) Findings(
	_ context.Context,
	_ graph.Snapshot,
	options dependencies.AdvisoryOptions,
) (dependencies.FindingResponse, error) {
	s.advisoryOptions = options
	return s.findings, nil
}

func (s *testDependencyService) StartAdvisoryRefresh(
	_ graph.Snapshot,
	force bool,
) (dependencies.AdvisoryRefreshProgress, error) {
	s.advisoryStarted = true
	s.force = force
	s.advisoryProgress = dependencies.AdvisoryRefreshProgress{State: "running"}
	return s.advisoryProgress, nil
}

func (s *testDependencyService) AdvisoryProgress() dependencies.AdvisoryRefreshProgress {
	return s.advisoryProgress
}

func (s *testMapService) Snapshot(_ context.Context, repositoryID int64, refresh bool) (graph.Snapshot, error) {
	s.repositoryID = repositoryID
	s.refresh = refresh
	return s.snapshot, nil
}

func (s *testMapService) Reachability(
	_ context.Context,
	repositoryID int64,
) (graph.ReachabilityReport, error) {
	s.repositoryID = repositoryID
	return graph.ReachabilityReport{
		ID:    "reachability-test",
		Scope: graph.Scope{Kind: "repository", Complete: true},
		Summary: graph.ReachabilitySummary{
			Reachable:           2,
			ProbablyUnreachable: 1,
			Unknown:             3,
			Roots:               1,
			Edges:               1,
		},
	}, nil
}

func (s *testMapService) ReadDependencySnapshot(
	_ context.Context,
	repositoryID int64,
) (graph.Snapshot, graph.ArtifactProgress, error) {
	s.repositoryID = repositoryID
	progress := s.progress
	if progress.State == "" {
		progress = graph.ArtifactProgress{
			State:                 "ready",
			RequestedRepositories: 1,
			ReadyRepositories:     1,
		}
	}
	return s.snapshot, progress, nil
}

func (s *testMapService) ReadTopologySnapshot(
	_ context.Context,
	repositoryID int64,
) (graph.Snapshot, graph.ArtifactProgress, error) {
	s.repositoryID = repositoryID
	progress := s.progress
	if progress.State == "" {
		progress = graph.ArtifactProgress{
			State: "ready", RequestedRepositories: 1, ReadyRepositories: 1,
		}
	}
	return s.snapshot, progress, nil
}

func (s *testMapService) ReadRouteSnapshot(
	_ context.Context,
	repositoryID int64,
) (graph.Snapshot, graph.ArtifactProgress, error) {
	s.repositoryID = repositoryID
	progress := s.progress
	if progress.State == "" {
		progress = graph.ArtifactProgress{
			State: "ready", RequestedRepositories: 1, ReadyRepositories: 1,
		}
	}
	return s.snapshot, progress, nil
}

func (s *testMapService) StructureProgress(context.Context, int64) (graph.ArtifactProgress, error) {
	if s.progress.State != "" {
		return s.progress, nil
	}
	return graph.ArtifactProgress{State: "ready", RequestedRepositories: 1, ReadyRepositories: 1}, nil
}

type testDocumentationService struct {
	site              docs.Site
	page              docs.Page
	generated         docs.GenerateRequest
	generatedRequests []docs.GenerateRequest
}

func (s *testDocumentationService) Plan(_ context.Context, repositoryID int64) (docs.Site, error) {
	s.site.RepositoryID = repositoryID
	return s.site, nil
}

func (s *testDocumentationService) Generate(_ context.Context, request docs.GenerateRequest) (docs.Site, error) {
	s.generated = request
	s.generatedRequests = append(s.generatedRequests, request)
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
		`read_code_reachability`,
		`list_named_contexts`,
		`resolve_effective_contexts`,
		`find_references`,
		`read_dependency_inventory`,
		`read_system_topology`,
		`read_dependency_findings`,
		`list_deep_wiki_pages`,
		`read_generated_document`,
		`20 tools · no writes`,
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

func TestAdministratorCanGenerateChosenDeepWikis(t *testing.T) {
	settingsStore := &testSettingsStore{values: make(map[string]string)}
	securityManager, err := security.New(context.Background(), settingsStore, security.Config{
		Address:       "127.0.0.1:7331",
		DataDirectory: t.TempDir(),
		Initial:       security.Settings{Mode: security.ModeLocal},
	})
	if err != nil {
		t.Fatal(err)
	}
	repositories := []catalog.Repository{
		{ID: 11, Name: "Payments", Path: `C:\code\payments`, IndexState: "ready"},
		{ID: 12, Name: "Shipping", Path: `C:\code\shipping`, IndexState: "ready"},
	}
	documents := &testDocumentationService{
		site: docs.Site{RepositoryID: repositories[0].ID, Repository: repositories[0].Name},
	}
	providers := &testConversations{}
	server, err := New(
		Config{
			Address:       "127.0.0.1:7331",
			Version:       "test",
			Security:      securityManager,
			Docs:          documents,
			Conversations: providers,
		},
		codeintel.New(
			testStore{repositories: repositories},
			testSearcher{},
			"http://127.0.0.1:7331",
		),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}

	pageRequest := httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:7331/admin?section=wiki",
		nil,
	)
	pageResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(pageResponse, pageRequest)
	if pageResponse.Code != http.StatusOK || len(pageResponse.Result().Cookies()) == 0 {
		t.Fatalf("admin Wiki page = %d: %s", pageResponse.Code, pageResponse.Body.String())
	}
	for _, expected := range []string{
		`aria-current="page">Deep Wiki`,
		`data-admin-wiki-batch`,
		`data-admin-wiki-repository="11"`,
		`data-admin-wiki-repository="12"`,
		`Payments`,
		`Shipping`,
	} {
		if !strings.Contains(pageResponse.Body.String(), expected) {
			t.Fatalf("admin Wiki page does not contain %q", expected)
		}
	}
	adminCookie := pageResponse.Result().Cookies()[0]
	match := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(pageResponse.Body.String())
	if len(match) != 2 {
		t.Fatalf("admin CSRF token missing: %s", pageResponse.Body.String())
	}

	body := fmt.Sprintf(
		`{"csrf":%q,"repository_id":11,"refresh":true,"provider":"codex","model":"gpt-5.6-sol","effort":"high","timeout_seconds":1800,"token_budget":32000}`,
		match[1],
	)
	generateRequest := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:7331/admin/wiki/generate",
		strings.NewReader(body),
	)
	generateRequest.Header.Set("Content-Type", "application/json")
	generateRequest.AddCookie(adminCookie)
	generateResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(generateResponse, generateRequest)
	if generateResponse.Code != http.StatusOK {
		t.Fatalf("admin Wiki generation = %d: %s", generateResponse.Code, generateResponse.Body.String())
	}
	if len(documents.generatedRequests) != 1 {
		t.Fatalf("generation requests = %#v", documents.generatedRequests)
	}
	generated := documents.generatedRequests[0]
	if generated.RepositoryID != 11 || generated.Page != "" || !generated.Refresh ||
		generated.Preset != "quality" || generated.Provider != "codex" ||
		generated.Model != "gpt-5.6-sol" || generated.Effort != "high" ||
		generated.Timeout != 1800 || generated.TokenBudget != 32000 {
		t.Fatalf("generated request = %+v", generated)
	}

	deniedRequest := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:7331/admin/wiki/generate",
		strings.NewReader(`{"csrf":"wrong","repository_id":12}`),
	)
	deniedRequest.Header.Set("Content-Type", "application/json")
	deniedRequest.AddCookie(adminCookie)
	deniedResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(deniedResponse, deniedRequest)
	if deniedResponse.Code != http.StatusForbidden {
		t.Fatalf("invalid CSRF status = %d: %s", deniedResponse.Code, deniedResponse.Body.String())
	}
	if len(documents.generatedRequests) != 1 {
		t.Fatalf("invalid CSRF reached generation: %#v", documents.generatedRequests)
	}
}

func TestWikiAndAdministrationAuditEventsArePrincipalScoped(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "repokarta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	securityManager, err := security.New(context.Background(), database, security.Config{
		Address: "127.0.0.1:7331", DataDirectory: t.TempDir(),
		Initial: security.Settings{Mode: security.ModeLocal}, Audit: database,
	})
	if err != nil {
		t.Fatal(err)
	}
	documents := &testDocumentationService{site: docs.Site{RepositoryID: 6}}
	server, err := New(
		Config{
			Address: "127.0.0.1:7331", Docs: documents,
			Security: securityManager, Enterprise: database,
		},
		codeintel.New(testStore{}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, target, body string) *httptest.ResponseRecorder {
		t.Helper()
		httpRequest := httptest.NewRequest(
			method, "http://127.0.0.1:7331"+target, strings.NewReader(body),
		)
		if body != "" {
			httpRequest.Header.Set("Content-Type", "application/json")
		}
		response := httptest.NewRecorder()
		server.server.Handler.ServeHTTP(response, httpRequest)
		return response
	}
	if response := request(
		http.MethodPost, "/api/wiki/generate", `{"repository_id":6,"plan_only":true}`,
	); response.Code != http.StatusOK {
		t.Fatalf("wiki generation = %d: %s", response.Code, response.Body.String())
	}
	if response := request(
		http.MethodPut, "/api/admin/audit/retention", `{"days":30,"max_events":1000}`,
	); response.Code != http.StatusOK {
		t.Fatalf("administration mutation = %d: %s", response.Code, response.Body.String())
	}
	for _, action := range []string{"generation.wiki", "audit.retention.update"} {
		page, queryErr := database.AuditEvents(
			context.Background(), audit.Filter{Action: action, Limit: 10},
		)
		if queryErr != nil || len(page.Events) == 0 {
			t.Fatalf("%s audit = %#v, error = %v", action, page.Events, queryErr)
		}
		for _, event := range page.Events {
			if event.ActorID != "local:admin" || event.Provider != "local" {
				t.Fatalf("%s audit principal = %#v", action, event)
			}
		}
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
		`data-map-reachability`,
		`data-reachability-completeness`,
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

	reachabilityRequest := httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:7331/api/reachability?repository=4",
		nil,
	)
	reachabilityResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(reachabilityResponse, reachabilityRequest)
	if reachabilityResponse.Code != http.StatusOK || maps.repositoryID != 4 {
		t.Fatalf(
			"reachability API status = %d, repository = %d",
			reachabilityResponse.Code,
			maps.repositoryID,
		)
	}
	for _, expected := range []string{
		`"id":"reachability-test"`,
		`"probably_unreachable":1`,
		`"unknown":3`,
	} {
		if !strings.Contains(reachabilityResponse.Body.String(), expected) {
			t.Fatalf(
				"reachability API body does not contain %q: %s",
				expected,
				reachabilityResponse.Body.String(),
			)
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
				Ecosystem:     "npm",
				Package:       "marked",
				Declared:      "^16.4.1",
				Resolution:    "constraint",
				Usage:         "production",
				Relationship:  "required",
				DeclaredScope: "dependencies",
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

	pageRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/dependencies?view=inventory&repository=4", nil)
	pageResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(pageResponse, pageRequest)
	if pageResponse.Code != http.StatusOK || maps.repositoryID != 4 {
		t.Fatalf("dependency page status = %d, repository = %d", pageResponse.Code, maps.repositoryID)
	}
	for _, expected := range []string{
		`aria-current="page">Dependencies`,
		`Package inventory`,
		`marked`,
		`^16.4.1`,
		`production`,
		`required · dependencies`,
		`Not checked`,
		`web/package.json:14`,
		`Rows per page`,
		`Showing 1 of 1 matching declarations`,
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
		`"returned_count":1`,
		`"limit":100`,
		`"ecosystem":"npm"`,
		`"package":"marked"`,
		`"declared":"^16.4.1"`,
		`"resolution":"constraint"`,
		`"usage":"production"`,
		`"relationship":"required"`,
		`"declared_scope":"dependencies"`,
		`"check_status":"unchecked"`,
	} {
		if !strings.Contains(apiResponse.Body.String(), expected) {
			t.Fatalf("dependency API does not contain %q: %s", expected, apiResponse.Body.String())
		}
	}
	invalidRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/dependencies?limit=501", nil)
	invalidResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(invalidResponse, invalidRequest)
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("oversized dependency page status = %d, body = %s", invalidResponse.Code, invalidResponse.Body.String())
	}
}

func TestDependencyTopologyIsDefaultAndExposesDirectedProtocolEvidence(t *testing.T) {
	repository := catalog.Repository{ID: 4, Name: "checkout", IndexState: "ready"}
	maps := &testMapService{snapshot: graph.Snapshot{
		ID: "topology-snapshot",
		Scope: graph.Scope{
			Kind: "repository", Complete: true,
			TotalRepositories: 1, AnalyzedRepositories: 1,
			RequestedRepositoryID: 4,
		},
		Components: []graph.SystemComponent{
			{ID: "checkout", Name: "checkout", Kind: "service", RepositoryID: 4, Repository: "checkout"},
			{ID: "orders", Name: "orders", Kind: "service", External: true},
		},
		Connections: []graph.SystemConnection{
			{
				ID: "http-checkout-orders", Source: "checkout", Target: "orders",
				Protocol: "http", Interaction: "calls", Transport: "https",
				Confidence: "high", EvidenceOrigin: "static",
				Evidence: []graph.Evidence{{
					RepositoryID: 4, Repository: "checkout", Revision: strings.Repeat("a", 40),
					Path: "internal/client.go", Line: 22,
					URL: "http://127.0.0.1:7331/source/4#L22",
				}},
			},
			{
				ID: "http-suppressed-orders", Source: "suppressed-deployment", Target: "orders",
				Protocol: "http", Interaction: "calls", Transport: "https",
				Confidence: "high", EvidenceOrigin: "static",
			},
		},
	}}
	registry := &testDependencyService{}
	server, err := New(
		Config{Address: "127.0.0.1:7331", Maps: maps, Dependencies: registry},
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
	if pageResponse.Code != http.StatusOK {
		t.Fatalf("topology page status = %d, body = %s", pageResponse.Code, pageResponse.Body.String())
	}
	for _, expected := range []string{
		`System topology`, `Inbound callers`, `outbound dependencies`, `HTTP`, `MCP`,
		`Resolved`, `Candidates`, `Unresolved`,
		`data-topology-workspace`, `data-topology-warning`,
		`/api/dependencies/topology?depth=1&amp;direction=both&amp;repository=4`,
	} {
		if !strings.Contains(pageResponse.Body.String(), expected) {
			t.Fatalf("topology page does not contain %q", expected)
		}
	}
	apiRequest := httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:7331/api/dependencies/topology?repository=4&protocol=http",
		nil,
	)
	apiResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(apiResponse, apiRequest)
	if apiResponse.Code != http.StatusOK ||
		!strings.Contains(apiResponse.Body.String(), `"protocol":"http"`) ||
		!strings.Contains(apiResponse.Body.String(), `"source":"checkout"`) ||
		!strings.Contains(apiResponse.Body.String(), `"target":"orders"`) ||
		!strings.Contains(apiResponse.Body.String(), `"neighborhood_direction":"outbound"`) ||
		!strings.Contains(apiResponse.Body.String(), `"outbound_connection_count":1`) ||
		!strings.Contains(apiResponse.Body.String(), `"code":"missing_component_reference"`) ||
		!strings.Contains(apiResponse.Body.String(), `"count":1`) ||
		strings.Contains(apiResponse.Body.String(), `"source":"suppressed-deployment"`) {
		t.Fatalf("topology API status = %d, body = %s", apiResponse.Code, apiResponse.Body.String())
	}
	inboundRequest := httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:7331/api/dependencies/topology?repository=4&direction=inbound&depth=1",
		nil,
	)
	inboundResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(inboundResponse, inboundRequest)
	if inboundResponse.Code != http.StatusOK ||
		!strings.Contains(inboundResponse.Body.String(), `"direction":"inbound"`) ||
		!strings.Contains(inboundResponse.Body.String(), `"connection_count":0`) ||
		!strings.Contains(inboundResponse.Body.String(), `"outbound_connection_count":1`) {
		t.Fatalf(
			"inbound topology API status = %d, body = %s",
			inboundResponse.Code, inboundResponse.Body.String(),
		)
	}
}

func TestDependencyAPIReportsColdArtifactProgressWithoutSynchronousBuild(t *testing.T) {
	maps := &testMapService{progress: graph.ArtifactProgress{
		State:                 "building",
		RequestedRepositories: 12,
		ReadyRepositories:     5,
		PendingRepositories:   7,
	}}
	server, err := New(
		Config{Address: "127.0.0.1:7331", Maps: maps},
		codeintel.New(testStore{}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/dependencies", nil)
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || response.Header().Get("Retry-After") != "2" {
		t.Fatalf("cold dependency status = %d, retry = %q, body = %s",
			response.Code, response.Header().Get("Retry-After"), response.Body.String())
	}
	for _, expected := range []string{
		`"state":"building"`,
		`"requested_repositories":12`,
		`"ready_repositories":5`,
		`"pending_repositories":7`,
	} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("cold dependency response does not contain %q: %s", expected, response.Body.String())
		}
	}
	if maps.repositoryID != 0 {
		t.Fatalf("cold dependency read used repository %d", maps.repositoryID)
	}
}

func TestDependencyRefreshAPIStartsFilteredBackgroundWork(t *testing.T) {
	maps := &testMapService{snapshot: graph.Snapshot{
		Manifests: []graph.Manifest{{Kind: "npm package", Path: "package.json"}},
	}}
	registry := &testDependencyService{progress: dependencies.RefreshProgress{State: "idle"}}
	server, err := New(
		Config{Address: "127.0.0.1:7331", Maps: maps, Dependencies: registry},
		codeintel.New(testStore{}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:7331/api/dependencies/refresh?repository=4&ecosystem=npm&usage=production&force=true",
		nil,
	)
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !registry.started || !registry.force ||
		registry.options.Ecosystem != "npm" || registry.options.Usage != "production" ||
		maps.repositoryID != 4 {
		t.Fatalf(
			"refresh status = %d, registry = %#v, repository = %d, body = %s",
			response.Code,
			registry,
			maps.repositoryID,
			response.Body.String(),
		)
	}
	if !strings.Contains(response.Body.String(), `"state":"running"`) ||
		!strings.Contains(response.Body.String(), `"total":2`) {
		t.Fatalf("refresh body = %s", response.Body.String())
	}
}

func TestDependencyFindingsUIAPIRefreshAndDescriptiveLimitError(t *testing.T) {
	revision := strings.Repeat("a", 40)
	maps := &testMapService{snapshot: graph.Snapshot{
		Repositories: []graph.Repository{{ID: 4, Name: "acme/service", Revision: revision}},
	}}
	registry := &testDependencyService{
		findings: dependencies.FindingResponse{
			CheckState: "ready", AdvisoryOnly: true, TotalFindingCount: 1,
			FindingCount: 1, ReturnedCount: 1, Limit: 100,
			CheckedDeclarationCount: 1,
			Snapshot: dependencies.AdvisorySnapshotStatus{
				State: "ready", Source: "OSV.dev", Version: "sha256:fixture",
				RetrievedAt: time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC),
			},
			Findings: []dependencies.Finding{{
				ID: "finding-1", AdvisoryID: "GHSA-fixture",
				Aliases: []string{"CVE-2026-0001"}, Summary: "Fixture vulnerability",
				Severity: "critical", Ecosystem: "npm", Package: "left-pad",
				Version: "1.5.0", MatchBasis: "resolved", MatchConfidence: "high",
				Usage: "production", RepositoryID: 4, Repository: "acme/service",
				Revision: revision, ManifestPath: "package-lock.json",
				ManifestEvidence: graph.Evidence{
					Line: 12, URL: "http://127.0.0.1:7331/source/4#L12",
				},
				AdvisoryEvidence: dependencies.AdvisoryEvidence{
					AdvisoryURL:     "https://api.osv.dev/v1/vulns/GHSA-fixture",
					SnapshotVersion: "sha256:fixture",
				},
			}},
		},
		advisoryProgress: dependencies.AdvisoryRefreshProgress{State: "idle"},
	}
	server, err := New(
		Config{Address: "127.0.0.1:7331", Maps: maps, Dependencies: registry},
		codeintel.New(testStore{repositories: []catalog.Repository{{
			ID: 4, Name: "acme/service", IndexState: "ready",
		}}}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}

	page := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(page, httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:7331/dependencies?view=findings&repository=4&severity=critical&usage=production",
		nil,
	))
	if page.Code != http.StatusOK {
		t.Fatalf("findings page status=%d body=%s", page.Code, page.Body.String())
	}
	for _, expected := range []string{
		"Security findings", "GHSA-fixture", "CVE-2026-0001",
		"critical", "production", "resolved (high confidence)",
		"never represented as an enforced CI gate", "Export SARIF",
	} {
		if !strings.Contains(page.Body.String(), expected) {
			t.Fatalf("findings page lacks %q: %s", expected, page.Body.String())
		}
	}
	if registry.advisoryOptions.Severity != "critical" ||
		registry.advisoryOptions.Usage != "production" || maps.repositoryID != 4 {
		t.Fatalf("finding options=%#v repository=%d", registry.advisoryOptions, maps.repositoryID)
	}

	api := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(api, httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:7331/api/dependencies/findings?repository=4&limit=50",
		nil,
	))
	if api.Code != http.StatusOK || !strings.Contains(api.Body.String(), `"check_state":"ready"`) ||
		!strings.Contains(api.Body.String(), `"advisory_only":true`) {
		t.Fatalf("findings API status=%d body=%s", api.Code, api.Body.String())
	}

	sarif := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(sarif, httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:7331/api/dependencies/findings.sarif?repository=4",
		nil,
	))
	if sarif.Code != http.StatusOK ||
		sarif.Header().Get("Content-Type") != "application/sarif+json" ||
		!strings.Contains(sarif.Body.String(), `"ruleId": "GHSA-fixture"`) {
		t.Fatalf("findings SARIF status=%d headers=%v body=%s", sarif.Code, sarif.Header(), sarif.Body.String())
	}

	invalid := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(invalid, httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:7331/api/dependencies/findings?limit=501",
		nil,
	))
	if invalid.Code != http.StatusBadRequest ||
		!strings.Contains(invalid.Body.String(), `"message":"limit must be between 1 and 500"`) {
		t.Fatalf("invalid findings status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	invalidSeverity := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(invalidSeverity, httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:7331/api/dependencies/findings?severity=urgent",
		nil,
	))
	if invalidSeverity.Code != http.StatusBadRequest ||
		!strings.Contains(invalidSeverity.Body.String(), `"message":"severity must be one of critical, high, medium, low, unknown"`) {
		t.Fatalf("invalid severity status=%d body=%s", invalidSeverity.Code, invalidSeverity.Body.String())
	}

	refresh := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(refresh, httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:7331/api/dependencies/advisories/refresh?repository=4&force=true",
		nil,
	))
	if refresh.Code != http.StatusAccepted || !registry.advisoryStarted || !registry.force {
		t.Fatalf("advisory refresh status=%d service=%#v body=%s", refresh.Code, registry, refresh.Body.String())
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
	if len(result.Matches) != 1 ||
		result.Matches[0].ResultType != "content" ||
		result.Matches[0].Citation == "" ||
		result.Matches[0].SourceURL == "" {
		t.Fatalf("evidence = %#v", result.Matches)
	}

	compactRequest := httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:7331/api/search?q=needle&limit=100&compact=true",
		nil,
	)
	compactResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(compactResponse, compactRequest)
	if compactResponse.Code != http.StatusOK {
		t.Fatalf("compact status = %d, body = %s", compactResponse.Code, compactResponse.Body.String())
	}
	var compact codeintel.SearchResponse
	if err := json.Unmarshal(compactResponse.Body.Bytes(), &compact); err != nil {
		t.Fatal(err)
	}
	if !compact.Compact || len(compact.Matches) != 1 || len(compact.Matches[0].Lines) != 1 ||
		compact.Matches[0].Lines[0].Number != 7 || compact.Matches[0].Lines[0].Text != "" ||
		len(compact.Matches[0].Ranking) != 0 || len(compact.Matches[0].Actions) != 0 ||
		len(compact.Facets) != 0 {
		t.Fatalf("compact API evidence = %#v", compact)
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

type fixedReferenceStructure struct {
	index graph.StructuralIndex
}

func (structure fixedReferenceStructure) ReadStructure(context.Context, int64) (graph.StructuralIndex, error) {
	return structure.index, nil
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

func TestAPIASTSearchReturnsExplicitIndexProgress(t *testing.T) {
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
		http.MethodPost,
		"http://127.0.0.1:7331/api/ast/search",
		strings.NewReader(`{"language":"go","query":"(function_declaration) @function"}`),
	)
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || response.Header().Get("Retry-After") != "2" {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var result codeintel.ASTSearchResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Resolution != "tree-sitter-query" || result.Index.State != "building" ||
		result.Index.ReadyRepositories != 1 || result.Index.PendingRepositories != 2 ||
		result.Complete {
		t.Fatalf("AST progress = %#v", result)
	}
}

func TestStructuredContextsFlowThroughJSONSearchAndChat(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(directory, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(directory, "internal", "helper.go"),
		[]byte("package internal\n\nfunc Helper() {}\n"),
		0o644,
	); err != nil {
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
	intelligence := codeintel.New(
		testStore{repositories: []catalog.Repository{repository}},
		testSearcher{},
		"http://127.0.0.1:7331",
	).UseStructure(fixedReferenceStructure{index: graph.StructuralIndex{
		Scope: graph.Scope{Complete: true, TotalRepositories: 1, AnalyzedRepositories: 1},
		Structure: []graph.StructuralDocument{{
			RepositoryID: repository.ID, Repository: repository.Name,
			Revision: revision, Path: "internal/helper.go",
			Symbols: []analysis.Symbol{{
				Name: "Helper", Kind: "function",
				Range: analysis.Range{StartLine: 3, EndLine: 3},
			}},
		}},
	}})
	server, err := New(
		Config{Address: "127.0.0.1:7331", Conversations: conversations},
		intelligence,
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}
	selector := contextscope.Selector{
		Kind: contextscope.KindFile, RepositoryID: repository.ID,
		Revision: revision, Path: "main.go",
	}
	resolveBody, err := json.Marshal(map[string]any{
		"contexts": []contextscope.Selector{selector},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolveRequest := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:7331/api/contexts/resolve",
		bytes.NewReader(resolveBody),
	)
	resolveResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(resolveResponse, resolveRequest)
	if resolveResponse.Code != http.StatusOK ||
		!strings.Contains(resolveResponse.Body.String(), `"label":"@fixture:main.go"`) {
		t.Fatalf("structured context resolution = %d, body = %s", resolveResponse.Code, resolveResponse.Body.String())
	}
	for _, testCase := range []struct {
		name     string
		selector contextscope.Selector
		label    string
	}{
		{
			name: "directory",
			selector: contextscope.Selector{
				Kind: contextscope.KindDirectory, RepositoryID: repository.ID,
				Revision: revision, Path: "internal",
			},
			label: `"label":"@fixture:internal/"`,
		},
		{
			name: "symbol",
			selector: contextscope.Selector{
				Kind: contextscope.KindSymbol, RepositoryID: repository.ID,
				Revision: revision, Path: "internal/helper.go", Symbol: "Helper",
				SymbolKind: "function", Line: 3,
			},
			label: `"label":"@fixture:internal/helper.go#Helper:3"`,
		},
	} {
		t.Run(testCase.name+" context resolution", func(t *testing.T) {
			body, marshalErr := json.Marshal(map[string]any{
				"contexts": []contextscope.Selector{testCase.selector},
			})
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			request := httptest.NewRequest(
				http.MethodPost,
				"http://127.0.0.1:7331/api/contexts/resolve",
				bytes.NewReader(body),
			)
			response := httptest.NewRecorder()
			server.server.Handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), testCase.label) {
				t.Fatalf("resolution = %d, body = %s", response.Code, response.Body.String())
			}
		})
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
	symbolSelector := contextscope.Selector{
		Kind: contextscope.KindSymbol, RepositoryID: repository.ID,
		Revision: revision, Path: "internal/helper.go", Symbol: "Helper",
		SymbolKind: "function", Line: 3,
	}
	symbolBody, err := json.Marshal(codeintel.SymbolRequest{
		Symbol: "Helper", Contexts: []contextscope.Selector{symbolSelector},
	})
	if err != nil {
		t.Fatal(err)
	}
	symbolRequest := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:7331/api/symbol",
		bytes.NewReader(symbolBody),
	)
	symbolResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(symbolResponse, symbolRequest)
	if symbolResponse.Code != http.StatusOK ||
		!strings.Contains(symbolResponse.Body.String(), `"symbol":"Helper"`) {
		t.Fatalf("structured symbol status = %d, body = %s", symbolResponse.Code, symbolResponse.Body.String())
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
	staleResolveBody, _ := json.Marshal(map[string]any{
		"contexts": []contextscope.Selector{staleSelector},
	})
	staleResolveRequest := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:7331/api/contexts/resolve",
		bytes.NewReader(staleResolveBody),
	)
	staleResolveResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(staleResolveResponse, staleResolveRequest)
	if staleResolveResponse.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(staleResolveResponse.Body.String(), `"code":"stale"`) {
		t.Fatalf(
			"stale context resolution = %d, body = %s",
			staleResolveResponse.Code,
			staleResolveResponse.Body.String(),
		)
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
		"http://127.0.0.1:7331/api/tree/repo?offset=-1",
	} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		response := httptest.NewRecorder()
		server.server.Handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s returned %d: %s", target, response.Code, response.Body.String())
		}
	}
}

func TestReadOnlyHTTPAPIsAgainstCommittedRepository(t *testing.T) {
	repositoryDirectory := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(repositoryDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	runHTTPGit(t, repositoryDirectory, "init")
	runHTTPGit(t, repositoryDirectory, "config", "user.email", "tests@repokarta.local")
	runHTTPGit(t, repositoryDirectory, "config", "user.name", "RepoKarta Tests")
	sourcePath := filepath.Join(repositoryDirectory, "main.go")
	if err := os.WriteFile(sourcePath, []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runHTTPGit(t, repositoryDirectory, "add", "main.go")
	runHTTPGit(t, repositoryDirectory, "commit", "-m", "first")
	firstRevision := strings.TrimSpace(runHTTPGit(t, repositoryDirectory, "rev-parse", "HEAD"))
	if err := os.WriteFile(sourcePath, []byte("package main\n\nfunc main() { println(\"ready\") }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runHTTPGit(t, repositoryDirectory, "add", "main.go")
	runHTTPGit(t, repositoryDirectory, "commit", "-m", "second")
	secondRevision := strings.TrimSpace(runHTTPGit(t, repositoryDirectory, "rev-parse", "HEAD"))

	repository := catalog.Repository{
		ID: 12, Name: "example/repository", Path: repositoryDirectory,
		OriginURL:       "https://github.com/example/repository.git",
		DefaultRevision: "main", HeadCommit: secondRevision, IndexedCommit: secondRevision,
		IndexState: "ready",
	}
	conversations := &testConversations{}
	maps := &testMapService{progress: graph.ArtifactProgress{
		State: "building", RequestedRepositories: 1, PendingRepositories: 1,
	}, snapshot: graph.Snapshot{
		Scope: graph.Scope{
			Kind: "repository", Complete: true, TotalRepositories: 1,
			AnalyzedRepositories: 1, RequestedRepositoryID: 12,
		},
		Nodes: []graph.Node{{
			ID: "route-ready", Kind: "route", Label: "GET /ready",
			RepositoryID: 12, Repository: "example/repository", Path: "main.go",
			Evidence: []graph.Evidence{{
				RepositoryID: 12, Repository: "example/repository",
				Revision: secondRevision, Path: "main.go", Line: 3,
			}},
		}},
		Components: []graph.SystemComponent{
			{
				ID: "example", Name: "example", Kind: "service",
				RepositoryID: 12, Repository: "example/repository",
			},
			{ID: "caller", Name: "checkout", Kind: "service", RepositoryID: 13},
		},
		Connections: []graph.SystemConnection{{
			ID: "caller-example", Source: "caller", Target: "example",
			Protocol: "http", Interaction: "calls", Transport: "https",
			Confidence: "high", EvidenceOrigin: "static", TargetResolved: true,
			Evidence: []graph.Evidence{{
				RepositoryID: 13, Repository: "checkout",
				Revision: strings.Repeat("c", 40), Path: "client.go", Line: 9,
				Label: "https://example.internal/ready",
				URL:   "http://127.0.0.1:7331/source/13?path=client.go&focus=9-9#L9",
			}},
		}},
	}}
	server, err := New(
		Config{
			Address: "127.0.0.1:7331", Version: "coverage-test",
			RepositoryRoot: repositoryDirectory, Conversations: conversations, Maps: maps,
			Dependencies: &testDependencyService{},
		},
		codeintel.New(testStore{repositories: []catalog.Repository{repository}}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}
	testCases := []struct {
		method   string
		target   string
		status   int
		contains string
	}{
		{http.MethodGet, "/healthz", http.StatusOK, `"version":"coverage-test"`},
		{http.MethodGet, "/api/repositories", http.StatusOK, `"example/repository"`},
		{http.MethodGet, "/repositories", http.StatusOK, "example/repository"},
		{http.MethodPost, "/repositories/refresh", http.StatusOK, "example/repository"},
		{http.MethodGet, "/api/providers", http.StatusOK, `"id":"test"`},
		{http.MethodGet, "/api/file/12?path=main.go&lines=1-3", http.StatusOK, `"package main"`},
		{http.MethodGet, "/api/tree/12", http.StatusOK, `"main.go"`},
		{http.MethodGet, "/projects/12", http.StatusOK, "Indexed project"},
		{http.MethodGet, "/api/git/log/12?limit=2", http.StatusOK, `"second"`},
		{http.MethodGet, "/api/git/diff/12?from=" + firstRevision + "&to=" + secondRevision + "&path=main.go", http.StatusOK, `println`},
		{http.MethodGet, "/source/12?path=main.go&lines=1-3&focus=3", http.StatusOK, "Current directory"},
		{http.MethodGet, "/api/artifacts/progress", http.StatusOK, `"state":"building"`},
		{http.MethodGet, "/api/contexts/suggest?kind=repository&q=example", http.StatusOK, `"suggestions"`},
		{http.MethodGet, "/api/symbol?symbol=main&repo=12", http.StatusOK, `"match_count":250`},
	}
	for _, testCase := range testCases {
		request := httptest.NewRequest(testCase.method, "http://127.0.0.1:7331"+testCase.target, nil)
		response := httptest.NewRecorder()
		server.server.Handler.ServeHTTP(response, request)
		if response.Code != testCase.status ||
			(testCase.contains != "" && !strings.Contains(response.Body.String(), testCase.contains)) {
			t.Fatalf("%s %s = %d: %s", testCase.method, testCase.target, response.Code, response.Body.String())
		}
		if response.Header().Get("X-Request-ID") == "" {
			t.Fatalf("%s did not receive a correlation ID", testCase.target)
		}
	}
	sourceRequest := httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:7331/source/12?path=main.go&lines=1-3",
		nil,
	)
	sourceResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(sourceResponse, sourceRequest)
	for _, expected := range []string{
		`data-source-intelligence`,
		`data-repository-id="12"`,
		`Find usages`,
		`data-source-intelligence-toggle`,
		`aria-controls="source-intelligence-results"`,
		`Search this repository`,
		`GET /ready`,
		`checkout`,
		`route-path evidence`,
		`direction=inbound`,
	} {
		if !strings.Contains(sourceResponse.Body.String(), expected) {
			t.Fatalf("source editor does not contain %q: %s", expected, sourceResponse.Body.String())
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	eventRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/events", nil).WithContext(ctx)
	eventResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(eventResponse, eventRequest)
	if eventResponse.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("event content type = %q", eventResponse.Header().Get("Content-Type"))
	}

	runServer, err := New(
		Config{Address: "127.0.0.1:0", Version: "coverage-test"},
		codeintel.New(testStore{}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}
	runContext, stop := context.WithCancel(context.Background())
	stop()
	if err := runServer.Run(runContext); err != nil {
		t.Fatalf("graceful run shutdown: %v", err)
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
	if s.conversation.Author.ID != filter.AuthorID {
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
	if !strings.Contains(response.Body.String(), `"can_view_all":false`) ||
		!strings.Contains(response.Body.String(), `"scope":"own"`) ||
		!strings.Contains(response.Body.String(), `"name":"Local administrator"`) {
		t.Fatalf("strict owner conversation scope was not exposed correctly: %s", response.Body.String())
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

func TestAdministratorCannotBypassConversationOwnership(t *testing.T) {
	conversations := &testHistoryConversations{conversation: agent.Conversation{
		ID: "alice-chat", Title: "Alice private", Provider: "test",
		Author: agent.ConversationAuthor{
			ID: "saml:alice", Name: "Alice", Provider: "saml",
		},
	}}
	server, err := New(
		Config{Address: "127.0.0.1:7331", Conversations: conversations},
		codeintel.New(testStore{}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}
	listRequest := httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:7331/api/conversations?scope=all",
		nil,
	)
	listResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(listResponse, listRequest)
	if listResponse.Code != http.StatusOK ||
		!strings.Contains(listResponse.Body.String(), `"conversations":[]`) ||
		!strings.Contains(listResponse.Body.String(), `"can_view_all":false`) ||
		conversations.lastFilter.AuthorID != "local:admin" {
		t.Fatalf(
			"administrator list status = %d, body = %s, filter = %#v",
			listResponse.Code, listResponse.Body.String(), conversations.lastFilter,
		)
	}
	getRequest := httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:7331/api/conversations/alice-chat",
		nil,
	)
	getResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusForbidden {
		t.Fatalf(
			"administrator cross-author get status = %d, body = %s",
			getResponse.Code, getResponse.Body.String(),
		)
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
		`>SCIP / AST references</option>`,
		`/source/1?rev=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`,
		`lines=1-200&focus=7-7#L7`,
		`internal%2Fexample.go`,
		`data-result-action="source"`,
		`data-result-action="map"`,
		`data-result-action="dependencies"`,
		`data-result-action="conversation"`,
		`data-result-action="context"`,
		`Start scoped conversation`,
		`Add to current context`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected response to contain %q\n%s", expected, body)
		}
	}
	if strings.Contains(body, "<tag>") {
		t.Fatal("source line was not HTML escaped")
	}
}

func TestSearchStreamReturnsProgressiveServerRenderedBatches(t *testing.T) {
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
			streamingTestSearcher{count: 45},
			"http://127.0.0.1:7331",
		),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:7331/api/search/stream?q=needle&limit=100",
		nil,
	)
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("stream status = %d, body = %s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); !strings.Contains(contentType, "application/x-ndjson") {
		t.Fatalf("stream content type = %q", contentType)
	}
	decoder := json.NewDecoder(response.Body)
	events := make([]searchStreamEvent, 0)
	for decoder.More() {
		var event searchStreamEvent
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if len(events) != 4 || events[0].Type != "started" {
		t.Fatalf("events = %#v, want started plus three result batches", events)
	}
	for index, event := range events[1:] {
		if event.Type != "results" || event.HTML == "" {
			t.Fatalf("result event %d = %#v", index, event)
		}
		if event.Complete != (index == 2) {
			t.Fatalf("result event %d complete = %v", index, event.Complete)
		}
	}
	if !strings.Contains(events[len(events)-1].HTML, "45 files shown") ||
		!strings.Contains(events[len(events)-1].HTML, "result-044.go") {
		t.Fatalf("final streamed HTML is incomplete: %s", events[len(events)-1].HTML)
	}
}

func TestDependencyOptionsAndURLsPreserveUsageFilters(t *testing.T) {
	request := httptest.NewRequest(
		http.MethodGet,
		"https://repo.example.com/dependencies?query=junit&ecosystem=maven&usage=test&relationship=optional&resolution=exact&limit=50",
		nil,
	)
	options, err := dependencyOptions(request)
	if err != nil {
		t.Fatal(err)
	}
	if options.Query != "junit" || options.Ecosystem != "maven" ||
		options.Usage != "test" || options.Relationship != "optional" ||
		options.Resolution != "exact" || options.Limit != 50 {
		t.Fatalf("dependency options = %#v", options)
	}
	target := dependencyURL("/api/dependencies", 42, options, 100)
	for _, expected := range []string{
		"repository=42", "query=junit", "ecosystem=maven", "usage=test",
		"relationship=optional", "resolution=exact", "limit=50", "offset=100",
	} {
		if !strings.Contains(target, expected) {
			t.Fatalf("dependency URL %q does not contain %q", target, expected)
		}
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
		`<option value="zoekt" selected>Zoekt syntax</option>`,
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
	conversations := &testHistoryConversations{conversation: agent.Conversation{
		ID: "conversation",
		Author: agent.ConversationAuthor{
			ID:       "local:admin",
			Provider: string(security.ModeLocal),
		},
	}}
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
	securityManager, err := security.New(context.Background(), &testSettingsStore{values: make(map[string]string)}, security.Config{
		Address: "127.0.0.1:7331",
		Initial: security.Settings{Mode: security.ModeLocal},
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(
		Config{Address: "127.0.0.1:7331", Security: securityManager},
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

	goodRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/healthz", nil)
	goodResponse := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(goodResponse, goodRequest)
	if goodResponse.Header().Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(goodResponse.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("security headers missing: %#v", goodResponse.Header())
	}
}

func TestAuditFilterPreservesEqualSinceAndUntil(t *testing.T) {
	timestamp := "2026-07-28T12:00:00Z"
	request := httptest.NewRequest(http.MethodGet, "/api/admin/audit?since="+timestamp+"&until="+timestamp, nil)
	filter, err := auditFilter(request)
	if err != nil {
		t.Fatal(err)
	}
	if filter.Since.IsZero() || filter.Until.IsZero() || !filter.Since.Equal(filter.Until) {
		t.Fatalf("equal audit bounds were not both preserved: %#v", filter)
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

func TestRoutePathMatchesCommitPinnedCallerEvidence(t *testing.T) {
	for _, testCase := range []struct {
		route    string
		evidence string
		want     bool
	}{
		{"GET /orders/{orderId}", "https://orders.internal/orders/42", true},
		{"POST /orders", "https://orders.internal/orders?dryRun=true", true},
		{"GET /orders/{orderId}", "https://orders.internal/orders/42/items", false},
		{"GET /orders", "orders-service", false},
	} {
		got := routeMatchesCallerEvidence(testCase.route, []graph.Evidence{{
			Label: testCase.evidence,
		}})
		if got != testCase.want {
			t.Fatalf(
				"routeMatchesCallerEvidence(%q, %q) = %v, want %v",
				testCase.route,
				testCase.evidence,
				got,
				testCase.want,
			)
		}
	}
}

func TestSourceIntelligenceBoundsRoutesAndPrioritizesVisibleWindow(t *testing.T) {
	nodes := make([]graph.Node, 0, 30)
	for line := 1; line <= 30; line++ {
		nodes = append(nodes, graph.Node{
			ID: fmt.Sprintf("route-%d", line), Kind: "route",
			Label:        fmt.Sprintf("GET /route/%d", line),
			RepositoryID: 7, Path: "Controller.java",
			Evidence: []graph.Evidence{{
				RepositoryID: 7, Path: "Controller.java", Line: line,
			}},
		})
	}
	server := &Server{maps: &testMapService{snapshot: graph.Snapshot{
		Nodes: nodes,
		Scope: graph.Scope{
			Complete: true, TotalRepositories: 1, AnalyzedRepositories: 1,
			RequestedRepositoryID: 7,
		},
	}}}
	view := server.sourceIntelligence(
		t.Context(),
		7,
		strings.Repeat("a", 40),
		"Controller.java",
		20,
		22,
	)
	if view.RouteCount != 30 || len(view.Routes) != maximumSourceRoutes ||
		view.OmittedRoutes != 6 {
		t.Fatalf(
			"bounded routes = total %d, returned %d, omitted %d",
			view.RouteCount,
			len(view.Routes),
			view.OmittedRoutes,
		)
	}
	for index, want := range []int{20, 21, 22} {
		if view.Routes[index].Line != want || !view.Routes[index].VisibleWindow {
			t.Fatalf("prioritized route %d = %#v, want visible line %d", index, view.Routes[index], want)
		}
	}
}

func TestSourceIntelligenceAttributesMonorepoCallerToOwningRouteComponent(t *testing.T) {
	snapshot := graph.Snapshot{
		Scope: graph.Scope{
			Kind: "repository", Complete: true, TotalRepositories: 1,
			AnalyzedRepositories: 1, RequestedRepositoryID: 7,
		},
		Nodes: []graph.Node{{
			ID: "orders-route", Kind: "route", Label: "GET /orders/{id}",
			RepositoryID: 7, Path: "apps/orders/Controller.java",
			Evidence: []graph.Evidence{{
				RepositoryID: 7, Path: "apps/orders/Controller.java", Line: 21,
			}},
		}},
		Components: []graph.SystemComponent{
			{
				ID: "orders", Name: "orders", Kind: "service",
				RepositoryID: 7, Path: "apps/orders",
			},
			{
				ID: "checkout", Name: "checkout", Kind: "service",
				RepositoryID: 7, Path: "apps/checkout",
			},
		},
		Connections: []graph.SystemConnection{{
			ID: "checkout-orders", Source: "checkout", Target: "orders",
			Protocol: "http", Interaction: "calls", Transport: "https",
			Confidence: "high", EvidenceOrigin: "static", TargetResolved: true,
			Evidence: []graph.Evidence{{
				RepositoryID: 7, Path: "apps/checkout/OrdersClient.java", Line: 14,
				Label: "https://orders.internal/orders/42",
			}},
		}},
	}
	server := &Server{
		maps:         &testMapService{snapshot: snapshot},
		dependencies: &testDependencyService{},
	}
	view := server.sourceIntelligence(
		t.Context(),
		7,
		strings.Repeat("a", 40),
		"apps/orders/Controller.java",
		1,
		100,
	)
	if len(view.Callers) != 1 || view.Callers[0].Name != "checkout" ||
		len(view.Routes) != 1 || len(view.Routes[0].Callers) != 1 {
		t.Fatalf("monorepo source intelligence = %#v", view)
	}
}

func TestHTTPBoundaryAndFormattingHelpers(t *testing.T) {
	if id, err := optionalRepositoryID(""); err != nil || id != 0 {
		t.Fatalf("optional blank repository = %d, %v", id, err)
	}
	if id, err := optionalRepositoryID("42"); err != nil || id != 42 {
		t.Fatalf("optional repository = %d, %v", id, err)
	}
	if _, err := optionalRepositoryID("bad"); err == nil {
		t.Fatal("invalid optional repository accepted")
	}
	if _, err := requiredRepositoryID(""); err == nil {
		t.Fatal("missing required repository accepted")
	}
	if id, name := repositorySelector("17"); id != 17 || name != "" {
		t.Fatalf("numeric selector = %d, %q", id, name)
	}
	if id, name := repositorySelector("owner/repo"); id != 0 || name != "owner/repo" {
		t.Fatalf("named selector = %d, %q", id, name)
	}
	for _, value := range []string{"", "../unsafe", `a\b:c?.zip`, strings.Repeat("x", 200)} {
		if (value != "" && safeDownloadName(value) == "") || strings.Contains(safeDownloadName(value), "/") {
			t.Fatalf("unsafe download name remained unsafe: %q", safeDownloadName(value))
		}
	}
	if start, end := parseLineRange("5-8"); start != 5 || end != 8 {
		t.Fatalf("line range = %d-%d", start, end)
	}
	if start, end := parseLineRange("900-100"); start != 900 || end != 900 {
		t.Fatalf("invalid line range fallback = %d-%d", start, end)
	}
	if parseSearchLimit("5000") != codeintel.MaximumSearchLimit ||
		parseSearchLimit("bad") != codeintel.DefaultSearchLimit {
		t.Fatal("search limit bounds changed")
	}
	if _, err := apiSearchLimit("0"); err == nil {
		t.Fatal("zero API search limit accepted")
	}
	if value, err := apiBoundedInteger("", "limit", 10, 20); err != nil || value != 10 {
		t.Fatalf("bounded fallback = %d, %v", value, err)
	}
	if _, err := apiBoundedInteger("21", "limit", 10, 20); err == nil {
		t.Fatal("oversized bounded integer accepted")
	}
	if formatDuration(1500*time.Millisecond) == "" || formatMilliseconds(2.25) == "" ||
		formatTime(time.Time{}) == "" || shortCommit("abcdef") != "abcdef" {
		t.Fatal("formatting helpers changed")
	}
	for _, state := range []string{"ready", "pending", "indexing", "error", "unknown"} {
		if statusLabel(state) == "" {
			t.Fatalf("empty status label for %q", state)
		}
	}
	if nextSearchLimit(100) <= 100 || nextSearchLimit(codeintel.MaximumSearchLimit) != codeintel.MaximumSearchLimit {
		t.Fatal("next search limit bounds changed")
	}
	if indexProgress(3, 4) != 75 || indexProgress(0, 0) != 0 {
		t.Fatal("index progress changed")
	}
	repositories := []catalog.Repository{{ID: 2, IndexedCommit: "b", IndexState: "ready"}, {ID: 1, IndexedCommit: "a", IndexState: "pending"}}
	if repositorySignature(repositories) == "" {
		t.Fatal("repository signature is empty")
	}
	if remoteFileURL("https://github.com/example/repo.git", "abc", "main.go", 2, 4) !=
		"https://github.com/example/repo/blob/abc/main.go#L2-L4" {
		t.Fatal("GitHub remote URL changed")
	}
	if remoteFileURL("", "abc", "main.go", 1, 1) != "" {
		t.Fatal("empty remote URL changed")
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/dependencies?repository=7&ecosystem=go&limit=25&offset=5", nil)
	options, err := dependencyOptions(request)
	if err != nil || options.Ecosystem != "go" || options.Limit != 25 || options.Offset != 5 {
		t.Fatalf("dependency options = %#v, %v", options, err)
	}
	if dependencyURL("/dependencies", 7, options, 30) == "" {
		t.Fatal("dependency URL is empty")
	}
	if data := buildMCPPageData("http://localhost/mcp", "secret-token-value", "repokarta", "http://localhost"); len(data.Tools) != 20 {
		t.Fatalf("MCP page tools = %d", len(data.Tools))
	}

	for _, write := range []func(http.ResponseWriter){
		func(response http.ResponseWriter) { writeDocumentationError(response, docs.ErrPageNotFound) },
		func(response http.ResponseWriter) { writeCodeIntelligenceError(response, source.ErrUnsafePath) },
		func(response http.ResponseWriter) { writeContextError(response, &contextscope.ResolutionError{}) },
	} {
		response := httptest.NewRecorder()
		write(response)
		if response.Code < 400 {
			t.Fatalf("error writer status = %d: %s", response.Code, response.Body.String())
		}
	}
}
