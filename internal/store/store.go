package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/access"
	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/audit"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/contextscope"
	_ "modernc.org/sqlite"
)

const (
	currentSchemaVersion = 19

	schemaV1 = `
CREATE TABLE IF NOT EXISTS repositories (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    path TEXT NOT NULL UNIQUE,
    discovered_at TEXT NOT NULL
);`

	schemaV2 = `
ALTER TABLE repositories ADD COLUMN origin_url TEXT NOT NULL DEFAULT '';
ALTER TABLE repositories ADD COLUMN default_revision TEXT NOT NULL DEFAULT '';
ALTER TABLE repositories ADD COLUMN head_commit TEXT NOT NULL DEFAULT '';
ALTER TABLE repositories ADD COLUMN bare INTEGER NOT NULL DEFAULT 0;
ALTER TABLE repositories ADD COLUMN scan_state TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE repositories ADD COLUMN scan_error TEXT NOT NULL DEFAULT '';
ALTER TABLE repositories ADD COLUMN scanned_at TEXT NOT NULL DEFAULT '';
ALTER TABLE repositories ADD COLUMN index_state TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE repositories ADD COLUMN index_error TEXT NOT NULL DEFAULT '';
ALTER TABLE repositories ADD COLUMN indexed_commit TEXT NOT NULL DEFAULT '';
ALTER TABLE repositories ADD COLUMN indexed_at TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS repositories_name_index ON repositories(name COLLATE NOCASE);
CREATE INDEX IF NOT EXISTS repositories_index_state_index ON repositories(index_state);`

	schemaV3 = `
CREATE TABLE IF NOT EXISTS app_settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);`

	schemaV4 = `
CREATE TABLE IF NOT EXISTS conversations (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    effort TEXT NOT NULL DEFAULT '',
    resume_cursor TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS conversations_updated_at_index
ON conversations(updated_at DESC);

CREATE TABLE IF NOT EXISTS conversation_messages (
    id INTEGER PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK(role IN ('user', 'assistant')),
    text TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'complete',
    error TEXT NOT NULL DEFAULT '',
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS conversation_messages_conversation_index
ON conversation_messages(conversation_id, id);

CREATE TABLE IF NOT EXISTS conversation_message_images (
    id INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES conversation_messages(id) ON DELETE CASCADE,
    name TEXT NOT NULL DEFAULT '',
    media_type TEXT NOT NULL,
    storage_path TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS conversation_message_citations (
    id INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES conversation_messages(id) ON DELETE CASCADE,
    label TEXT NOT NULL,
    url TEXT NOT NULL
);`

	// Wiki pages and their manifest are intentionally file-backed. Schema
	// versions 5 and 6 are retained so existing pre-release databases remain
	// readable, but fresh databases never create Wiki tables.
	schemaV5 = `SELECT 1;`
	schemaV6 = `SELECT 1;`

	// Version 7 removes the Wiki tables an upgraded pre-release database may
	// still carry. Wiki content and page metadata live only on the filesystem,
	// so leaving them behind would keep a second, stale copy in SQLite.
	schemaV7 = `
DROP TABLE IF EXISTS document_citations;
DROP TABLE IF EXISTS document_pages;`

	// Version 8 assigns every durable conversation to a stable authenticated
	// author. Existing local-first chats belong to the local administrator.
	schemaV8 = `
CREATE TABLE IF NOT EXISTS conversations (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    effort TEXT NOT NULL DEFAULT '',
    resume_cursor TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0
);
ALTER TABLE conversations ADD COLUMN author_id TEXT NOT NULL DEFAULT 'local:admin';
ALTER TABLE conversations ADD COLUMN author_name TEXT NOT NULL DEFAULT 'Local administrator';
ALTER TABLE conversations ADD COLUMN author_email TEXT NOT NULL DEFAULT '';
ALTER TABLE conversations ADD COLUMN author_provider TEXT NOT NULL DEFAULT 'local';
CREATE INDEX IF NOT EXISTS conversations_author_updated_index
ON conversations(author_id, updated_at DESC);`

	// Version 9 gives every repository an explicit, deny-by-default access
	// policy. Existing local-first repositories stay owned by local:admin.
	// Derived artifacts inherit the repository policy instead of maintaining a
	// second authorization system that can drift.
	schemaV9 = `
CREATE TABLE IF NOT EXISTS repositories (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    path TEXT NOT NULL UNIQUE,
    discovered_at TEXT NOT NULL,
    origin_url TEXT NOT NULL DEFAULT '',
    default_revision TEXT NOT NULL DEFAULT '',
    head_commit TEXT NOT NULL DEFAULT '',
    bare INTEGER NOT NULL DEFAULT 0,
    scan_state TEXT NOT NULL DEFAULT 'pending',
    scan_error TEXT NOT NULL DEFAULT '',
    scanned_at TEXT NOT NULL DEFAULT '',
    index_state TEXT NOT NULL DEFAULT 'pending',
    index_error TEXT NOT NULL DEFAULT '',
    indexed_commit TEXT NOT NULL DEFAULT '',
    indexed_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS repository_access (
    repository_id INTEGER PRIMARY KEY REFERENCES repositories(id) ON DELETE CASCADE,
    owner_id TEXT NOT NULL DEFAULT 'local:admin',
    visibility TEXT NOT NULL DEFAULT 'private'
        CHECK(visibility IN ('private', 'shared')),
    updated_at TEXT NOT NULL DEFAULT ''
);
INSERT OR IGNORE INTO repository_access (repository_id, owner_id, visibility, updated_at)
SELECT id, 'local:admin', 'private', '' FROM repositories;
CREATE TABLE IF NOT EXISTS repository_access_grants (
    repository_id INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    subject_type TEXT NOT NULL CHECK(subject_type IN ('user', 'group')),
    subject_id TEXT NOT NULL,
    PRIMARY KEY(repository_id, subject_type, subject_id)
);
CREATE INDEX IF NOT EXISTS repository_access_grants_subject_index
ON repository_access_grants(subject_type, subject_id, repository_id);
CREATE TRIGGER IF NOT EXISTS repositories_access_insert
AFTER INSERT ON repositories
BEGIN
    INSERT OR IGNORE INTO repository_access (
        repository_id, owner_id, visibility, updated_at
    ) VALUES (NEW.id, 'local:admin', 'private', '');
END;`

	// Version 10 retains the identity-provider groups captured when a
	// conversation starts. Internal provider MCP calls can therefore enforce
	// the same team grants as the initiating browser request.
	schemaV10 = `
ALTER TABLE conversations ADD COLUMN author_groups TEXT NOT NULL DEFAULT '[]';`

	// Version 11 adds immutable, redacted security evidence. Application code
	// only appends events; the sole deletion path is the explicit retention
	// operation, which records its own administration event afterward.
	schemaV11 = `
CREATE TABLE IF NOT EXISTS audit_events (
    id INTEGER PRIMARY KEY,
    actor_id TEXT NOT NULL,
    actor_name TEXT NOT NULL DEFAULT '',
    action TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL DEFAULT '',
    outcome TEXT NOT NULL,
    authentication_provider TEXT NOT NULL,
    correlation_id TEXT NOT NULL,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS audit_events_created_index
ON audit_events(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS audit_events_actor_index
ON audit_events(actor_id COLLATE NOCASE, id DESC);
CREATE INDEX IF NOT EXISTS audit_events_action_index
ON audit_events(action COLLATE NOCASE, id DESC);`

	// Version 12 adds SCIM-managed identities and groups plus immediately
	// evaluated direct and identity-provider-group role assignments.
	schemaV12 = `
CREATE TABLE IF NOT EXISTS identities (
    id TEXT PRIMARY KEY,
    external_id TEXT NOT NULL DEFAULT '',
    user_name TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    email TEXT NOT NULL DEFAULT '',
    auth_provider TEXT NOT NULL DEFAULT '',
    auth_subject TEXT NOT NULL DEFAULT '',
    active INTEGER NOT NULL DEFAULT 1,
    role TEXT NOT NULL DEFAULT 'reader'
        CHECK(role IN ('reader', 'knowledge-maintainer', 'administrator')),
    scim_managed INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS identities_external_id_unique
ON identities(external_id) WHERE external_id <> '';
CREATE UNIQUE INDEX IF NOT EXISTS identities_auth_unique
ON identities(auth_provider, auth_subject)
WHERE auth_provider <> '' AND auth_subject <> '';
CREATE UNIQUE INDEX IF NOT EXISTS identities_username_unique
ON identities(user_name COLLATE NOCASE);
CREATE INDEX IF NOT EXISTS identities_email_index
ON identities(email COLLATE NOCASE);

CREATE TABLE IF NOT EXISTS identity_groups (
    id TEXT PRIMARY KEY,
    external_id TEXT NOT NULL DEFAULT '',
    display_name TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'reader'
        CHECK(role IN ('reader', 'knowledge-maintainer', 'administrator')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS identity_groups_external_id_unique
ON identity_groups(external_id) WHERE external_id <> '';
CREATE UNIQUE INDEX IF NOT EXISTS identity_groups_name_unique
ON identity_groups(display_name COLLATE NOCASE);
CREATE TABLE IF NOT EXISTS identity_group_members (
    group_id TEXT NOT NULL REFERENCES identity_groups(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES identities(id) ON DELETE CASCADE,
    PRIMARY KEY(group_id, user_id)
);
CREATE INDEX IF NOT EXISTS identity_group_members_user_index
ON identity_group_members(user_id, group_id);

CREATE TABLE IF NOT EXISTS identity_role_mappings (
    id INTEGER PRIMARY KEY,
    provider TEXT NOT NULL,
    group_value TEXT NOT NULL,
    role TEXT NOT NULL
        CHECK(role IN ('reader', 'knowledge-maintainer', 'administrator')),
    updated_at TEXT NOT NULL,
    UNIQUE(provider, group_value COLLATE NOCASE)
);`

	// Version 13 keeps server-resolved structured contexts on
	// the user message that introduced them. Saved chats can render exact chips
	// without reconstructing identities from message text. The defensive CREATE
	// supports early pre-release databases whose recorded version omitted the
	// conversation tables.
	schemaV13 = `
CREATE TABLE IF NOT EXISTS conversation_messages (
    id INTEGER PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK(role IN ('user', 'assistant')),
    text TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'complete',
    error TEXT NOT NULL DEFAULT '',
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);
ALTER TABLE conversation_messages ADD COLUMN contexts_json TEXT NOT NULL DEFAULT '[]';`

	// Version 14 stores normalized, immutable code-insight runs and
	// observations. Reports inherit repository access and remain separate from
	// source. Sonar credentials are referenced by environment-variable name;
	// credential values never enter SQLite.
	schemaV14 = `
CREATE TABLE IF NOT EXISTS insight_runs (
    id TEXT PRIMARY KEY,
    repository_id INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    revision TEXT NOT NULL,
    branch TEXT NOT NULL DEFAULT '',
    tool TEXT NOT NULL,
    tool_version TEXT NOT NULL DEFAULT '',
    source_kind TEXT NOT NULL,
    source_ref TEXT NOT NULL DEFAULT '',
    rule_pack TEXT NOT NULL DEFAULT '',
    configuration TEXT NOT NULL DEFAULT '',
    license TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    status_message TEXT NOT NULL DEFAULT '',
    confidence TEXT NOT NULL,
    observed_at TEXT NOT NULL,
    ingested_at TEXT NOT NULL,
    metadata_json TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS insight_runs_repository_time_index
ON insight_runs(repository_id, observed_at DESC);
CREATE INDEX IF NOT EXISTS insight_runs_tool_time_index
ON insight_runs(tool COLLATE NOCASE, observed_at DESC);

CREATE TABLE IF NOT EXISTS insight_observations (
    id INTEGER PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES insight_runs(id) ON DELETE CASCADE,
    repository_id INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    revision TEXT NOT NULL,
    branch TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL CHECK(kind IN ('metric', 'finding')),
    key TEXT NOT NULL,
    value REAL,
    unit TEXT NOT NULL DEFAULT '',
    severity TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL DEFAULT '',
    path TEXT NOT NULL DEFAULT '',
    start_line INTEGER NOT NULL DEFAULT 0,
    end_line INTEGER NOT NULL DEFAULT 0,
    language TEXT NOT NULL DEFAULT '',
    owner TEXT NOT NULL DEFAULT '',
    fingerprint TEXT NOT NULL DEFAULT '',
    suppressed INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL,
    confidence TEXT NOT NULL,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    code_flow_json TEXT NOT NULL DEFAULT 'null',
    source_url TEXT NOT NULL DEFAULT '',
    observed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS insight_observations_repository_revision_index
ON insight_observations(repository_id, revision, observed_at DESC);
CREATE INDEX IF NOT EXISTS insight_observations_key_index
ON insight_observations(key COLLATE NOCASE, kind, observed_at DESC);
CREATE INDEX IF NOT EXISTS insight_observations_finding_index
ON insight_observations(fingerprint, severity, observed_at DESC);

CREATE TABLE IF NOT EXISTS insight_thresholds (
    id INTEGER PRIMARY KEY,
    repository_id INTEGER NOT NULL DEFAULT 0,
    key TEXT NOT NULL,
    operator TEXT NOT NULL CHECK(operator IN ('lt', 'lte', 'gt', 'gte')),
    value REAL NOT NULL,
    severity TEXT NOT NULL DEFAULT 'warning',
    enabled INTEGER NOT NULL DEFAULT 1,
    updated_at TEXT NOT NULL,
    UNIQUE(repository_id, key)
);

CREATE TABLE IF NOT EXISTS sonar_connections (
    id INTEGER PRIMARY KEY,
    repository_id INTEGER NOT NULL UNIQUE REFERENCES repositories(id) ON DELETE CASCADE,
    base_url TEXT NOT NULL,
    project_key TEXT NOT NULL,
    token_env TEXT NOT NULL,
    poll_interval_minutes INTEGER NOT NULL DEFAULT 15,
    retention_runs INTEGER NOT NULL DEFAULT 50,
    enabled INTEGER NOT NULL DEFAULT 1,
    state TEXT NOT NULL DEFAULT 'stale',
    status_message TEXT NOT NULL DEFAULT '',
    last_polled_at TEXT NOT NULL DEFAULT '',
    next_poll_at TEXT NOT NULL DEFAULT '',
    failure_count INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL
);`

	// Version 15 records administrator-approved local and hosted repository
	// sources separately from the derived search catalogue. Secret values are
	// deliberately excluded; credential_ref stores only an environment variable
	// or external credential-helper reference.
	schemaV15 = `
CREATE TABLE IF NOT EXISTS repository_acquisitions (
    id INTEGER PRIMARY KEY,
    provider TEXT NOT NULL CHECK(provider IN ('local', 'github', 'gitlab')),
    provider_repository_id TEXT NOT NULL DEFAULT '',
    canonical_id TEXT NOT NULL UNIQUE COLLATE NOCASE,
    name TEXT NOT NULL,
    namespace TEXT NOT NULL DEFAULT '',
    remote_url TEXT NOT NULL DEFAULT '',
    web_url TEXT NOT NULL DEFAULT '',
    checkout_path TEXT NOT NULL UNIQUE,
    default_branch TEXT NOT NULL DEFAULT '',
    credential_ref TEXT NOT NULL DEFAULT '',
    inclusion_policy TEXT NOT NULL DEFAULT 'approved',
    visibility TEXT NOT NULL DEFAULT '',
    archived INTEGER NOT NULL DEFAULT 0,
    forked INTEGER NOT NULL DEFAULT 0,
    owned INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL CHECK(state IN ('acquiring', 'ready', 'syncing', 'error')),
    last_error TEXT NOT NULL DEFAULT '',
    head_commit TEXT NOT NULL DEFAULT '',
    failure_count INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    discovered_at TEXT NOT NULL,
    synced_at TEXT NOT NULL DEFAULT '',
    next_sync_at TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL
);
ALTER TABLE repositories ADD COLUMN acquisition_id INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS repository_acquisitions_state_index
ON repository_acquisitions(state, next_sync_at);
CREATE UNIQUE INDEX IF NOT EXISTS repository_acquisitions_provider_id_index
ON repository_acquisitions(provider, provider_repository_id)
WHERE provider_repository_id <> '';
CREATE TABLE IF NOT EXISTS repository_acquisition_events (
    id INTEGER PRIMARY KEY,
    repository_id INTEGER NOT NULL DEFAULT 0,
    canonical_id TEXT NOT NULL DEFAULT '',
    action TEXT NOT NULL,
    outcome TEXT NOT NULL,
    revision TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS repository_acquisition_events_created_index
ON repository_acquisition_events(created_at DESC);`

	// Version 16 caches public dependency-registry observations independently
	// from commit-pinned declarations. Conditional-request validators and expiry
	// timestamps keep refreshes cheap without treating cached data as source.
	schemaV16 = `
CREATE TABLE IF NOT EXISTS dependency_registry_observations (
    ecosystem TEXT NOT NULL,
    registry TEXT NOT NULL,
    package TEXT NOT NULL,
    latest_stable TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK(status IN ('ok', 'error')),
    error TEXT NOT NULL DEFAULT '',
    etag TEXT NOT NULL DEFAULT '',
    last_modified TEXT NOT NULL DEFAULT '',
    observed_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    PRIMARY KEY(ecosystem, registry, package)
);
CREATE INDEX IF NOT EXISTS dependency_registry_observations_expiry_index
ON dependency_registry_observations(expires_at);`

	// Version 17 persists revision-pinned named search contexts. Personal
	// definitions remain private to their owner; shared and administrator
	// defaults are immutable to non-administrators.
	schemaV17 = `
CREATE TABLE IF NOT EXISTS named_contexts (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    category TEXT NOT NULL
        CHECK(category IN ('team', 'product', 'service_fleet', 'release', 'personal_task')),
    visibility TEXT NOT NULL
        CHECK(visibility IN ('personal', 'shared')),
    default_scope TEXT NOT NULL DEFAULT 'none'
        CHECK(default_scope IN ('none', 'personal', 'administrator')),
    owner_id TEXT NOT NULL,
    managed INTEGER NOT NULL DEFAULT 0,
    selectors_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS named_contexts_owner_title_index
ON named_contexts(owner_id, title COLLATE NOCASE);
CREATE INDEX IF NOT EXISTS named_contexts_visibility_default_index
ON named_contexts(visibility, default_scope, updated_at DESC);`

	// Version 18 stores bounded runtime service-graph observations separately
	// from commit-pinned static topology. Provider and time-window columns keep
	// observed traffic honest and allow static/observed drift to be explained.
	schemaV18 = `
CREATE TABLE IF NOT EXISTS runtime_topology_observations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    provider TEXT NOT NULL,
    environment TEXT NOT NULL DEFAULT '',
    source_name TEXT NOT NULL,
    source_kind TEXT NOT NULL,
    target_name TEXT NOT NULL,
    target_kind TEXT NOT NULL,
    protocol TEXT NOT NULL,
    interaction TEXT NOT NULL,
    transport TEXT NOT NULL DEFAULT '',
    observed_from TEXT NOT NULL,
    observed_to TEXT NOT NULL,
    request_count INTEGER NOT NULL DEFAULT 0 CHECK(request_count >= 0),
    error_count INTEGER NOT NULL DEFAULT 0 CHECK(error_count >= 0),
    latency_p95_ms REAL NOT NULL DEFAULT 0 CHECK(latency_p95_ms >= 0),
    imported_at TEXT NOT NULL,
    UNIQUE(
        provider, environment, source_name, target_name, protocol,
        interaction, transport, observed_from, observed_to
    )
);
CREATE INDEX IF NOT EXISTS runtime_topology_observations_window_index
ON runtime_topology_observations(observed_to DESC, observed_from DESC);
CREATE INDEX IF NOT EXISTS runtime_topology_observations_peer_index
ON runtime_topology_observations(source_name, target_name, protocol);`

	// Java SCIP generation is an optional derived build. Its state is separate
	// from repositories.index_state so build-tool failures retain normal source
	// search and syntax-backed navigation.
	schemaV19 = `
CREATE TABLE IF NOT EXISTS repository_scip_indexes (
    repository_id INTEGER PRIMARY KEY REFERENCES repositories(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN (
        'pending', 'indexing', 'ready', 'failed', 'unavailable', 'skipped'
    )),
    applicable INTEGER NOT NULL DEFAULT 0,
    revision TEXT NOT NULL DEFAULT '',
    configuration TEXT NOT NULL DEFAULT '',
    indexer TEXT NOT NULL DEFAULT '',
    version TEXT NOT NULL DEFAULT '',
    build_root TEXT NOT NULL DEFAULT '',
    documents INTEGER NOT NULL DEFAULT 0 CHECK(documents >= 0),
    symbols INTEGER NOT NULL DEFAULT 0 CHECK(symbols >= 0),
    occurrences INTEGER NOT NULL DEFAULT 0 CHECK(occurrences >= 0),
    error TEXT NOT NULL DEFAULT '',
    queued_at TEXT NOT NULL DEFAULT '',
    started_at TEXT NOT NULL DEFAULT '',
    finished_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS repository_scip_indexes_state_index
ON repository_scip_indexes(state, revision);`
)

// SchemaVersion is the current durable SQLite format. Diagnostics and upgrade
// tests use this value without reaching into migration internals.
const SchemaVersion = currentSchemaVersion

// Store persists RepoKarta-owned metadata. Repository source remains read-only.
type Store struct {
	db                    *sql.DB
	conversationDirectory string
}

// Open opens the SQLite database, enables WAL, and applies migrations.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec("PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure SQLite database: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	conversationDirectory := filepath.Join(filepath.Dir(path), "conversations")
	if err := os.MkdirAll(conversationDirectory, 0o700); err != nil {
		db.Close()
		return nil, fmt.Errorf("create conversation storage: %w", err)
	}

	return &Store{db: db, conversationDirectory: conversationDirectory}, nil
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > currentSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, currentSchemaVersion)
	}

	for version < currentSchemaVersion {
		next := version + 1
		var migration string
		switch next {
		case 1:
			migration = schemaV1
		case 2:
			migration = schemaV2
		case 3:
			migration = schemaV3
		case 4:
			migration = schemaV4
		case 5:
			migration = schemaV5
		case 6:
			migration = schemaV6
		case 7:
			migration = schemaV7
		case 8:
			migration = schemaV8
		case 9:
			migration = schemaV9
		case 10:
			migration = schemaV10
		case 11:
			migration = schemaV11
		case 12:
			migration = schemaV12
		case 13:
			migration = schemaV13
		case 14:
			migration = schemaV14
		case 15:
			migration = schemaV15
		case 16:
			migration = schemaV16
		case 17:
			migration = schemaV17
		case 18:
			migration = schemaV18
		case 19:
			migration = schemaV19
		default:
			return fmt.Errorf("missing migration for schema version %d", next)
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin schema migration %d: %w", next, err)
		}
		if _, err := tx.Exec(migration); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply schema migration %d: %w", next, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", next)); err != nil {
			tx.Rollback()
			return fmt.Errorf("record schema migration %d: %w", next, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit schema migration %d: %w", next, err)
		}
		version = next
	}
	return nil
}

// EnsureIndexConfiguration queues every repository for reindexing when the
// indexer's capability signature changes, such as ctags becoming available.
func (s *Store) EnsureIndexConfiguration(ctx context.Context, signature string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var current string
	err = tx.QueryRowContext(ctx, "SELECT value FROM app_settings WHERE key = 'index_configuration'").Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil && current == signature {
		return false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE repositories
SET index_state = CASE
        WHEN scan_state = 'empty' THEN 'empty'
        WHEN scan_state = 'error' THEN 'error'
        ELSE 'pending'
    END,
    index_error = CASE
        WHEN scan_state IN ('empty', 'error') THEN scan_error
        ELSE ''
    END,
    indexed_at = ''`); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO app_settings (key, value) VALUES ('index_configuration', ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, signature); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// AppSetting returns one non-secret application setting.
func (s *Store) AppSetting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM app_settings WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read application setting %q: %w", key, err)
	}
	return value, true, nil
}

// SetAppSetting persists one non-secret application setting.
func (s *Store) SetAppSetting(ctx context.Context, key, value string) error {
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO app_settings (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
		return fmt.Errorf("write application setting %q: %w", key, err)
	}
	return nil
}

// Close closes the metadata database.
func (s *Store) Close() error {
	return s.db.Close()
}

// ConversationImagePaths returns the exact RepoKarta-owned image filenames
// referenced by durable conversation metadata. Callers use this to distinguish
// live attachments from orphaned files without reading image contents.
func (s *Store) ConversationImagePaths(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT storage_path
FROM conversation_message_images
ORDER BY storage_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	paths := make(map[string]struct{})
	for rows.Next() {
		var storagePath string
		if err := rows.Scan(&storagePath); err != nil {
			return nil, err
		}
		if filepath.Base(storagePath) == storagePath && storagePath != "." && storagePath != "" {
			paths[storagePath] = struct{}{}
		}
	}
	return paths, rows.Err()
}

// SyncRepositories updates discovered repositories without discarding existing
// indexing state. Repositories no longer below the configured root are removed.
func (s *Store) SyncRepositories(ctx context.Context, repositories []catalog.Repository) error {
	repositories = reconcileRepositoryPaths(repositories)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	previous := make(map[string]string)
	rows, err := tx.QueryContext(ctx, `SELECT path, CAST(id AS TEXT) FROM repositories`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var path, id string
		if err := rows.Scan(&path, &id); err != nil {
			rows.Close()
			return err
		}
		previous[repositoryPathKey(path)] = id
	}
	if err := rows.Close(); err != nil {
		return err
	}

	seen := make([]string, 0, len(repositories))
	current := make(map[string]catalog.Repository, len(repositories))
	for _, repository := range repositories {
		discoveredAt := formatTime(repository.DiscoveredAt)
		scannedAt := formatTime(repository.ScannedAt)
		indexState := repository.IndexState
		indexError := repository.IndexError
		if indexState == "" {
			switch repository.ScanState {
			case "empty":
				indexState = "empty"
				if indexError == "" {
					indexError = repository.ScanError
				}
				if indexError == "" {
					indexError = catalog.EmptyRepositoryReason
				}
			case "error":
				indexState = "error"
				if indexError == "" {
					indexError = repository.ScanError
				}
			default:
				indexState = "pending"
			}
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO repositories (
    name, path, origin_url, default_revision, head_commit, bare,
    scan_state, scan_error, index_state, index_error, discovered_at, scanned_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(path) DO UPDATE SET
    name = excluded.name,
    origin_url = excluded.origin_url,
    default_revision = excluded.default_revision,
    head_commit = excluded.head_commit,
    bare = excluded.bare,
    scan_state = excluded.scan_state,
    scan_error = excluded.scan_error,
    scanned_at = excluded.scanned_at,
    index_state = CASE
        WHEN excluded.index_state IN ('empty', 'error')
        THEN excluded.index_state
        WHEN repositories.indexed_commit = excluded.head_commit AND repositories.index_state = 'ready'
        THEN repositories.index_state
        ELSE 'pending'
    END,
    index_error = CASE
        WHEN excluded.index_state IN ('empty', 'error')
        THEN excluded.index_error
        WHEN repositories.indexed_commit = excluded.head_commit AND repositories.index_state = 'ready'
        THEN repositories.index_error
        ELSE ''
    END`,
			repository.Name,
			repository.Path,
			repository.OriginURL,
			repository.DefaultRevision,
			repository.HeadCommit,
			repository.Bare,
			repository.ScanState,
			repository.ScanError,
			indexState,
			indexError,
			discoveredAt,
			scannedAt,
		); err != nil {
			return fmt.Errorf("sync repository %q: %w", repository.Path, err)
		}
		seen = append(seen, repository.Path)
		current[repositoryPathKey(repository.Path)] = repository
	}

	if len(seen) == 0 {
		if _, err := tx.ExecContext(ctx, "DELETE FROM repositories"); err != nil {
			return err
		}
	} else {
		placeholders := "?"
		arguments := make([]any, len(seen))
		for index, path := range seen {
			arguments[index] = path
			if index > 0 {
				placeholders += ",?"
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM repositories WHERE path NOT IN ("+placeholders+")", arguments...); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE repositories
SET acquisition_id = COALESCE((
    SELECT a.id
    FROM repository_acquisitions a
    WHERE a.checkout_path = repositories.path
), 0)`); err != nil {
		return fmt.Errorf("link repository acquisition provenance: %w", err)
	}

	for key, repository := range current {
		if _, existed := previous[key]; existed {
			continue
		}
		var repositoryID string
		if err := tx.QueryRowContext(ctx, `SELECT CAST(id AS TEXT) FROM repositories WHERE path = ?`, repository.Path).Scan(&repositoryID); err != nil {
			return err
		}
		if err := appendAuditEvent(ctx, tx, audit.Event{
			ActorID: "system:catalogue", ActorName: "Repository catalogue",
			Action: "repository.acquire", TargetType: "repository", TargetID: repositoryID,
			Outcome: "success", Provider: "system",
			Metadata: map[string]string{"name": repository.Name, "path": repository.Path},
		}); err != nil {
			return err
		}
	}
	for key, repositoryID := range previous {
		if _, retained := current[key]; retained {
			continue
		}
		if err := appendAuditEvent(ctx, tx, audit.Event{
			ActorID: "system:catalogue", ActorName: "Repository catalogue",
			Action: "repository.remove", TargetType: "repository", TargetID: repositoryID,
			Outcome: "success", Provider: "system",
			Metadata: map[string]string{"path_key": key},
		}); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func reconcileRepositoryPaths(repositories []catalog.Repository) []catalog.Repository {
	reconciled := make([]catalog.Repository, 0, len(repositories))
	seen := make(map[string]struct{}, len(repositories))
	for _, repository := range repositories {
		repository.Path = canonicalRepositoryPath(repository.Path)
		key := repositoryPathKey(repository.Path)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		reconciled = append(reconciled, repository)
	}
	return reconciled
}

func canonicalRepositoryPath(path string) string {
	path = filepath.Clean(path)
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = filepath.Clean(resolved)
	}
	return path
}

func repositoryPathKey(path string) string {
	path = filepath.ToSlash(filepath.Clean(path))
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

// ReplaceRepositories is retained for compatibility with the M0 API.
func (s *Store) ReplaceRepositories(ctx context.Context, repositories []catalog.Repository) error {
	return s.SyncRepositories(ctx, repositories)
}

// ListRepositories returns the repository catalogue in display order.
func (s *Store) ListRepositories(ctx context.Context) ([]catalog.Repository, error) {
	query := `
SELECT
    r.id, r.name, r.path, r.origin_url, r.default_revision, r.head_commit, r.bare,
    r.scan_state, r.scan_error, r.index_state, r.index_error, r.indexed_commit,
    r.discovered_at, r.scanned_at, r.indexed_at, r.acquisition_id,
    si.provider, si.state, si.applicable, si.revision, si.configuration,
    si.indexer, si.version, si.build_root, si.documents, si.symbols,
    si.occurrences, si.error, si.queued_at, si.started_at, si.finished_at
FROM repositories r
LEFT JOIN repository_scip_indexes si ON si.repository_id = r.id`
	arguments := []any{}
	if viewer, restricted := access.ViewerFromContext(ctx); restricted && !viewer.Admin {
		query += `
JOIN repository_access a ON a.repository_id = r.id
WHERE a.visibility = 'shared' OR a.owner_id = ? OR EXISTS (
    SELECT 1 FROM repository_access_grants g
    WHERE g.repository_id = r.id AND (
        (g.subject_type = 'user' AND lower(g.subject_id) = lower(?))`
		arguments = append(arguments, viewer.ID, viewer.ID)
		if len(viewer.Groups) > 0 {
			query += ` OR (g.subject_type = 'group' AND lower(g.subject_id) IN (` +
				strings.TrimSuffix(strings.Repeat("?,", len(viewer.Groups)), ",") + `))`
			for _, group := range viewer.Groups {
				arguments = append(arguments, strings.ToLower(group))
			}
		}
		query += `))`
	}
	query += ` ORDER BY r.name COLLATE NOCASE, r.path COLLATE NOCASE`
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var repositories []catalog.Repository
	for rows.Next() {
		repository, err := scanRepository(rows)
		if err != nil {
			return nil, err
		}
		repositories = append(repositories, repository)
	}
	return repositories, rows.Err()
}

// RepositoryByID returns one repository or sql.ErrNoRows.
func (s *Store) RepositoryByID(ctx context.Context, id int64) (catalog.Repository, error) {
	query := `
SELECT
    r.id, r.name, r.path, r.origin_url, r.default_revision, r.head_commit, r.bare,
    r.scan_state, r.scan_error, r.index_state, r.index_error, r.indexed_commit,
    r.discovered_at, r.scanned_at, r.indexed_at, r.acquisition_id,
    si.provider, si.state, si.applicable, si.revision, si.configuration,
    si.indexer, si.version, si.build_root, si.documents, si.symbols,
    si.occurrences, si.error, si.queued_at, si.started_at, si.finished_at
FROM repositories r
LEFT JOIN repository_scip_indexes si ON si.repository_id = r.id`
	arguments := []any{id}
	if viewer, restricted := access.ViewerFromContext(ctx); restricted && !viewer.Admin {
		query += `
JOIN repository_access a ON a.repository_id = r.id
WHERE r.id = ? AND (
    a.visibility = 'shared' OR a.owner_id = ? OR EXISTS (
        SELECT 1 FROM repository_access_grants g
        WHERE g.repository_id = r.id AND (
            (g.subject_type = 'user' AND lower(g.subject_id) = lower(?))`
		arguments = append(arguments, viewer.ID, viewer.ID)
		if len(viewer.Groups) > 0 {
			query += ` OR (g.subject_type = 'group' AND lower(g.subject_id) IN (` +
				strings.TrimSuffix(strings.Repeat("?,", len(viewer.Groups)), ",") + `))`
			for _, group := range viewer.Groups {
				arguments = append(arguments, strings.ToLower(group))
			}
		}
		query += `)))`
	} else {
		query += ` WHERE r.id = ?`
	}
	row := s.db.QueryRowContext(ctx, query, arguments...)
	repository, err := scanRepository(row)
	if errors.Is(err, sql.ErrNoRows) {
		// Agents and API clients see this message, so it names the missing
		// selector instead of leaking a driver-level error.
		return catalog.Repository{}, fmt.Errorf("repository %d is not indexed", id)
	}
	return repository, err
}

// RepositoryAccess is the administrator-facing policy for one repository.
// Maps, Wiki pages, exports, dependency facts, and MCP reads inherit it.
type RepositoryAccess struct {
	RepositoryID   int64
	Repository     string
	RepositoryPath string
	OwnerID        string
	Visibility     string
	Users          []string
	Groups         []string
	UpdatedAt      time.Time
}

// ListRepositoryAccess returns every policy for the protected admin surface.
func (s *Store) ListRepositoryAccess(ctx context.Context) ([]RepositoryAccess, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT r.id, r.name, r.path, a.owner_id, a.visibility, a.updated_at
FROM repositories r
JOIN repository_access a ON a.repository_id = r.id
ORDER BY r.name COLLATE NOCASE, r.path COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var policies []RepositoryAccess
	for rows.Next() {
		var policy RepositoryAccess
		var updated string
		if err := rows.Scan(&policy.RepositoryID, &policy.Repository, &policy.RepositoryPath, &policy.OwnerID, &policy.Visibility, &updated); err != nil {
			return nil, err
		}
		policy.UpdatedAt = parseTime(updated)
		policies = append(policies, policy)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range policies {
		grants, err := s.repositoryGrants(ctx, policies[index].RepositoryID)
		if err != nil {
			return nil, err
		}
		policies[index].Users = grants["user"]
		policies[index].Groups = grants["group"]
	}
	return policies, nil
}

// SetRepositoryAccess atomically replaces one repository policy.
func (s *Store) SetRepositoryAccess(ctx context.Context, policy RepositoryAccess) error {
	policy.OwnerID = strings.TrimSpace(policy.OwnerID)
	if policy.RepositoryID <= 0 || policy.OwnerID == "" {
		return errors.New("repository and owner are required")
	}
	if policy.Visibility != access.VisibilityPrivate && policy.Visibility != access.VisibilityShared {
		return errors.New("repository visibility must be private or shared")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
UPDATE repository_access
SET owner_id = ?, visibility = ?, updated_at = ?
WHERE repository_id = ?`,
		policy.OwnerID, policy.Visibility, formatTime(time.Now().UTC()), policy.RepositoryID)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf("repository %d is not indexed", policy.RepositoryID)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM repository_access_grants WHERE repository_id = ?`, policy.RepositoryID); err != nil {
		return err
	}
	for subjectType, values := range map[string][]string{"user": policy.Users, "group": policy.Groups} {
		seen := map[string]struct{}{}
		for _, value := range values {
			value = strings.TrimSpace(value)
			key := strings.ToLower(value)
			if value == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO repository_access_grants(repository_id, subject_type, subject_id)
VALUES (?, ?, ?)`, policy.RepositoryID, subjectType, value); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) repositoryGrants(ctx context.Context, repositoryID int64) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT subject_type, subject_id
FROM repository_access_grants
WHERE repository_id = ?
ORDER BY subject_type, subject_id COLLATE NOCASE`, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	output := map[string][]string{"user": {}, "group": {}}
	for rows.Next() {
		var subjectType, subjectID string
		if err := rows.Scan(&subjectType, &subjectID); err != nil {
			return nil, err
		}
		output[subjectType] = append(output[subjectType], subjectID)
	}
	return output, rows.Err()
}

// UpdateIndexState records an indexing transition.
func (s *Store) UpdateIndexState(ctx context.Context, id int64, state, indexedCommit, indexError string) error {
	indexedAt := ""
	if state == "ready" {
		indexedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE repositories
SET index_state = ?, indexed_commit = ?, index_error = ?, indexed_at = ?
WHERE id = ?`,
		state,
		indexedCommit,
		indexError,
		indexedAt,
		id,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdateSCIPIndexStatus atomically records the independent compiler-index
// lifecycle for one repository.
func (s *Store) UpdateSCIPIndexStatus(ctx context.Context, repositoryID int64, status catalog.SCIPIndexStatus) error {
	if repositoryID <= 0 {
		return errors.New("SCIP repository ID is required")
	}
	result, err := s.db.ExecContext(ctx, `
INSERT INTO repository_scip_indexes (
    repository_id, provider, state, applicable, revision, configuration,
    indexer, version, build_root, documents, symbols, occurrences, error,
    queued_at, started_at, finished_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(repository_id) DO UPDATE SET
    provider = excluded.provider,
    state = excluded.state,
    applicable = excluded.applicable,
    revision = excluded.revision,
    configuration = excluded.configuration,
    indexer = excluded.indexer,
    version = excluded.version,
    build_root = excluded.build_root,
    documents = excluded.documents,
    symbols = excluded.symbols,
    occurrences = excluded.occurrences,
    error = excluded.error,
    queued_at = excluded.queued_at,
    started_at = excluded.started_at,
    finished_at = excluded.finished_at`,
		repositoryID,
		status.Provider,
		status.State,
		status.Applicable,
		status.Revision,
		status.Configuration,
		status.Indexer,
		status.Version,
		status.BuildRoot,
		status.Documents,
		status.Symbols,
		status.Occurrences,
		status.Error,
		formatTime(status.QueuedAt),
		formatTime(status.StartedAt),
		formatTime(status.FinishedAt),
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanRepository(row rowScanner) (catalog.Repository, error) {
	var repository catalog.Repository
	var discoveredAt, scannedAt, indexedAt string
	var (
		scipProvider, scipState, scipRevision, scipConfiguration sql.NullString
		scipIndexer, scipVersion, scipBuildRoot, scipError       sql.NullString
		scipQueuedAt, scipStartedAt, scipFinishedAt              sql.NullString
		scipApplicable                                           sql.NullBool
		scipDocuments, scipSymbols, scipOccurrences              sql.NullInt64
	)
	if err := row.Scan(
		&repository.ID,
		&repository.Name,
		&repository.Path,
		&repository.OriginURL,
		&repository.DefaultRevision,
		&repository.HeadCommit,
		&repository.Bare,
		&repository.ScanState,
		&repository.ScanError,
		&repository.IndexState,
		&repository.IndexError,
		&repository.IndexedCommit,
		&discoveredAt,
		&scannedAt,
		&indexedAt,
		&repository.AcquisitionID,
		&scipProvider,
		&scipState,
		&scipApplicable,
		&scipRevision,
		&scipConfiguration,
		&scipIndexer,
		&scipVersion,
		&scipBuildRoot,
		&scipDocuments,
		&scipSymbols,
		&scipOccurrences,
		&scipError,
		&scipQueuedAt,
		&scipStartedAt,
		&scipFinishedAt,
	); err != nil {
		return catalog.Repository{}, err
	}
	repository.DiscoveredAt = parseTime(discoveredAt)
	repository.ScannedAt = parseTime(scannedAt)
	repository.IndexedAt = parseTime(indexedAt)
	if scipState.Valid {
		repository.SCIPJava = &catalog.SCIPIndexStatus{
			Provider:      scipProvider.String,
			State:         scipState.String,
			Applicable:    scipApplicable.Bool,
			Revision:      scipRevision.String,
			Configuration: scipConfiguration.String,
			Indexer:       scipIndexer.String,
			Version:       scipVersion.String,
			BuildRoot:     scipBuildRoot.String,
			Documents:     int(scipDocuments.Int64),
			Symbols:       int(scipSymbols.Int64),
			Occurrences:   int(scipOccurrences.Int64),
			Error:         scipError.String,
			QueuedAt:      parseTime(scipQueuedAt.String),
			StartedAt:     parseTime(scipStartedAt.String),
			FinishedAt:    parseTime(scipFinishedAt.String),
		}
	}
	return repository, nil
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}
	}
	return parsed
}

// CreateConversation creates durable metadata for a provider-neutral chat.
func (s *Store) CreateConversation(ctx context.Context, conversation agent.Conversation) error {
	if strings.TrimSpace(conversation.ID) == "" {
		return errors.New("conversation id is required")
	}
	if strings.TrimSpace(conversation.Author.ID) == "" {
		conversation.Author = agent.ConversationAuthor{
			ID:       "local:admin",
			Name:     "Local administrator",
			Provider: "local",
		}
	}
	now := conversation.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	updated := conversation.UpdatedAt
	if updated.IsZero() {
		updated = now
	}
	authorGroups, err := json.Marshal(conversation.Author.Groups)
	if err != nil {
		return fmt.Errorf("encode conversation author groups: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO conversations (
    id, title, provider, model, effort,
    author_id, author_name, author_email, author_provider, author_groups,
    resume_cursor, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		conversation.ID,
		strings.TrimSpace(conversation.Title),
		strings.TrimSpace(conversation.Provider),
		strings.TrimSpace(conversation.Model),
		strings.TrimSpace(conversation.Effort),
		strings.TrimSpace(conversation.Author.ID),
		strings.TrimSpace(conversation.Author.Name),
		strings.TrimSpace(conversation.Author.Email),
		strings.TrimSpace(conversation.Author.Provider),
		string(authorGroups),
		strings.TrimSpace(conversation.ResumeCursor),
		formatTime(now),
		formatTime(updated),
	)
	if err != nil {
		return fmt.Errorf("create conversation: %w", err)
	}
	return nil
}

// UpdateConversationAuthor refreshes non-secret identity metadata after a
// newly authenticated continuation request. The stable owner ID never changes.
func (s *Store) UpdateConversationAuthor(ctx context.Context, id string, author agent.ConversationAuthor) error {
	groups, err := json.Marshal(author.Groups)
	if err != nil {
		return fmt.Errorf("encode conversation author groups: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE conversations
SET author_name = ?, author_email = ?, author_provider = ?, author_groups = ?
WHERE id = ? AND author_id = ?`,
		strings.TrimSpace(author.Name),
		strings.TrimSpace(author.Email),
		strings.TrimSpace(author.Provider),
		string(groups),
		strings.TrimSpace(id),
		strings.TrimSpace(author.ID),
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return agent.ErrConversationNotFound
	}
	return nil
}

// ListConversations returns newest-first durable chat summaries.
func (s *Store) ListConversations(ctx context.Context, filter agent.ConversationFilter) ([]agent.Conversation, error) {
	query := `
SELECT
    c.id, c.title, c.provider, c.model, c.effort, c.resume_cursor,
    c.author_id, c.author_name, c.author_email, c.author_provider, c.author_groups,
    c.created_at, c.updated_at, c.input_tokens, c.output_tokens,
    COUNT(m.id)
FROM conversations c
LEFT JOIN conversation_messages m ON m.conversation_id = c.id
WHERE c.author_id = ?
GROUP BY c.id
ORDER BY c.updated_at DESC, c.id DESC`
	rows, err := s.db.QueryContext(ctx, query, strings.TrimSpace(filter.AuthorID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var conversations []agent.Conversation
	for rows.Next() {
		var conversation agent.Conversation
		var createdAt, updatedAt, authorGroups string
		if err := rows.Scan(
			&conversation.ID,
			&conversation.Title,
			&conversation.Provider,
			&conversation.Model,
			&conversation.Effort,
			&conversation.ResumeCursor,
			&conversation.Author.ID,
			&conversation.Author.Name,
			&conversation.Author.Email,
			&conversation.Author.Provider,
			&authorGroups,
			&createdAt,
			&updatedAt,
			&conversation.InputTokens,
			&conversation.OutputTokens,
			&conversation.MessageCount,
		); err != nil {
			return nil, err
		}
		conversation.CreatedAt = parseTime(createdAt)
		conversation.UpdatedAt = parseTime(updatedAt)
		_ = json.Unmarshal([]byte(authorGroups), &conversation.Author.Groups)
		conversations = append(conversations, conversation)
	}
	return conversations, rows.Err()
}

// GetConversation returns durable metadata and the complete ordered transcript.
func (s *Store) GetConversation(ctx context.Context, id string) (agent.Conversation, error) {
	var conversation agent.Conversation
	var createdAt, updatedAt string
	row := s.db.QueryRowContext(ctx, `
SELECT
    id, title, provider, model, effort, resume_cursor,
    author_id, author_name, author_email, author_provider, author_groups,
    created_at, updated_at,
    input_tokens, output_tokens
FROM conversations
WHERE id = ?`, id)
	var authorGroups string
	err := row.Scan(
		&conversation.ID,
		&conversation.Title,
		&conversation.Provider,
		&conversation.Model,
		&conversation.Effort,
		&conversation.ResumeCursor,
		&conversation.Author.ID,
		&conversation.Author.Name,
		&conversation.Author.Email,
		&conversation.Author.Provider,
		&authorGroups,
		&createdAt,
		&updatedAt,
		&conversation.InputTokens,
		&conversation.OutputTokens,
	)
	if err != nil {
		return agent.Conversation{}, err
	}
	conversation.CreatedAt = parseTime(createdAt)
	conversation.UpdatedAt = parseTime(updatedAt)
	_ = json.Unmarshal([]byte(authorGroups), &conversation.Author.Groups)

	rows, err := s.db.QueryContext(ctx, `
SELECT
    id, conversation_id, role, text, status, error,
    input_tokens, output_tokens, contexts_json, created_at
FROM conversation_messages
WHERE conversation_id = ?
ORDER BY id`, id)
	if err != nil {
		return agent.Conversation{}, err
	}
	for rows.Next() {
		var message agent.Message
		var messageCreatedAt string
		var messageContexts string
		if err := rows.Scan(
			&message.ID,
			&message.ConversationID,
			&message.Role,
			&message.Text,
			&message.Status,
			&message.Error,
			&message.InputTokens,
			&message.OutputTokens,
			&messageContexts,
			&messageCreatedAt,
		); err != nil {
			rows.Close()
			return agent.Conversation{}, err
		}
		message.CreatedAt = parseTime(messageCreatedAt)
		if err := json.Unmarshal([]byte(messageContexts), &message.Contexts); err != nil {
			rows.Close()
			return agent.Conversation{}, fmt.Errorf("decode message contexts: %w", err)
		}
		conversation.Messages = append(conversation.Messages, message)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return agent.Conversation{}, err
	}
	if err := rows.Close(); err != nil {
		return agent.Conversation{}, err
	}
	for index := range conversation.Messages {
		message := &conversation.Messages[index]
		message.Images, err = s.messageImages(ctx, message.ID)
		if err != nil {
			return agent.Conversation{}, err
		}
		message.Sources, err = s.messageCitations(ctx, message.ID)
		if err != nil {
			return agent.Conversation{}, err
		}
	}
	conversation.MessageCount = len(conversation.Messages)
	return conversation, nil
}

// AppendMessage stores one transcript entry and its filesystem-backed images.
func (s *Store) AppendMessage(ctx context.Context, message agent.Message) (agent.Message, error) {
	if message.Role != agent.RoleUser && message.Role != agent.RoleAssistant {
		return agent.Message{}, fmt.Errorf("invalid conversation role %q", message.Role)
	}
	if err := agent.ValidateImages(message.Images); err != nil {
		return agent.Message{}, err
	}
	if message.CreatedAt.IsZero() {
		message.CreatedAt = time.Now().UTC()
	}
	if message.Status == "" {
		message.Status = "complete"
	}
	if len(message.Contexts) > contextscope.MaximumContexts {
		return agent.Message{}, fmt.Errorf(
			"conversation message exceeds %d structured contexts",
			contextscope.MaximumContexts,
		)
	}
	contextsJSON, err := json.Marshal(message.Contexts)
	if err != nil {
		return agent.Message{}, fmt.Errorf("encode message contexts: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return agent.Message{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
INSERT INTO conversation_messages (
    conversation_id, role, text, status, error,
    input_tokens, output_tokens, contexts_json, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		message.ConversationID,
		message.Role,
		message.Text,
		message.Status,
		message.Error,
		message.InputTokens,
		message.OutputTokens,
		string(contextsJSON),
		formatTime(message.CreatedAt),
	)
	if err != nil {
		return agent.Message{}, fmt.Errorf("append conversation message: %w", err)
	}
	message.ID, err = result.LastInsertId()
	if err != nil {
		return agent.Message{}, err
	}

	var writtenPaths []string
	defer func() {
		if err != nil {
			for _, path := range writtenPaths {
				_ = os.Remove(path)
			}
		}
	}()
	for index, image := range message.Images {
		decoded, decodeErr := agent.DecodeImage(image)
		if decodeErr != nil {
			err = decodeErr
			return agent.Message{}, err
		}
		fileName := fmt.Sprintf("%d-%d%s", message.ID, index+1, conversationImageExtension(image.MediaType))
		absolutePath := filepath.Join(s.conversationDirectory, fileName)
		if writeErr := os.WriteFile(absolutePath, decoded, 0o600); writeErr != nil {
			err = writeErr
			return agent.Message{}, fmt.Errorf("persist conversation image: %w", writeErr)
		}
		writtenPaths = append(writtenPaths, absolutePath)
		imageName := strings.TrimSpace(image.Name)
		if imageName != "" {
			imageName = strings.ReplaceAll(imageName, `\`, "/")
			imageName = filepath.Base(imageName)
		}
		if _, insertErr := tx.ExecContext(ctx, `
INSERT INTO conversation_message_images (message_id, name, media_type, storage_path)
VALUES (?, ?, ?, ?)`,
			message.ID,
			imageName,
			strings.ToLower(strings.TrimSpace(image.MediaType)),
			fileName,
		); insertErr != nil {
			err = insertErr
			return agent.Message{}, insertErr
		}
	}
	for _, citation := range message.Sources {
		if strings.TrimSpace(citation.URL) == "" {
			continue
		}
		if _, err = tx.ExecContext(ctx, `
INSERT INTO conversation_message_citations (message_id, label, url)
VALUES (?, ?, ?)`, message.ID, citation.Label, citation.URL); err != nil {
			return agent.Message{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `
UPDATE conversations
SET
    updated_at = ?,
    input_tokens = input_tokens + ?,
    output_tokens = output_tokens + ?
WHERE id = ?`,
		formatTime(message.CreatedAt),
		message.InputTokens,
		message.OutputTokens,
		message.ConversationID,
	); err != nil {
		return agent.Message{}, err
	}
	if err = tx.Commit(); err != nil {
		return agent.Message{}, err
	}
	return message, nil
}

// RenameConversation changes only user-visible metadata.
func (s *Store) RenameConversation(ctx context.Context, id, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return errors.New("conversation title is required")
	}
	if len([]rune(title)) > 120 {
		return errors.New("conversation title exceeds 120 characters")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE conversations SET title = ?, updated_at = ? WHERE id = ?`,
		title,
		formatTime(time.Now().UTC()),
		id,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdateConversationCursor stores a provider-owned opaque resume identifier.
// It is local session metadata, never an authentication credential.
func (s *Store) UpdateConversationCursor(ctx context.Context, id, cursor string) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE conversations SET resume_cursor = ? WHERE id = ?`,
		strings.TrimSpace(cursor),
		id,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteConversation removes the transcript and only its exact RepoKarta-owned
// image files.
func (s *Store) DeleteConversation(ctx context.Context, id string) error {
	rows, err := s.db.QueryContext(ctx, `
SELECT i.storage_path
FROM conversation_message_images i
JOIN conversation_messages m ON m.id = i.message_id
WHERE m.conversation_id = ?`, id)
	if err != nil {
		return err
	}
	var storagePaths []string
	for rows.Next() {
		var storagePath string
		if err := rows.Scan(&storagePath); err != nil {
			rows.Close()
			return err
		}
		storagePaths = append(storagePaths, storagePath)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, "DELETE FROM conversations WHERE id = ?", id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	for _, storagePath := range storagePaths {
		if filepath.Base(storagePath) != storagePath {
			continue
		}
		_ = os.Remove(filepath.Join(s.conversationDirectory, storagePath))
	}
	return nil
}

func (s *Store) messageImages(ctx context.Context, messageID int64) ([]agent.Image, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT name, media_type, storage_path
FROM conversation_message_images
WHERE message_id = ?
ORDER BY id`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var images []agent.Image
	for rows.Next() {
		var name, mediaType, storagePath string
		if err := rows.Scan(&name, &mediaType, &storagePath); err != nil {
			return nil, err
		}
		if filepath.Base(storagePath) != storagePath {
			return nil, errors.New("invalid conversation image path")
		}
		content, err := os.ReadFile(filepath.Join(s.conversationDirectory, storagePath))
		if err != nil {
			return nil, err
		}
		images = append(images, agent.Image{
			Name:      name,
			MediaType: mediaType,
			Data:      base64.StdEncoding.EncodeToString(content),
		})
	}
	return images, rows.Err()
}

func (s *Store) messageCitations(ctx context.Context, messageID int64) ([]agent.Citation, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT label, url
FROM conversation_message_citations
WHERE message_id = ?
ORDER BY id`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var citations []agent.Citation
	for rows.Next() {
		var citation agent.Citation
		if err := rows.Scan(&citation.Label, &citation.URL); err != nil {
			return nil, err
		}
		citations = append(citations, citation)
	}
	return citations, rows.Err()
}

func conversationImageExtension(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/gif":
		return ".gif"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}
