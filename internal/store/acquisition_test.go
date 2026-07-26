package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/acquisition"
	"github.com/spolnik/RepoKarta/internal/catalog"
)

func TestAcquisitionRegistryRoundTripAndRemovalAuditSurvives(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "acquisition.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	repository, err := storage.UpsertAcquisition(ctx, acquisition.Repository{
		Provider:             acquisition.ProviderGitHub,
		ProviderRepositoryID: "4242",
		CanonicalID:          "github.com/acme/example",
		Name:                 "example",
		Namespace:            "acme",
		RemoteURL:            "https://github.com/acme/example.git",
		CheckoutPath:         filepath.Join(t.TempDir(), "repositories", "github", "acme", "example"),
		DefaultBranch:        "main",
		CredentialRef:        "GITHUB_TOKEN",
		InclusionPolicy:      "approved; team=platform",
		Visibility:           "private",
		Owned:                true,
		State:                acquisition.StateReady,
		HeadCommit:           strings.Repeat("a", 40),
		CreatedAt:            now,
		DiscoveredAt:         now,
		SyncedAt:             now,
		UpdatedAt:            now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if repository.ID <= 0 || repository.CredentialRef != "GITHUB_TOKEN" ||
		repository.ProviderRepositoryID != "4242" ||
		repository.InclusionPolicy != "approved; team=platform" ||
		repository.RemoteURL != "https://github.com/acme/example.git" {
		t.Fatalf("stored acquisition = %#v", repository)
	}
	if err := storage.SyncRepositories(ctx, []catalog.Repository{{
		Name:         repository.Name,
		Path:         repository.CheckoutPath,
		HeadCommit:   repository.HeadCommit,
		ScanState:    "ready",
		IndexState:   "pending",
		DiscoveredAt: now,
		ScannedAt:    now,
	}}); err != nil {
		t.Fatal(err)
	}
	catalogue, err := storage.ListRepositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalogue) != 1 || catalogue[0].AcquisitionID != repository.ID {
		t.Fatalf("catalogue acquisition provenance = %#v", catalogue)
	}
	repository.State = acquisition.StateError
	repository.LastError = "credential helper unavailable"
	repository.FailureCount = 2
	repository.NextSyncAt = now.Add(2 * time.Minute)
	repository, err = storage.UpsertAcquisition(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	list, err := storage.ListAcquisitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].State != acquisition.StateError ||
		list[0].FailureCount != 2 || list[0].LastError == "" {
		t.Fatalf("acquisition list = %#v", list)
	}
	if err := storage.RecordAcquisitionEvent(ctx, acquisition.Event{
		RepositoryID: repository.ID,
		CanonicalID:  repository.CanonicalID,
		Action:       "remove",
		Outcome:      "success",
		Revision:     repository.HeadCommit,
		CreatedAt:    now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteAcquisition(ctx, repository.ID); err != nil {
		t.Fatal(err)
	}
	var eventCount int
	if err := storage.db.QueryRow(`SELECT count(*) FROM repository_acquisition_events WHERE action = 'remove'`).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("removal event count = %d", eventCount)
	}
}

func TestAcquisitionRegistryNeverStoresCredentialValue(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "credentials.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	secret := "super-secret-provider-token"
	repository, err := storage.UpsertAcquisition(context.Background(), acquisition.Repository{
		Provider:      acquisition.ProviderGitLab,
		CanonicalID:   "gitlab.com/acme/example",
		Name:          "example",
		CheckoutPath:  filepath.Join(t.TempDir(), "example"),
		CredentialRef: "GITLAB_TOKEN",
		State:         acquisition.StateAcquiring,
	})
	if err != nil {
		t.Fatal(err)
	}
	var serialized string
	if err := storage.db.QueryRow(`
SELECT provider || canonical_id || name || namespace || remote_url || web_url ||
       checkout_path || default_branch || credential_ref || visibility ||
       last_error || head_commit
FROM repository_acquisitions WHERE id = ?`, repository.ID).Scan(&serialized); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(serialized, secret) || !strings.Contains(serialized, "GITLAB_TOKEN") {
		t.Fatalf("credential storage = %q", serialized)
	}
}
