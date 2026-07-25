package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/catalog"
	_ "modernc.org/sqlite"
)

const (
	currentSchemaVersion = 6

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
	// version 5 is retained so existing pre-release databases remain readable,
	// but fresh databases never create Wiki tables.
	schemaV5 = `SELECT 1;`
	schemaV6 = `SELECT 1;`
)

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
SET index_state = 'pending', index_error = '', indexed_at = ''`); err != nil {
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

// Close closes the metadata database.
func (s *Store) Close() error {
	return s.db.Close()
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

	seen := make([]string, 0, len(repositories))
	for _, repository := range repositories {
		discoveredAt := formatTime(repository.DiscoveredAt)
		scannedAt := formatTime(repository.ScannedAt)
		if _, err := tx.ExecContext(ctx, `
INSERT INTO repositories (
    name, path, origin_url, default_revision, head_commit, bare,
    scan_state, scan_error, discovered_at, scanned_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
        WHEN repositories.indexed_commit = excluded.head_commit AND repositories.index_state = 'ready'
        THEN repositories.index_state
        ELSE 'pending'
    END,
    index_error = CASE
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
			discoveredAt,
			scannedAt,
		); err != nil {
			return fmt.Errorf("sync repository %q: %w", repository.Path, err)
		}
		seen = append(seen, repository.Path)
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
	rows, err := s.db.QueryContext(ctx, `
SELECT
    id, name, path, origin_url, default_revision, head_commit, bare,
    scan_state, scan_error, index_state, index_error, indexed_commit,
    discovered_at, scanned_at, indexed_at
FROM repositories
ORDER BY name COLLATE NOCASE, path COLLATE NOCASE`)
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
	row := s.db.QueryRowContext(ctx, `
SELECT
    id, name, path, origin_url, default_revision, head_commit, bare,
    scan_state, scan_error, index_state, index_error, indexed_commit,
    discovered_at, scanned_at, indexed_at
FROM repositories
WHERE id = ?`, id)
	return scanRepository(row)
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

type rowScanner interface {
	Scan(...any) error
}

func scanRepository(row rowScanner) (catalog.Repository, error) {
	var repository catalog.Repository
	var discoveredAt, scannedAt, indexedAt string
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
	); err != nil {
		return catalog.Repository{}, err
	}
	repository.DiscoveredAt = parseTime(discoveredAt)
	repository.ScannedAt = parseTime(scannedAt)
	repository.IndexedAt = parseTime(indexedAt)
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
	now := conversation.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	updated := conversation.UpdatedAt
	if updated.IsZero() {
		updated = now
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO conversations (
    id, title, provider, model, effort, resume_cursor, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		conversation.ID,
		strings.TrimSpace(conversation.Title),
		strings.TrimSpace(conversation.Provider),
		strings.TrimSpace(conversation.Model),
		strings.TrimSpace(conversation.Effort),
		strings.TrimSpace(conversation.ResumeCursor),
		formatTime(now),
		formatTime(updated),
	)
	if err != nil {
		return fmt.Errorf("create conversation: %w", err)
	}
	return nil
}

// ListConversations returns newest-first durable chat summaries.
func (s *Store) ListConversations(ctx context.Context) ([]agent.Conversation, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT
    c.id, c.title, c.provider, c.model, c.effort, c.resume_cursor,
    c.created_at, c.updated_at, c.input_tokens, c.output_tokens,
    COUNT(m.id)
FROM conversations c
LEFT JOIN conversation_messages m ON m.conversation_id = c.id
GROUP BY c.id
ORDER BY c.updated_at DESC, c.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var conversations []agent.Conversation
	for rows.Next() {
		var conversation agent.Conversation
		var createdAt, updatedAt string
		if err := rows.Scan(
			&conversation.ID,
			&conversation.Title,
			&conversation.Provider,
			&conversation.Model,
			&conversation.Effort,
			&conversation.ResumeCursor,
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
		conversations = append(conversations, conversation)
	}
	return conversations, rows.Err()
}

// GetConversation returns durable metadata and the complete ordered transcript.
func (s *Store) GetConversation(ctx context.Context, id string) (agent.Conversation, error) {
	var conversation agent.Conversation
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx, `
SELECT
    id, title, provider, model, effort, resume_cursor, created_at, updated_at,
    input_tokens, output_tokens
FROM conversations
WHERE id = ?`, id).Scan(
		&conversation.ID,
		&conversation.Title,
		&conversation.Provider,
		&conversation.Model,
		&conversation.Effort,
		&conversation.ResumeCursor,
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

	rows, err := s.db.QueryContext(ctx, `
SELECT
    id, conversation_id, role, text, status, error,
    input_tokens, output_tokens, created_at
FROM conversation_messages
WHERE conversation_id = ?
ORDER BY id`, id)
	if err != nil {
		return agent.Conversation{}, err
	}
	for rows.Next() {
		var message agent.Message
		var messageCreatedAt string
		if err := rows.Scan(
			&message.ID,
			&message.ConversationID,
			&message.Role,
			&message.Text,
			&message.Status,
			&message.Error,
			&message.InputTokens,
			&message.OutputTokens,
			&messageCreatedAt,
		); err != nil {
			rows.Close()
			return agent.Conversation{}, err
		}
		message.CreatedAt = parseTime(messageCreatedAt)
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return agent.Message{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
INSERT INTO conversation_messages (
    conversation_id, role, text, status, error,
    input_tokens, output_tokens, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		message.ConversationID,
		message.Role,
		message.Text,
		message.Status,
		message.Error,
		message.InputTokens,
		message.OutputTokens,
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
