package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/spolnik/RepoKarta/internal/access"
	"github.com/spolnik/RepoKarta/internal/searchworkspace"
)

func TestSearchWorkspaceIsAuthorScopedAndMonitorHistoryIsBounded(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "repokarta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()

	alice := access.WithViewer(context.Background(), access.Viewer{ID: "saml:alice"})
	bob := access.WithViewer(context.Background(), access.Viewer{ID: "saml:bob"})
	admin := access.WithViewer(context.Background(), access.Viewer{ID: "local:admin", Admin: true})
	for index := 0; index < 55; index++ {
		if err := storage.AddRecentSearch(alice, searchworkspace.RecentRecord{
			RequestJSON: `{"query":"owner:platform"}`,
			ResultCount: index,
		}); err != nil {
			t.Fatal(err)
		}
	}
	recent, err := storage.ListRecentSearches(alice, 50)
	if err != nil || len(recent) != 50 {
		t.Fatalf("recent searches = %d, %v", len(recent), err)
	}
	if other, err := storage.ListRecentSearches(bob, 50); err != nil || len(other) != 0 {
		t.Fatalf("Bob recent searches = %#v, %v", other, err)
	}

	personal, err := storage.CreateSavedSearchRecord(alice, searchworkspace.SavedRecord{
		Title: "Platform ownership", Visibility: "personal",
		RevisionPolicy: "latest_indexed", RequestJSON: `{"query":"owner:platform"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetSavedSearchRecord(bob, personal.ID); !errors.Is(err, searchworkspace.ErrNotFound) {
		t.Fatalf("cross-author personal read error = %v", err)
	}
	if _, err := storage.CreateSavedSearchRecord(alice, searchworkspace.SavedRecord{
		Title: "Team template", Visibility: "shared",
		RevisionPolicy: "pinned", RequestJSON: `{"query":"result_type:route"}`,
	}); !errors.Is(err, searchworkspace.ErrForbidden) {
		t.Fatalf("non-admin shared template error = %v", err)
	}
	shared, err := storage.CreateSavedSearchRecord(admin, searchworkspace.SavedRecord{
		Title: "Team template", Visibility: "shared",
		RevisionPolicy: "pinned", RequestJSON: `{"query":"result_type:route"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if visible, err := storage.GetSavedSearchRecord(bob, shared.ID); err != nil || !visible.Managed {
		t.Fatalf("shared template = %#v, %v", visible, err)
	}

	monitor, err := storage.UpsertSearchMonitorRecord(alice, searchworkspace.MonitorRecord{
		SavedSearchID: personal.ID, Enabled: true, HistoryLimit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		if _, err := storage.AddSearchMonitorRun(alice, searchworkspace.RunRecord{
			MonitorID: monitor.ID, RevisionKey: "7:abc",
			ResultKeysJSON: `[]`, AddedJSON: `[]`, RemovedJSON: `[]`,
			MatchCount: index, Status: "complete",
			NotificationStatus: "not_configured",
		}, monitor.HistoryLimit); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := storage.ListSearchMonitorRuns(alice, monitor.ID, 10)
	if err != nil || len(runs) != 2 || runs[0].NotificationStatus != "not_configured" {
		t.Fatalf("bounded monitor runs = %#v, %v", runs, err)
	}
}
