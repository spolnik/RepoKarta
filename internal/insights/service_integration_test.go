package insights_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/insights"
	"github.com/spolnik/RepoKarta/internal/store"
)

func TestImportReconcilesIndexedPathsAndQuarantinesOtherRevisions(t *testing.T) {
	ctx := context.Background()
	storage, repository, revision := insightRepository(t)
	defer storage.Close()
	service := insights.New(storage, "http://127.0.0.1:7331")
	report := []byte("SF:/ci/workspace/service/internal/service.go\nLF:10\nLH:8\nBRF:2\nBRH:1\nend_of_record\n")
	run, err := service.Import(ctx, insights.ImportRequest{
		RepositoryID: repository.ID, Revision: revision, Branch: "main",
		Format: "lcov", Tool: "unit coverage", PathPrefix: "/ci/workspace/service",
		Content: report,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != insights.StatusCurrent {
		t.Fatalf("run status = %q, message = %q", run.Status, run.StatusMessage)
	}
	result, err := service.Query(ctx, insights.Filter{RepositoryID: repository.ID, Rule: "coverage.line", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	foundPinned := false
	for _, observation := range result.Current {
		if observation.Path == "internal/service.go" &&
			strings.Contains(observation.SourceURL, "/source/") &&
			strings.Contains(observation.SourceURL, "rev="+revision) &&
			strings.Contains(observation.SourceURL, "lines=1-200") {
			foundPinned = true
		}
	}
	if !foundPinned {
		t.Fatalf("missing reconciled pinned observation: %#v", result.Current)
	}
	if len(result.History) != 0 {
		t.Fatalf("first run should not also appear as history: %#v", result.History)
	}
	_, err = service.Import(ctx, insights.ImportRequest{
		RepositoryID: repository.ID, Revision: revision, Branch: "main",
		Format: "lcov", Tool: "unit coverage", PathPrefix: "/ci/workspace/service",
		Content: []byte("SF:/ci/workspace/service/internal/service.go\nLF:10\nLH:9\nBRF:2\nBRH:2\nend_of_record\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err = service.Query(ctx, insights.Filter{
		RepositoryID: repository.ID, Rule: "coverage.line", File: "internal/service.go", Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Current) != 1 || len(result.History) != 1 {
		t.Fatalf("current/history split = %d/%d, response = %#v", len(result.Current), len(result.History), result)
	}
	if result.Current[0].Value == nil || *result.Current[0].Value != 90 ||
		result.History[0].Value == nil || *result.History[0].Value != 80 {
		t.Fatalf("current/history values = %#v / %#v", result.Current[0].Value, result.History[0].Value)
	}
	quarantined, err := service.Import(ctx, insights.ImportRequest{
		RepositoryID: repository.ID, Revision: strings.Repeat("f", 40),
		Format: "lcov", Content: report,
	})
	if err != nil {
		t.Fatal(err)
	}
	if quarantined.Status != insights.StatusQuarantined {
		t.Fatalf("quarantined status = %q", quarantined.Status)
	}
	result, err = service.Query(ctx, insights.Filter{RepositoryID: repository.ID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, listed := range result.Runs {
		if listed.ID == quarantined.ID {
			t.Fatalf("quarantined run leaked into default query: %#v", result.Runs)
		}
	}
}

func TestQueryMarksEvidenceStaleAfterIndexedRevisionAdvances(t *testing.T) {
	ctx := context.Background()
	storage, repository, revision := insightRepository(t)
	defer storage.Close()
	service := insights.New(storage, "http://127.0.0.1:7331")
	if _, err := service.Import(ctx, insights.ImportRequest{
		RepositoryID: repository.ID,
		Revision:     revision,
		Format:       "lcov",
		Content:      []byte("SF:internal/service.go\nLF:10\nLH:8\nend_of_record\n"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository.Path, "NEW.md"), []byte("new revision\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository.Path, "add", "NEW.md")
	runGit(t, repository.Path, "commit", "-m", "advance indexed revision")
	nextRevision := strings.TrimSpace(runGit(t, repository.Path, "rev-parse", "HEAD"))
	if err := storage.UpdateIndexState(ctx, repository.ID, "ready", nextRevision, ""); err != nil {
		t.Fatal(err)
	}

	result, err := service.Query(ctx, insights.Filter{RepositoryID: repository.ID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Current) != 0 || len(result.History) == 0 || len(result.Warnings) == 0 {
		t.Fatalf("stale current/history/warnings = %d/%d/%#v", len(result.Current), len(result.History), result.Warnings)
	}
	if len(result.Runs) != 1 || result.Runs[0].Status != insights.StatusStale ||
		!strings.Contains(result.Runs[0].StatusMessage, nextRevision[:8]) {
		t.Fatalf("stale runs = %#v", result.Runs)
	}

	historical, err := service.Query(ctx, insights.Filter{
		RepositoryID: repository.ID, Revision: revision, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(historical.Current) == 0 || len(historical.Runs) != 1 ||
		historical.Runs[0].Status != insights.StatusStale {
		t.Fatalf("explicit historical revision = %#v", historical)
	}
}

func TestDeriveReadsCommittedSourceWithoutRunningRepositoryScripts(t *testing.T) {
	ctx := context.Background()
	storage, repository, _ := insightRepository(t)
	defer storage.Close()
	marker := filepath.Join(repository.Path, "should-not-exist")
	if err := os.WriteFile(filepath.Join(repository.Path, "build.cmd"), []byte("@echo touched > should-not-exist"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := insights.New(storage, "http://127.0.0.1:7331")
	run, err := service.Derive(ctx, repository.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Tool != "RepoKarta deterministic syntax indicators" || run.Confidence != "syntax_approximation" {
		t.Fatalf("run = %#v", run)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repository build script appears to have run: %v", err)
	}
	result, err := service.Query(ctx, insights.Filter{RepositoryID: repository.ID, Tool: run.Tool, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, observation := range result.Current {
		if observation.Key == "complexity.branch_points" && observation.State == insights.StateDerived {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing derived complexity observation: %#v", result.Current)
	}
}

func TestSonarSyncImportsMeasuresIssuesAndOriginatingQualityGate(t *testing.T) {
	ctx := context.Background()
	storage, repository, revision := insightRepository(t)
	defer storage.Close()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		user, _, ok := request.BasicAuth()
		if !ok || user != "secret-token" {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/project_analyses/search":
			fmt.Fprintf(response, `{"analyses":[{"key":"analysis-1","date":"2026-07-26T12:00:00Z","revision":%q}]}`, revision)
		case "/api/measures/component":
			fmt.Fprint(response, `{"component":{"measures":[{"metric":"coverage","value":"82.5"},{"metric":"ncloc","value":"120"}]}}`)
		case "/api/issues/search":
			fmt.Fprint(response, `{"issues":[{"key":"issue-1","rule":"go:S100","severity":"MAJOR","component":"service:internal/service.go","message":"Simplify this code","hash":"fp-1","status":"OPEN","textRange":{"startLine":2,"endLine":2}}]}`)
		case "/api/qualitygates/project_status":
			fmt.Fprint(response, `{"projectStatus":{"status":"ERROR","conditions":[{"metricKey":"coverage","status":"ERROR"}]}}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("REPOKARTA_TEST_SONAR_TOKEN", "secret-token")
	service := insights.New(storage, "http://127.0.0.1:7331")
	_, err := service.ConfigureSonar(ctx, insights.SonarConnection{
		RepositoryID: repository.ID, BaseURL: server.URL, ProjectKey: "service",
		TokenEnv: "REPOKARTA_TEST_SONAR_TOKEN", PollIntervalMinutes: 15,
		RetentionRuns: 10, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := service.SyncSonar(ctx, repository.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != insights.StatusCurrent || run.ObservationCount != 4 {
		t.Fatalf("run = %#v", run)
	}
	result, err := service.Query(ctx, insights.Filter{RepositoryID: repository.ID, Tool: run.Tool, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	foundIssue, foundGate := false, false
	for _, observation := range result.Current {
		if observation.Key == "go:S100" && observation.SourceURL != "" {
			foundIssue = true
		}
		if observation.Key == "sonar.quality_gate" && observation.Value != nil && *observation.Value == 0 {
			foundGate = true
		}
	}
	if !foundIssue || !foundGate {
		t.Fatalf("Sonar observations = %#v", result.Current)
	}
}

func insightRepository(t *testing.T) (*store.Store, catalog.Repository, string) {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "RepoKarta Test")
	if err := os.MkdirAll(filepath.Join(root, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "service.go"), []byte("package internal\n\nfunc service(value bool) int {\n\tif value { return 1 }\n\treturn 0\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "internal/service.go")
	runGit(t, root, "commit", "-m", "initial")
	revision := strings.TrimSpace(runGit(t, root, "rev-parse", "HEAD"))
	storage, err := store.Open(filepath.Join(t.TempDir(), "repokarta.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.SyncRepositories(context.Background(), []catalog.Repository{{
		Name: "service", Path: root, DefaultRevision: "main",
		HeadCommit: revision, IndexedCommit: revision,
		ScanState: "ready", IndexState: "ready",
		DiscoveredAt: time.Now().UTC(), ScannedAt: time.Now().UTC(), IndexedAt: time.Now().UTC(),
	}}); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	repositories, err := storage.ListRepositories(context.Background())
	if err != nil || len(repositories) != 1 {
		storage.Close()
		t.Fatalf("repositories = %#v, err = %v", repositories, err)
	}
	if err := storage.UpdateIndexState(context.Background(), repositories[0].ID, "ready", revision, ""); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	repositories, err = storage.ListRepositories(context.Background())
	if err != nil || len(repositories) != 1 {
		storage.Close()
		t.Fatalf("indexed repositories = %#v, err = %v", repositories, err)
	}
	return storage, repositories[0], revision
}

func runGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return string(output)
}
