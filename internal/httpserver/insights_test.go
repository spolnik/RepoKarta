package httpserver

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/codeintel"
	"github.com/spolnik/RepoKarta/internal/insights"
	"github.com/spolnik/RepoKarta/internal/security"
	"github.com/spolnik/RepoKarta/internal/store"
)

func TestInsightsWorkspaceAndAPIExposeExplicitEvidenceStates(t *testing.T) {
	repository := catalog.Repository{
		ID: 8, Name: "service", DefaultRevision: "main",
		IndexedCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", IndexState: "ready",
	}
	evidence := &testInsightService{response: insights.QueryResponse{
		Current: []insights.Observation{{
			RepositoryID: 8, Repository: "service", Revision: repository.IndexedCommit,
			Kind: insights.KindMetric, Key: "coverage.line", Value: insightNumber(81.5),
			Unit: "percent", State: insights.StateMeasured, Confidence: "reported",
		}, {
			RepositoryID: 8, Repository: "service", Revision: repository.IndexedCommit,
			Kind: insights.KindFinding, Key: "SEC001", Severity: "warning",
			Message: "Review this call", Path: "internal/service.go", StartLine: 12,
			State: insights.StateUnresolvedPath, Confidence: "reported",
		}},
		Runs: []insights.Run{{
			ID: "run-1", RepositoryID: 8, Repository: "service", Revision: repository.IndexedCommit,
			Tool: "scanner", SourceKind: "uploaded_report", Status: insights.StatusPartial,
			Confidence: "reported", ObservationCount: 2,
		}},
	}}
	server, err := New(Config{
		Address: "127.0.0.1:7331", Version: "test", Insights: evidence,
	}, codeintel.New(
		testStore{repositories: []catalog.Repository{repository}},
		testSearcher{}, "http://127.0.0.1:7331",
	), testRefresher{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/insights?repository=8", nil)
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("workspace status = %d: %s", response.Code, response.Body.String())
	}
	for _, expected := range []string{
		`aria-current="page">Insights`, `coverage.line`, `81.50`,
		`SEC001`, `unresolved_path`, `Advisory only`, `never runs repository tests`,
	} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("workspace missing %q\n%s", expected, response.Body.String())
		}
	}

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/insights?repository=8&severity=warning&limit=25", nil)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || evidence.lastFilter.RepositoryID != 8 ||
		evidence.lastFilter.Severity != "warning" || evidence.lastFilter.Limit != 25 {
		t.Fatalf("API status=%d filter=%#v body=%s", response.Code, evidence.lastFilter, response.Body.String())
	}
}

func TestInsightMultipartImportCarriesProvenanceAndRedirects(t *testing.T) {
	repository := catalog.Repository{ID: 8, Name: "service", IndexedCommit: "abc", IndexState: "ready"}
	evidence := &testInsightService{}
	server, err := New(Config{
		Address: "127.0.0.1:7331", Version: "test", Insights: evidence,
	}, codeintel.New(testStore{repositories: []catalog.Repository{repository}}, testSearcher{}, "http://127.0.0.1:7331"), testRefresher{})
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range map[string]string{
		"repository_id": "8", "revision": "abc", "branch": "main",
		"format": "sarif", "tool": "Semgrep", "tool_version": "1.2.3",
		"rule_pack": "community@4", "configuration": "ci.yml", "license": "LGPL-2.1",
	} {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile("report", "report.sarif")
	if err != nil {
		t.Fatal(err)
	}
	part.Write([]byte(`{"version":"2.1.0","runs":[]}`))
	writer.Close()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/insights/import", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("import status = %d, body = %s", response.Code, response.Body.String())
	}
	if evidence.lastImport.RepositoryID != 8 || evidence.lastImport.Revision != "abc" ||
		evidence.lastImport.ToolVersion != "1.2.3" || evidence.lastImport.RulePack != "community@4" ||
		evidence.lastImport.License != "LGPL-2.1" {
		t.Fatalf("import = %#v", evidence.lastImport)
	}
}

func TestReaderCannotMutateInsightsAndDoesNotSeeMutationControls(t *testing.T) {
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
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := catalog.Repository{ID: 8, Name: "service", IndexedCommit: "abc", IndexState: "ready"}
	evidence := &testInsightService{}
	server, err := New(Config{
		Address: "0.0.0.0:7331", Version: "test", Insights: evidence, Security: securityManager,
	}, codeintel.New(testStore{repositories: []catalog.Repository{repository}}, testSearcher{}, "https://repo.example.com"), testRefresher{})
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "https://repo.example.com/api/insights/derive", strings.NewReader("repository_id=8"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || evidence.deriveCalls != 0 {
		t.Fatalf("reader derive status = %d, calls = %d, body = %s", response.Code, evidence.deriveCalls, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "https://repo.example.com/insights?repository=8&view=ingestion", nil)
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		strings.Contains(response.Body.String(), `action="/insights/import"`) ||
		strings.Contains(response.Body.String(), `action="/insights/derive"`) ||
		!strings.Contains(response.Body.String(), "Maintainer permission required") {
		t.Fatalf("reader insights page status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestInsightMutationAndComparisonAPIs(t *testing.T) {
	repository := catalog.Repository{
		ID: 8, Name: "service", DefaultRevision: "main",
		IndexedCommit: strings.Repeat("a", 40), IndexState: "ready",
	}
	evidence := &testInsightService{}
	server, err := New(
		Config{Address: "127.0.0.1:7331", Version: "test", Insights: evidence},
		codeintel.New(testStore{repositories: []catalog.Repository{repository}}, testSearcher{}, "http://127.0.0.1:7331"),
		testRefresher{},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, target, body, contentType string) *httptest.ResponseRecorder {
		t.Helper()
		httpRequest := httptest.NewRequest(method, "http://127.0.0.1:7331"+target, strings.NewReader(body))
		if contentType != "" {
			httpRequest.Header.Set("Content-Type", contentType)
		}
		response := httptest.NewRecorder()
		server.server.Handler.ServeHTTP(response, httpRequest)
		return response
	}

	for _, testCase := range []struct {
		method      string
		target      string
		body        string
		contentType string
		status      int
		contains    string
	}{
		{http.MethodPost, "/api/insights/derive", `{"repository_id":8}`, "application/json", http.StatusCreated, `"tool":"derived"`},
		{http.MethodGet, "/api/insights/compare?repository=8&from_revision=old&to_revision=new", "", "", http.StatusOK, `"repository_id":0`},
		{http.MethodGet, "/api/insights/thresholds?repository=8", "", "", http.StatusOK, `"advisory":true`},
		{http.MethodPut, "/api/insights/thresholds", `{"repository_id":8,"key":"coverage.line","operator":">=","value":80,"enabled":true}`, "application/json", http.StatusOK, `"coverage.line"`},
		{http.MethodGet, "/api/insights/sonar", "", "", http.StatusOK, `"connections"`},
		{http.MethodPut, "/api/insights/sonar", `{"repository_id":8,"base_url":"https://sonar.example.com","project_key":"service"}`, "application/json", http.StatusOK, `"project_key":"service"`},
		{http.MethodPost, "/api/insights/sonar/sync", `{"repository_id":8}`, "application/json", http.StatusCreated, `"tool":"SonarQube"`},
		{http.MethodPost, "/insights/derive", "repository_id=8", "application/x-www-form-urlencoded", http.StatusSeeOther, "Stored%20derived%20run"},
		{http.MethodPost, "/insights/threshold", "repository_id=8&key=coverage.line&operator=%3E%3D&value=80&severity=warning", "application/x-www-form-urlencoded", http.StatusSeeOther, "Advisory%20threshold%20saved"},
		{http.MethodPost, "/insights/sonar", "repository_id=8&base_url=https%3A%2F%2Fsonar.example.com&project_key=service&poll_interval_minutes=30&retention_runs=10", "application/x-www-form-urlencoded", http.StatusSeeOther, "SonarQube%20connection%20saved"},
		{http.MethodPost, "/insights/sonar/sync", "repository_id=8", "application/x-www-form-urlencoded", http.StatusSeeOther, "Stored%20SonarQube%20run"},
	} {
		response := request(testCase.method, testCase.target, testCase.body, testCase.contentType)
		if response.Code != testCase.status ||
			(testCase.contains != "" &&
				!strings.Contains(response.Body.String()+response.Header().Get("Location"), testCase.contains)) {
			t.Fatalf("%s %s = %d, location %q, body %s",
				testCase.method, testCase.target, response.Code,
				response.Header().Get("Location"), response.Body.String())
		}
	}
	if evidence.deriveCalls != 2 {
		t.Fatalf("derive calls = %d", evidence.deriveCalls)
	}

	for _, testCase := range []struct {
		method      string
		target      string
		body        string
		contentType string
	}{
		{http.MethodPost, "/api/insights/derive", `{`, "application/json"},
		{http.MethodGet, "/api/insights/compare?repository=bad", "", ""},
		{http.MethodPut, "/api/insights/thresholds", `{`, "application/json"},
		{http.MethodPut, "/api/insights/sonar", `{`, "application/json"},
		{http.MethodGet, "/api/insights?repository=bad", "", ""},
		{http.MethodGet, "/api/insights?limit=6000", "", ""},
		{http.MethodGet, "/api/insights?since=yesterday", "", ""},
	} {
		response := request(testCase.method, testCase.target, testCase.body, testCase.contentType)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid %s = %d: %s", testCase.target, response.Code, response.Body.String())
		}
	}
}

type testInsightService struct {
	response    insights.QueryResponse
	lastFilter  insights.Filter
	lastImport  insights.ImportRequest
	deriveCalls int
}

func (s *testInsightService) Query(_ context.Context, filter insights.Filter) (insights.QueryResponse, error) {
	s.lastFilter = filter
	return s.response, nil
}
func (s *testInsightService) Import(_ context.Context, request insights.ImportRequest) (insights.Run, error) {
	s.lastImport = request
	return insights.Run{Tool: request.Tool, Status: insights.StatusCurrent}, nil
}
func (s *testInsightService) Derive(context.Context, int64) (insights.Run, error) {
	s.deriveCalls++
	return insights.Run{Tool: "derived", Status: insights.StatusCurrent}, nil
}
func (*testInsightService) Compare(context.Context, int64, string, string) (insights.Comparison, error) {
	return insights.Comparison{}, nil
}
func (*testInsightService) Thresholds(context.Context, int64) ([]insights.Threshold, error) {
	return nil, nil
}
func (*testInsightService) SetThreshold(_ context.Context, threshold insights.Threshold) (insights.Threshold, error) {
	return threshold, nil
}
func (*testInsightService) EvaluateThresholds(context.Context, int64) ([]insights.ThresholdEvaluation, error) {
	return nil, nil
}
func (*testInsightService) ConfigureSonar(_ context.Context, connection insights.SonarConnection) (insights.SonarConnection, error) {
	return connection, nil
}
func (*testInsightService) SonarConnections(context.Context) ([]insights.SonarConnection, error) {
	return nil, nil
}
func (*testInsightService) SyncSonar(context.Context, int64) (insights.Run, error) {
	return insights.Run{Tool: "SonarQube", Status: insights.StatusCurrent}, nil
}

func insightNumber(value float64) *float64 { return &value }
