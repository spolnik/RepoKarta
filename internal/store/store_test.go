package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/access"
	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/contextscope"
	_ "modernc.org/sqlite"
)

func TestConversationTranscriptPersistsAcrossDatabaseReopen(t *testing.T) {
	dataDirectory := t.TempDir()
	databasePath := filepath.Join(dataDirectory, "repokarta.db")
	storage, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	conversation := agent.Conversation{
		ID:       "conversation-1",
		Title:    "Trace authentication",
		Provider: "anthropic-api",
		Model:    "claude-sonnet-5",
		Author: agent.ConversationAuthor{
			ID:       "saml:alice",
			Name:     "Alice Example",
			Email:    "alice@example.com",
			Provider: "saml",
			Groups:   []string{"engineering"},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := storage.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	image := agent.Image{
		Name:      `..\diagram.png`,
		MediaType: "image/png",
		Data:      "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=",
	}
	if _, err := storage.AppendMessage(ctx, agent.Message{
		ConversationID: conversation.ID,
		Role:           agent.RoleUser,
		Text:           "Where is authentication configured?",
		Images:         []agent.Image{image},
		Contexts: []contextscope.Context{{
			Kind:         contextscope.KindFile,
			RepositoryID: 42,
			Repository:   "RepoKarta",
			Revision:     strings.Repeat("a", 40),
			Path:         "internal/app/app.go",
			Label:        "@RepoKarta:internal/app/app.go",
		}, {
			Kind:         contextscope.KindSymbol,
			RepositoryID: 42,
			Repository:   "RepoKarta",
			Revision:     strings.Repeat("a", 40),
			Path:         "internal/app/app.go",
			Symbol:       "New",
			SymbolKind:   "function",
			Line:         14,
			StartLine:    14,
			EndLine:      22,
			Label:        "@RepoKarta:internal/app/app.go#New:14",
		}},
		CreatedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.AppendMessage(ctx, agent.Message{
		ConversationID: conversation.ID,
		Role:           agent.RoleAssistant,
		Text:           "It is configured here.",
		Sources: []agent.Citation{{
			Label: "internal/auth/config.go:10-20",
			URL:   "/source/1?path=internal%2Fauth%2Fconfig.go&lines=10-20",
		}},
		InputTokens:  120,
		OutputTokens: 30,
		CreatedAt:    now.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := storage.UpdateConversationCursor(ctx, conversation.ID, "opaque-provider-cursor"); err != nil {
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}

	storage, err = Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	got, err := storage.GetConversation(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != conversation.Title || got.ResumeCursor != "opaque-provider-cursor" ||
		!reflect.DeepEqual(got.Author, conversation.Author) {
		t.Fatalf("conversation metadata = %#v", got)
	}
	if got.MessageCount != 2 || got.InputTokens != 120 || got.OutputTokens != 30 {
		t.Fatalf("conversation totals = %#v", got)
	}
	if len(got.Messages) != 2 || len(got.Messages[0].Images) != 1 || len(got.Messages[1].Sources) != 1 {
		t.Fatalf("conversation transcript = %#v", got.Messages)
	}
	if len(got.Messages[0].Contexts) != 2 ||
		got.Messages[0].Contexts[0].Path != "internal/app/app.go" ||
		got.Messages[0].Contexts[0].Revision != strings.Repeat("a", 40) ||
		got.Messages[0].Contexts[1].Symbol != "New" ||
		got.Messages[0].Contexts[1].Line != 14 ||
		got.Messages[0].Contexts[1].StartLine != 14 ||
		got.Messages[0].Contexts[1].EndLine != 22 {
		t.Fatalf("persisted contexts = %#v", got.Messages[0].Contexts)
	}
	if got.Messages[0].Images[0].Name != "diagram.png" || got.Messages[0].Images[0].Data != image.Data {
		t.Fatalf("persisted image = %#v", got.Messages[0].Images[0])
	}
	imagePath := filepath.Join(storage.conversationDirectory, "1-1.png")
	if _, err := os.Stat(imagePath); err != nil {
		t.Fatalf("persisted image file: %v", err)
	}
	if err := storage.CreateConversation(ctx, agent.Conversation{
		ID:       "conversation-2",
		Title:    "Bob's conversation",
		Provider: "anthropic-api",
		Author: agent.ConversationAuthor{
			ID:       "saml:bob",
			Name:     "Bob Example",
			Provider: "saml",
		},
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	own, err := storage.ListConversations(ctx, agent.ConversationFilter{AuthorID: "saml:alice"})
	if err != nil || len(own) != 1 || own[0].Author.ID != "saml:alice" {
		t.Fatalf("own conversations = %#v, error = %v", own, err)
	}
	bob, err := storage.ListConversations(ctx, agent.ConversationFilter{AuthorID: "saml:bob"})
	if err != nil || len(bob) != 1 || bob[0].Author.ID != "saml:bob" {
		t.Fatalf("bob conversations = %#v, error = %v", bob, err)
	}
	if err := storage.DeleteConversation(ctx, conversation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(imagePath); !os.IsNotExist(err) {
		t.Fatalf("conversation image still exists after delete: %v", err)
	}
}

func TestMigrationFromEnterpriseSchemaAddsStructuredContexts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema-12.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for index, migration := range []string{
		schemaV1,
		schemaV2,
		schemaV3,
		schemaV4,
		schemaV5,
		schemaV6,
		schemaV7,
		schemaV8,
		schemaV9,
		schemaV10,
		schemaV11,
		schemaV12,
	} {
		if _, err := legacy.Exec(migration); err != nil {
			t.Fatalf("apply legacy migration %d: %v", index+1, err)
		}
	}
	if _, err := legacy.Exec("PRAGMA user_version = 12;"); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	storage, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()

	var version int
	if err := storage.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, currentSchemaVersion)
	}
	rows, err := storage.db.Query("SELECT contexts_json FROM conversation_messages LIMIT 0")
	if err != nil {
		t.Fatalf("structured context column was not added after schema 12: %v", err)
	}
	rows.Close()
}

func TestRepositoryAccessDefaultsPrivateAndSupportsUserGroupAndSharedScopes(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "repokarta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	ctx := context.Background()
	if err := storage.SyncRepositories(ctx, []catalog.Repository{{
		Name: "private-repo", Path: filepath.Join(t.TempDir(), "private-repo"),
		ScanState: "ready", DiscoveredAt: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	all, err := storage.ListRepositories(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("trusted repository list = %#v, error = %v", all, err)
	}
	repositoryID := all[0].ID
	alice := access.WithViewer(ctx, access.Viewer{ID: "saml:alice"})
	if repositories, err := storage.ListRepositories(alice); err != nil || len(repositories) != 0 {
		t.Fatalf("default private visibility = %#v, error = %v", repositories, err)
	}
	if _, err := storage.RepositoryByID(alice, repositoryID); err == nil ||
		!strings.Contains(err.Error(), "not indexed") {
		t.Fatalf("private repository lookup error = %v", err)
	}

	if err := storage.SetRepositoryAccess(ctx, RepositoryAccess{
		RepositoryID: repositoryID,
		OwnerID:      "saml:owner",
		Visibility:   access.VisibilityPrivate,
		Users:        []string{"saml:alice"},
		Groups:       []string{"engineering"},
	}); err != nil {
		t.Fatal(err)
	}
	if repositories, err := storage.ListRepositories(alice); err != nil || len(repositories) != 1 {
		t.Fatalf("user grant visibility = %#v, error = %v", repositories, err)
	}
	bob := access.WithViewer(ctx, access.Viewer{ID: "saml:bob", Groups: []string{"engineering"}})
	if repositories, err := storage.ListRepositories(bob); err != nil || len(repositories) != 1 {
		t.Fatalf("group grant visibility = %#v, error = %v", repositories, err)
	}
	outsider := access.WithViewer(ctx, access.Viewer{ID: "saml:outsider"})
	if repositories, err := storage.ListRepositories(outsider); err != nil || len(repositories) != 0 {
		t.Fatalf("outsider visibility = %#v, error = %v", repositories, err)
	}
	if err := storage.SetRepositoryAccess(ctx, RepositoryAccess{
		RepositoryID: repositoryID,
		OwnerID:      "saml:owner",
		Visibility:   access.VisibilityShared,
	}); err != nil {
		t.Fatal(err)
	}
	if repositories, err := storage.ListRepositories(outsider); err != nil || len(repositories) != 1 {
		t.Fatalf("shared visibility = %#v, error = %v", repositories, err)
	}
	admin := access.WithViewer(ctx, access.Viewer{ID: "local:admin", Admin: true})
	if repositories, err := storage.ListRepositories(admin); err != nil || len(repositories) != 1 {
		t.Fatalf("administrator visibility = %#v, error = %v", repositories, err)
	}
}

func TestOpenMigratesM0DatabaseAndPreservesIndexStateAcrossScans(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "repokarta.db")
	legacy, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(schemaV1); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(
		"INSERT INTO repositories (name, path, discovered_at) VALUES (?, ?, ?)",
		"legacy",
		filepath.Join(t.TempDir(), "legacy"),
		time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	storage, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()

	repository := catalog.Repository{
		Name:            "repo",
		Path:            filepath.Join(t.TempDir(), "repo"),
		HeadCommit:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DefaultRevision: "main",
		ScanState:       "ready",
		DiscoveredAt:    time.Now(),
		ScannedAt:       time.Now(),
	}
	if err := storage.SyncRepositories(context.Background(), []catalog.Repository{repository}); err != nil {
		t.Fatal(err)
	}
	repositories, err := storage.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 1 || repositories[0].IndexState != "pending" {
		t.Fatalf("unexpected repositories after sync: %#v", repositories)
	}

	if err := storage.UpdateIndexState(
		context.Background(),
		repositories[0].ID,
		"ready",
		repository.HeadCommit,
		"",
	); err != nil {
		t.Fatal(err)
	}
	if err := storage.SyncRepositories(context.Background(), []catalog.Repository{repository}); err != nil {
		t.Fatal(err)
	}
	repositories, err = storage.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if repositories[0].IndexState != "ready" || repositories[0].IndexedCommit != repository.HeadCommit {
		t.Fatalf("expected ready index state to survive unchanged scan, got %#v", repositories[0])
	}

	repository.HeadCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := storage.SyncRepositories(context.Background(), []catalog.Repository{repository}); err != nil {
		t.Fatal(err)
	}
	repositories, err = storage.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if repositories[0].IndexState != "pending" {
		t.Fatalf("expected changed commit to become pending, got %#v", repositories[0])
	}
}

func TestEmptyRepositoryRemainsTerminalAcrossScansAndConfigurationChanges(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "repokarta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()

	repository := catalog.Repository{
		Name:         "empty",
		Path:         filepath.Join(t.TempDir(), "empty"),
		ScanState:    "empty",
		ScanError:    catalog.EmptyRepositoryReason,
		IndexState:   "empty",
		IndexError:   catalog.EmptyRepositoryReason,
		DiscoveredAt: time.Now(),
		ScannedAt:    time.Now(),
	}
	ctx := context.Background()
	if err := storage.SyncRepositories(ctx, []catalog.Repository{repository}); err != nil {
		t.Fatal(err)
	}
	assertEmpty := func(stage string) {
		t.Helper()
		repositories, listErr := storage.ListRepositories(ctx)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(repositories) != 1 ||
			repositories[0].ScanState != "empty" ||
			repositories[0].IndexState != "empty" ||
			repositories[0].IndexError != catalog.EmptyRepositoryReason {
			t.Fatalf("%s empty repository = %#v", stage, repositories)
		}
	}
	assertEmpty("initial sync")

	if changed, err := storage.EnsureIndexConfiguration(ctx, "empty-state-test"); err != nil || !changed {
		t.Fatalf("index configuration change = %v, %v", changed, err)
	}
	assertEmpty("configuration change")

	if err := storage.SyncRepositories(ctx, []catalog.Repository{repository}); err != nil {
		t.Fatal(err)
	}
	assertEmpty("rescan")
}

func TestIndexConfigurationChangeQueuesRepositoriesOnce(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "repokarta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	repository := catalog.Repository{
		Name:         "repo",
		Path:         filepath.Join(t.TempDir(), "repo"),
		HeadCommit:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ScanState:    "ready",
		DiscoveredAt: time.Now(),
		ScannedAt:    time.Now(),
	}
	ctx := context.Background()
	if err := storage.SyncRepositories(ctx, []catalog.Repository{repository}); err != nil {
		t.Fatal(err)
	}
	repositories, err := storage.ListRepositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.UpdateIndexState(ctx, repositories[0].ID, "ready", repository.HeadCommit, ""); err != nil {
		t.Fatal(err)
	}
	changed, err := storage.EnsureIndexConfiguration(ctx, "symbols=disabled")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("first configuration should be recorded as a change")
	}
	repositories, err = storage.ListRepositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if repositories[0].IndexState != "pending" {
		t.Fatalf("index state = %q, want pending", repositories[0].IndexState)
	}
	if err := storage.UpdateIndexState(ctx, repositories[0].ID, "ready", repository.HeadCommit, ""); err != nil {
		t.Fatal(err)
	}
	changed, err = storage.EnsureIndexConfiguration(ctx, "symbols=disabled")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("unchanged configuration should not queue another rebuild")
	}
	repositories, err = storage.ListRepositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if repositories[0].IndexState != "ready" {
		t.Fatalf("index state = %q, want ready", repositories[0].IndexState)
	}
}

func TestSyncRepositoriesCanonicalizesDuplicatesAndRemovesStaleRows(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "repokarta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()

	root := t.TempDir()
	currentPath := filepath.Join(root, "current")
	aliasPath := filepath.Join(root, "nested", "..", "current")
	stalePath := filepath.Join(root, "stale")
	now := time.Now()
	if err := storage.SyncRepositories(context.Background(), []catalog.Repository{
		{Name: "duplicate", Path: currentPath, ScanState: "ready", DiscoveredAt: now},
		{Name: "duplicate", Path: aliasPath, ScanState: "ready", DiscoveredAt: now},
		{Name: "stale", Path: stalePath, ScanState: "ready", DiscoveredAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	repositories, err := storage.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 2 {
		t.Fatalf("initial repositories = %#v, want one canonical duplicate and one stale row", repositories)
	}

	if err := storage.SyncRepositories(context.Background(), []catalog.Repository{
		{Name: "duplicate", Path: aliasPath, ScanState: "ready", DiscoveredAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	repositories, err = storage.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 1 || repositories[0].Path != canonicalRepositoryPath(currentPath) {
		t.Fatalf("reconciled repositories = %#v", repositories)
	}
}

func TestFreshDatabaseDoesNotCreateWikiTables(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "repokarta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	var count int
	if err := storage.db.QueryRow(`
SELECT COUNT(*)
FROM sqlite_master
WHERE type = 'table' AND name IN ('document_pages', 'document_citations')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("fresh database created %d Wiki tables; Wiki persistence must remain filesystem-only", count)
	}
}

func TestMigrationRemovesWikiTablesFromUpgradedDatabases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgraded.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
CREATE TABLE document_pages (repository_id INTEGER, slug TEXT, markdown TEXT);
CREATE TABLE document_citations (id INTEGER PRIMARY KEY, page_slug TEXT, url TEXT);
INSERT INTO document_pages VALUES (1, 'overview', '# Overview');
INSERT INTO document_citations VALUES (1, 'overview', 'http://127.0.0.1:7331/source/1');
PRAGMA user_version = 6;`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	storage, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()

	var count int
	if err := storage.db.QueryRow(`
SELECT COUNT(*)
FROM sqlite_master
WHERE type = 'table' AND name IN ('document_pages', 'document_citations')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("upgrade left %d Wiki tables in SQLite; Wiki persistence must remain filesystem-only", count)
	}
}

func TestMigrationAssignsLegacyConversationsToLocalAdministrator(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-conversations.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := legacy.Exec(schemaV4); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
INSERT INTO conversations (id, title, provider, created_at, updated_at)
VALUES (?, ?, ?, ?, ?);
PRAGMA user_version = 7;`,
		"legacy-chat",
		"Legacy chat",
		"codex",
		formatTime(now),
		formatTime(now),
	); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	storage, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	conversation, err := storage.GetConversation(context.Background(), "legacy-chat")
	if err != nil {
		t.Fatal(err)
	}
	if conversation.Author.ID != "local:admin" ||
		conversation.Author.Name != "Local administrator" ||
		conversation.Author.Provider != "local" {
		t.Fatalf("legacy author = %#v, want local administrator", conversation.Author)
	}
}

func TestMigrationIsIdempotentAcrossRepeatedUpgradeOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repeated-upgrade.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(schemaV1 + `
INSERT INTO repositories (id, name, path, discovered_at)
VALUES (42, 'legacy', 'C:/repositories/legacy', '2026-07-20T10:00:00Z');
PRAGMA user_version = 1;`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		storage, err := Open(path)
		if err != nil {
			t.Fatalf("open attempt %d: %v", attempt+1, err)
		}
		repositories, err := storage.ListRepositories(context.Background())
		if err != nil {
			storage.Close()
			t.Fatal(err)
		}
		if len(repositories) != 1 || repositories[0].ID != 42 || repositories[0].Name != "legacy" {
			storage.Close()
			t.Fatalf("repositories after attempt %d = %#v", attempt+1, repositories)
		}
		if err := storage.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenRejectsFutureSchemaWithoutMutatingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	future, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := future.Exec(`
CREATE TABLE future_marker (value TEXT NOT NULL);
INSERT INTO future_marker VALUES ('preserve-me');
PRAGMA user_version = 999;`); err != nil {
		t.Fatal(err)
	}
	if err := future.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("future schema error = %v", err)
	}
	verify, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer verify.Close()
	var marker string
	var version int
	if err := verify.QueryRow("SELECT value FROM future_marker").Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if err := verify.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if marker != "preserve-me" || version != 999 {
		t.Fatalf("future database was mutated: marker %q, version %d", marker, version)
	}
}

func TestRepositoryByIDReportsAMissingRepositoryClearly(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "missing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()

	_, err = storage.RepositoryByID(context.Background(), 4242)
	if err == nil || !strings.Contains(err.Error(), "repository 4242 is not indexed") {
		t.Fatalf("missing repository error = %v", err)
	}
	if strings.Contains(err.Error(), "sql:") {
		t.Fatalf("missing repository error leaks a driver error: %v", err)
	}
}

func TestAppSettingRoundTrip(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	ctx := context.Background()
	if value, ok, err := storage.AppSetting(ctx, "security"); err != nil || ok || value != "" {
		t.Fatalf("missing AppSetting() = %q, %v, %v", value, ok, err)
	}
	if err := storage.SetAppSetting(ctx, "security", `{"mode":"local"}`); err != nil {
		t.Fatal(err)
	}
	if value, ok, err := storage.AppSetting(ctx, "security"); err != nil || !ok || value != `{"mode":"local"}` {
		t.Fatalf("AppSetting() = %q, %v, %v", value, ok, err)
	}
	if err := storage.SetAppSetting(ctx, "security", `{"mode":"open"}`); err != nil {
		t.Fatal(err)
	}
	if value, ok, err := storage.AppSetting(ctx, "security"); err != nil || !ok || value != `{"mode":"open"}` {
		t.Fatalf("updated AppSetting() = %q, %v, %v", value, ok, err)
	}
}
