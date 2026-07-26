package httpserver

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/codeintel"
	"github.com/spolnik/RepoKarta/internal/insights"
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

type testInsightService struct {
	response   insights.QueryResponse
	lastFilter insights.Filter
	lastImport insights.ImportRequest
}

func (s *testInsightService) Query(_ context.Context, filter insights.Filter) (insights.QueryResponse, error) {
	s.lastFilter = filter
	return s.response, nil
}
func (s *testInsightService) Import(_ context.Context, request insights.ImportRequest) (insights.Run, error) {
	s.lastImport = request
	return insights.Run{Tool: request.Tool, Status: insights.StatusCurrent}, nil
}
func (*testInsightService) Derive(context.Context, int64) (insights.Run, error) {
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
