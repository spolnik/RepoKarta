package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/acquisition"
	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/codework"
	"github.com/spolnik/RepoKarta/internal/identity"
)

func TestPostgresRebindSkipsLiteralsAndComments(t *testing.T) {
	query := "SELECT ?, '?' AS literal, \"?\" AS identifier -- ?\n/* ? */ WHERE value = ?"
	got := rebind(query, BackendPostgres)
	want := "SELECT $1, '?' AS literal, \"?\" AS identifier -- ?\n/* ? */ WHERE value = $2"
	if got != want {
		t.Fatalf("rebound query = %q, want %q", got, want)
	}
}

func TestPostgresMigrationsAreSpecialized(t *testing.T) {
	for version := 1; version <= SchemaVersion; version++ {
		statement, err := migration(version, BackendPostgres)
		if err != nil {
			t.Fatal(err)
		}
		for _, incompatible := range []string{
			"COLLATE NOCASE",
			"INSERT OR IGNORE",
			"AUTOINCREMENT",
			"PRAGMA ",
		} {
			if strings.Contains(statement, incompatible) {
				t.Fatalf(
					"PostgreSQL migration %d retains incompatible %q",
					version,
					incompatible,
				)
			}
		}
	}
}

func TestSQLiteToPostgresMigrationTableListIsComplete(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "repokarta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	rows, err := storage.db.Query(`
SELECT name
FROM sqlite_master
WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	actual := make(map[string]struct{})
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		actual[table] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(actual) != len(migrationTables) {
		t.Fatalf("SQLite tables = %#v; migration tables = %#v", actual, migrationTables)
	}
	for _, table := range migrationTables {
		if _, ok := actual[table]; !ok {
			t.Fatalf("SQLite table %q is missing from the migration table list", table)
		}
	}
}

func TestPostgresBackendLifecycle(t *testing.T) {
	postgresURL := strings.TrimSpace(os.Getenv("REPOKARTA_TEST_POSTGRES_URL"))
	if postgresURL == "" {
		t.Skip("REPOKARTA_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	storage, err := OpenConfig(Config{
		Backend:       BackendPostgres,
		PostgresURL:   postgresURL,
		DataDirectory: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if storage.Backend() != BackendPostgres {
		t.Fatalf("backend = %q", storage.Backend())
	}
	if err := storage.SyncRepositories(ctx, []catalog.Repository{{
		Name:         "postgres-fixture",
		Path:         filepath.Join(t.TempDir(), "repository"),
		Bare:         true,
		ScanState:    "ready",
		DiscoveredAt: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	repositories, err := storage.ListRepositories(ctx)
	if err != nil || len(repositories) != 1 || !repositories[0].Bare {
		t.Fatalf("repositories = %#v, %v", repositories, err)
	}
	registered, err := storage.UpsertAcquisition(ctx, acquisition.Repository{
		Provider:        "github",
		CanonicalID:     "acme/backend",
		Name:            "backend",
		CheckoutPath:    filepath.Join(t.TempDir(), "checkout"),
		InclusionPolicy: "approved",
		Archived:        true,
		Owned:           true,
		State:           "ready",
	})
	if err != nil || registered.ID == 0 || !registered.Archived || !registered.Owned {
		t.Fatalf("acquisition = %#v, %v", registered, err)
	}
	if _, err := storage.SaveUser(ctx, identity.User{
		ID:       "case-user-1",
		UserName: "CaseSensitive",
		Role:     identity.RoleReader,
		Active:   true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.SaveUser(ctx, identity.User{
		ID:       "case-user-2",
		UserName: "casesensitive",
		Role:     identity.RoleReader,
		Active:   true,
	}); err == nil {
		t.Fatal("expected case-insensitive PostgreSQL username uniqueness")
	}
	if err := storage.SetRoleMapping(ctx, identity.RoleMapping{
		Provider:   "saml",
		GroupValue: "Engineering",
		Role:       identity.RoleReader,
	}); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetRoleMapping(ctx, identity.RoleMapping{
		Provider:   "saml",
		GroupValue: "engineering",
		Role:       identity.RoleAdmin,
	}); err != nil {
		t.Fatal(err)
	}
	mappings, err := storage.ListRoleMappings(ctx)
	if err != nil || len(mappings) != 1 ||
		mappings[0].Role != identity.RoleAdmin {
		t.Fatalf("role mappings = %#v, %v", mappings, err)
	}
	conversation := agent.Conversation{
		ID:       "postgres-conversation",
		Title:    "PostgreSQL",
		Provider: "codex",
		Code:     true,
		Author:   agent.ConversationAuthor{ID: "local:admin"},
	}
	if err := storage.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	message, err := storage.AppendMessage(ctx, agent.Message{
		ConversationID: conversation.ID,
		Role:           agent.RoleUser,
		Text:           "persist me",
	})
	if err != nil || message.ID == 0 {
		t.Fatalf("message = %#v, %v", message, err)
	}
	loaded, err := storage.GetConversation(ctx, conversation.ID)
	if err != nil || !loaded.Code || len(loaded.Messages) != 1 || loaded.Messages[0].Text != "persist me" {
		t.Fatalf("conversation = %#v, %v", loaded, err)
	}
	if err := storage.SetRepositoryAccess(ctx, RepositoryAccess{
		RepositoryID: repositories[0].ID, OwnerID: "local:admin",
		Visibility: "private", CodeEnabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if enabled, err := storage.RepositoryCodingEnabled(ctx, repositories[0].ID); err != nil || !enabled {
		t.Fatalf("PostgreSQL Code policy = %v, error = %v", enabled, err)
	}
	codeSession := codework.Session{
		ID: "code-fedcba9876543210", RepositoryID: repositories[0].ID,
		Repository: repositories[0].Name, AuthorID: "local:admin", Provider: "codex",
		Baseline: strings.Repeat("a", 40), Branch: "repokarta/code/fedcba9876543210",
		State: codework.StateReady, Version: 1, CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := storage.CreateCodeSession(ctx, codeSession); err != nil {
		t.Fatal(err)
	}
	if err := storage.CreateCodeApproval(ctx, codework.Approval{
		ID: "postgres-approval", SessionID: codeSession.ID, Kind: "command",
		Directory: "internal/store", Status: "pending", RequestedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	approvals, err := storage.CodeApprovals(ctx, codeSession.ID)
	if err != nil || len(approvals) != 1 || approvals[0].Directory != "internal/store" {
		t.Fatalf("PostgreSQL Code approvals = %#v, error = %v", approvals, err)
	}
	var version int
	if err := storage.db.QueryRow(
		"SELECT COALESCE(MAX(version), 0) FROM repokarta_schema_migrations",
	).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("schema version = %d, %v", version, err)
	}
}

func TestSQLiteToPostgresMigration(t *testing.T) {
	postgresURL := strings.TrimSpace(os.Getenv("REPOKARTA_TEST_MIGRATION_POSTGRES_URL"))
	if postgresURL == "" {
		t.Skip("REPOKARTA_TEST_MIGRATION_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	dataDirectory := t.TempDir()
	sqlitePath := filepath.Join(dataDirectory, "repokarta.db")
	source, err := Open(sqlitePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.SyncRepositories(ctx, []catalog.Repository{{
		Name:         "migration-fixture",
		Path:         filepath.Join(t.TempDir(), "repository"),
		ScanState:    "ready",
		DiscoveredAt: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := source.CreateConversation(ctx, agent.Conversation{
		ID:       "migrated-conversation",
		Title:    "Migrated",
		Provider: "codex",
		Author:   agent.ConversationAuthor{ID: "local:admin"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AppendMessage(ctx, agent.Message{
		ConversationID: "migrated-conversation",
		Role:           agent.RoleAssistant,
		Text:           "still here",
	}); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	report, err := MigrateSQLiteToPostgres(
		ctx,
		sqlitePath,
		postgresURL,
		dataDirectory,
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.Tables != len(migrationTables) || report.Rows == 0 {
		t.Fatalf("migration report = %#v", report)
	}
	if _, err := MigrateSQLiteToPostgres(
		ctx,
		sqlitePath,
		postgresURL,
		dataDirectory,
	); err == nil || !strings.Contains(err.Error(), "destination is not empty") {
		t.Fatalf("second migration error = %v", err)
	}
	destination, err := OpenConfig(Config{
		Backend:       BackendPostgres,
		PostgresURL:   postgresURL,
		DataDirectory: dataDirectory,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	repositories, err := destination.ListRepositories(ctx)
	if err != nil || len(repositories) != 1 || repositories[0].Name != "migration-fixture" {
		t.Fatalf("repositories = %#v, %v", repositories, err)
	}
	conversation, err := destination.GetConversation(ctx, "migrated-conversation")
	if err != nil || len(conversation.Messages) != 1 ||
		conversation.Messages[0].Text != "still here" {
		t.Fatalf("conversation = %#v, %v", conversation, err)
	}
}
