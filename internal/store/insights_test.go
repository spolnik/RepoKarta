package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/insights"
)

func TestInsightRunsPersistFilterCompareEvidenceAndRetention(t *testing.T) {
	ctx := context.Background()
	storage, err := Open(filepath.Join(t.TempDir(), "insights.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	repositories := []catalog.Repository{{
		Name: "service", Path: filepath.Join(t.TempDir(), "service"),
		HeadCommit: "abc", IndexedCommit: "abc", ScanState: "ready", IndexState: "ready",
		DiscoveredAt: time.Now().UTC(), ScannedAt: time.Now().UTC(), IndexedAt: time.Now().UTC(),
	}}
	if err := storage.SyncRepositories(ctx, repositories); err != nil {
		t.Fatal(err)
	}
	repository := mustRepositoryByName(t, storage, "service")
	now := time.Now().UTC().Truncate(time.Second)
	for index, revision := range []string{"abc", "def"} {
		run := insights.Run{
			ID: "run-" + revision, RepositoryID: repository.ID, Revision: revision,
			Tool: "scanner", SourceKind: "uploaded_report", Status: insights.StatusCurrent,
			Confidence: "reported", ObservedAt: now.Add(time.Duration(index) * time.Minute),
			IngestedAt: now.Add(time.Duration(index) * time.Minute),
		}
		value := float64(80 + index)
		observations := []insights.Observation{{
			Kind: insights.KindMetric, Key: "coverage.line", Value: &value,
			Unit: "percent", State: insights.StateMeasured, Confidence: "reported",
			Path: "service.go", ObservedAt: run.ObservedAt,
		}}
		if err := storage.SaveInsightRun(ctx, run, observations); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := storage.ListInsightRuns(ctx, insights.Filter{RepositoryID: repository.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].Revision != "def" || runs[0].ObservationCount != 1 {
		t.Fatalf("runs = %#v", runs)
	}
	observations, err := storage.ListInsightObservations(ctx, insights.Filter{
		RepositoryID: repository.ID, Revision: "abc", Rule: "coverage.line", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || observations[0].Value == nil || *observations[0].Value != 80 {
		t.Fatalf("observations = %#v", observations)
	}
	if err := storage.DeleteOldInsightRuns(ctx, repository.ID, "scanner", 1); err != nil {
		t.Fatal(err)
	}
	runs, err = storage.ListInsightRuns(ctx, insights.Filter{RepositoryID: repository.ID, Limit: 10})
	if err != nil || len(runs) != 1 || runs[0].Revision != "def" {
		t.Fatalf("retained runs = %#v, err = %v", runs, err)
	}
}

func TestInsightRetentionBoundsUniqueToolNamesPerRepository(t *testing.T) {
	ctx := context.Background()
	storage, err := Open(filepath.Join(t.TempDir(), "insights.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if err := storage.SyncRepositories(ctx, []catalog.Repository{{
		Name: "service", Path: filepath.Join(t.TempDir(), "service"),
		DiscoveredAt: time.Now().UTC(), ScanState: "ready", IndexState: "ready",
	}}); err != nil {
		t.Fatal(err)
	}
	repository := mustRepositoryByName(t, storage, "service")
	tx, err := storage.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for index := 0; index < maximumInsightRunsPerRepository+5; index++ {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO insight_runs (
    id, repository_id, revision, tool, source_kind, status, confidence,
    observed_at, ingested_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("run-%04d", index),
			repository.ID,
			"abc",
			fmt.Sprintf("tool-%04d", index),
			"test",
			insights.StatusCurrent,
			"reported",
			formatTime(now.Add(time.Duration(index)*time.Second)),
			formatTime(now.Add(time.Duration(index)*time.Second)),
		); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteOldInsightRuns(ctx, repository.ID, "tool-1004", 50); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := storage.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM insight_runs WHERE repository_id = ?",
		repository.ID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != maximumInsightRunsPerRepository {
		t.Fatalf("retained unique-tool runs = %d, want %d", count, maximumInsightRunsPerRepository)
	}
}

func TestInsightThresholdAndSonarConfigurationStoreCredentialReferenceOnly(t *testing.T) {
	ctx := context.Background()
	storage, err := Open(filepath.Join(t.TempDir(), "insights.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if err := storage.SyncRepositories(ctx, []catalog.Repository{{
		Name: "service", Path: filepath.Join(t.TempDir(), "service"),
		DiscoveredAt: time.Now().UTC(), ScanState: "ready", IndexState: "ready",
	}}); err != nil {
		t.Fatal(err)
	}
	repository := mustRepositoryByName(t, storage, "service")
	threshold, err := storage.UpsertInsightThreshold(ctx, insights.Threshold{
		RepositoryID: repository.ID, Key: "coverage.line", Operator: "lt",
		Value: 80, Severity: "warning", Enabled: true,
	})
	if err != nil || threshold.ID == 0 {
		t.Fatalf("threshold = %#v, err = %v", threshold, err)
	}
	connection, err := storage.UpsertSonarConnection(ctx, insights.SonarConnection{
		RepositoryID: repository.ID, BaseURL: "https://sonar.example.com",
		ProjectKey: "service", TokenEnv: "REPOKARTA_SONAR_TOKEN",
		PollIntervalMinutes: 15, RetentionRuns: 25, Enabled: true,
		State: insights.StatusStale,
	})
	if err != nil {
		t.Fatal(err)
	}
	connections, err := storage.ListSonarConnections(ctx, false)
	if err != nil || len(connections) != 1 {
		t.Fatalf("connections = %#v, err = %v", connections, err)
	}
	if connections[0].ID != connection.ID || connections[0].TokenEnv != "REPOKARTA_SONAR_TOKEN" ||
		connections[0].RetentionRuns != 25 {
		t.Fatalf("connection = %#v", connections[0])
	}
}

func mustRepositoryByName(t *testing.T, storage *Store, name string) catalog.Repository {
	t.Helper()
	repositories, err := storage.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, repository := range repositories {
		if repository.Name == name {
			return repository
		}
	}
	t.Fatalf("repository %q not found", name)
	return catalog.Repository{}
}
