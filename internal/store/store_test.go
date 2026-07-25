package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/catalog"
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
		ID:        "conversation-1",
		Title:     "Trace authentication",
		Provider:  "anthropic-api",
		Model:     "claude-sonnet-5",
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
		CreatedAt:      now.Add(time.Second),
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
	if got.Title != conversation.Title || got.ResumeCursor != "opaque-provider-cursor" {
		t.Fatalf("conversation metadata = %#v", got)
	}
	if got.MessageCount != 2 || got.InputTokens != 120 || got.OutputTokens != 30 {
		t.Fatalf("conversation totals = %#v", got)
	}
	if len(got.Messages) != 2 || len(got.Messages[0].Images) != 1 || len(got.Messages[1].Sources) != 1 {
		t.Fatalf("conversation transcript = %#v", got.Messages)
	}
	if got.Messages[0].Images[0].Name != "diagram.png" || got.Messages[0].Images[0].Data != image.Data {
		t.Fatalf("persisted image = %#v", got.Messages[0].Images[0])
	}
	imagePath := filepath.Join(storage.conversationDirectory, "1-1.png")
	if _, err := os.Stat(imagePath); err != nil {
		t.Fatalf("persisted image file: %v", err)
	}
	if err := storage.DeleteConversation(ctx, conversation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(imagePath); !os.IsNotExist(err) {
		t.Fatalf("conversation image still exists after delete: %v", err)
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
