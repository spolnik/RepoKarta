package codeintel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

func TestUnifiedRepositoryCommitAndDiffResultsUsePermissionFilteredEvidence(t *testing.T) {
	directory := t.TempDir()
	filePath := filepath.Join(directory, "service.go")
	if err := os.WriteFile(filePath, []byte("package service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, directory, "init", "-q")
	runGit(t, directory, "add", ".")
	runGit(
		t,
		directory,
		"-c", "user.name=RepoKarta Test",
		"-c", "user.email=test@repokarta.local",
		"commit", "--date=2026-01-01T10:00:00Z", "-qm", "initial service",
	)
	firstRevision := strings.TrimSpace(runGit(t, directory, "rev-parse", "HEAD"))
	if err := os.WriteFile(
		filePath,
		[]byte("package service\n\nfunc Needle() {}\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	runGit(t, directory, "add", ".")
	runGit(
		t,
		directory,
		"-c", "user.name=Search Author",
		"-c", "user.email=search@example.test",
		"commit", "--date=2026-02-01T10:00:00Z", "-qm", "add needle endpoint",
	)
	needleRevision := strings.TrimSpace(runGit(t, directory, "rev-parse", "HEAD"))
	if err := os.WriteFile(
		filePath,
		[]byte("package service\n\nfunc Found() {}\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	runGit(t, directory, "add", ".")
	runGit(
		t,
		directory,
		"-c", "user.name=Release Author",
		"-c", "user.email=release@example.test",
		"commit", "--date=2026-03-01T10:00:00Z", "-qm", "replace needle endpoint",
	)
	revision := strings.TrimSpace(runGit(t, directory, "rev-parse", "HEAD"))
	branch := strings.TrimSpace(runGit(t, directory, "symbolic-ref", "--short", "HEAD"))
	repository := catalog.Repository{
		ID:              41,
		Name:            "payments",
		Path:            directory,
		OriginURL:       "https://example.test/payments.git",
		DefaultRevision: "main",
		HeadCommit:      revision,
		IndexedCommit:   revision,
		ScanState:       "ready",
		IndexState:      "ready",
	}
	searcher := &capturingSearcher{}
	service := New(referenceTestStore{repository: repository}, searcher, "http://localhost:7331")

	repositories, err := service.Search(context.Background(), SearchRequest{
		Query: "payment result_type:repository",
		Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if repositories.ResultType != "repository" ||
		repositories.ReturnedItems != 1 ||
		len(repositories.Items) != 1 ||
		repositories.Items[0].RepositoryID != repository.ID ||
		!strings.Contains(repositories.Items[0].SourceURL, "/maps?repository=41") ||
		len(repositories.Matches) != 0 {
		t.Fatalf("repository results = %#v", repositories)
	}

	commits, err := service.Search(context.Background(), SearchRequest{
		Query: "result_type:commit repository:payments path:service.go " +
			"author:\"Search Author\" message:\"add needle\" " +
			"after:2026-02-01 before:2026-02-01 branch:" + branch,
		Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if commits.ResultType != "commit" ||
		commits.ReturnedItems != 1 ||
		len(commits.Items) != 1 ||
		commits.Items[0].Revision != needleRevision ||
		commits.Items[0].Title != "add needle endpoint" ||
		!strings.Contains(commits.Items[0].SourceURL, "/api/git/log/41") {
		t.Fatalf("commit results = %#v", commits)
	}

	diffs, err := service.Search(context.Background(), SearchRequest{
		Query: "result_type:diff repository:payments file:service.go " +
			"added:\"func Needle\" to:" + needleRevision,
		Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if diffs.ResultType != "diff" ||
		diffs.ReturnedItems != 1 ||
		len(diffs.Items) != 1 ||
		diffs.Items[0].Revision != needleRevision ||
		!strings.Contains(diffs.Items[0].Detail, "+func Needle()") ||
		!strings.Contains(diffs.Items[0].SourceURL, "/api/git/diff/41") {
		t.Fatalf("diff results = %#v", diffs)
	}

	rangeDiffs, err := service.Search(context.Background(), SearchRequest{
		Query: "result_type:diff repository:payments path:service.go " +
			"added:\"func Found\" removed:\"func Needle\" " +
			"from:" + needleRevision + " to:" + revision,
		Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rangeDiffs.ReturnedItems != 1 ||
		rangeDiffs.Items[0].Revision != revision ||
		searchItemMetadata(rangeDiffs.Items[0], "from") != needleRevision ||
		searchItemMetadata(rangeDiffs.Items[0], "to") != revision {
		t.Fatalf("range diff results = %#v", rangeDiffs)
	}

	boundedCommits, err := service.Search(context.Background(), SearchRequest{
		Query: "result_type:commit repository:payments from:" + firstRevision +
			" to:" + revision + " -author:\"Release Author\"",
		Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if boundedCommits.ReturnedItems != 1 ||
		boundedCommits.Items[0].Revision != needleRevision {
		t.Fatalf("bounded commit results = %#v", boundedCommits)
	}
	if searcher.query.Text != "" {
		t.Fatalf("entity results unexpectedly reached the source index: %#v", searcher.query)
	}
}

func TestUnifiedGitResultsRejectFiltersThatWouldBroadenEvidence(t *testing.T) {
	repository := catalog.Repository{
		ID: 1, Name: "visible", Path: t.TempDir(),
		IndexedCommit: strings.Repeat("a", 40),
	}
	service := New(referenceTestStore{repository: repository}, &capturingSearcher{}, "")
	for _, query := range []string{
		"result_type:commit -path:internal",
		"result_type:diff language:Go",
		"result_type:repository owner:team",
		"result_type:commit after:not-a-date",
		"result_type:diff branch:main to:aaaaaaaa",
	} {
		if _, err := service.Search(t.Context(), SearchRequest{Query: query}); err == nil {
			t.Fatalf("Search(%q) unexpectedly succeeded", query)
		}
	}
}
