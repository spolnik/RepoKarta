package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/access"
	"github.com/spolnik/RepoKarta/internal/contextscope"
)

func namedContextViewer(ctx context.Context) access.Viewer {
	if viewer, ok := access.ViewerFromContext(ctx); ok {
		if strings.TrimSpace(viewer.ID) == "" {
			viewer.ID = "anonymous"
		}
		return viewer
	}
	return access.Viewer{ID: "local:admin", Admin: true}
}

// ListNamedContextRecords returns the caller's personal contexts and all
// administrator-published shared contexts. Administrators do not implicitly
// gain visibility into another user's private definitions.
func (s *Store) ListNamedContextRecords(ctx context.Context) ([]contextscope.NamedContextRecord, error) {
	viewer := namedContextViewer(ctx)
	rows, err := s.db.QueryContext(ctx, `
SELECT id, title, description, category, visibility, default_scope, owner_id,
       managed, selectors_json, created_at, updated_at
FROM named_contexts
WHERE owner_id = ? OR visibility = 'shared'
ORDER BY
    CASE default_scope WHEN 'administrator' THEN 0 WHEN 'personal' THEN 1 ELSE 2 END,
    title COLLATE NOCASE,
    id
`, viewer.ID)
	if err != nil {
		return nil, fmt.Errorf("list named contexts: %w", err)
	}
	defer rows.Close()
	output := make([]contextscope.NamedContextRecord, 0)
	for rows.Next() {
		record, err := scanNamedContext(rows.Scan)
		if err != nil {
			return nil, err
		}
		output = append(output, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list named contexts: %w", err)
	}
	return output, nil
}

// GetNamedContextRecord reads one visible definition without leaking whether
// an inaccessible personal ID exists.
func (s *Store) GetNamedContextRecord(ctx context.Context, id string) (contextscope.NamedContextRecord, error) {
	viewer := namedContextViewer(ctx)
	record, err := scanNamedContext(s.db.QueryRowContext(ctx, `
SELECT id, title, description, category, visibility, default_scope, owner_id,
       managed, selectors_json, created_at, updated_at
FROM named_contexts
WHERE id = ? AND (owner_id = ? OR visibility = 'shared')
`, strings.TrimSpace(id), viewer.ID).Scan)
	if errors.Is(err, contextscope.ErrNamedContextNotFound) {
		return contextscope.NamedContextRecord{}, err
	}
	return record, err
}

// CreateNamedContextRecord stores one already-validated and revision-pinned
// definition.
func (s *Store) CreateNamedContextRecord(
	ctx context.Context,
	record contextscope.NamedContextRecord,
) (contextscope.NamedContextRecord, error) {
	viewer := namedContextViewer(ctx)
	if strings.TrimSpace(record.ID) == "" {
		id, err := newNamedContextID()
		if err != nil {
			return contextscope.NamedContextRecord{}, err
		}
		record.ID = id
	}
	record.OwnerID = viewer.ID
	record.Managed = record.Visibility == contextscope.VisibilityShared ||
		record.DefaultScope == contextscope.DefaultAdministrator
	if record.Managed && !viewer.Admin {
		return contextscope.NamedContextRecord{}, contextscope.ErrNamedContextForbidden
	}
	now := time.Now().UTC()
	record.CreatedAt = now
	record.UpdatedAt = now
	selectors, err := json.Marshal(record.Selectors)
	if err != nil {
		return contextscope.NamedContextRecord{}, fmt.Errorf("encode named context selectors: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO named_contexts (
    id, title, description, category, visibility, default_scope, owner_id,
    managed, selectors_json, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, record.ID, record.Title, record.Description, record.Category, record.Visibility,
		record.DefaultScope, record.OwnerID, record.Managed, string(selectors),
		formatTime(record.CreatedAt), formatTime(record.UpdatedAt))
	if isUniqueConstraint(err) {
		return contextscope.NamedContextRecord{}, contextscope.ErrNamedContextConflict
	}
	if err != nil {
		return contextscope.NamedContextRecord{}, fmt.Errorf("create named context: %w", err)
	}
	return record, nil
}

// UpdateNamedContextRecord changes an owned personal definition or an
// administrator-managed definition.
func (s *Store) UpdateNamedContextRecord(
	ctx context.Context,
	id string,
	record contextscope.NamedContextRecord,
) (contextscope.NamedContextRecord, error) {
	current, err := s.GetNamedContextRecord(ctx, id)
	if err != nil {
		return contextscope.NamedContextRecord{}, err
	}
	viewer := namedContextViewer(ctx)
	if current.Managed {
		if !viewer.Admin {
			return contextscope.NamedContextRecord{}, contextscope.ErrNamedContextForbidden
		}
	} else if current.OwnerID != viewer.ID {
		return contextscope.NamedContextRecord{}, contextscope.ErrNamedContextForbidden
	}
	managed := record.Visibility == contextscope.VisibilityShared ||
		record.DefaultScope == contextscope.DefaultAdministrator
	if managed && !viewer.Admin {
		return contextscope.NamedContextRecord{}, contextscope.ErrNamedContextForbidden
	}
	record.ID = current.ID
	record.OwnerID = current.OwnerID
	record.Managed = managed
	record.CreatedAt = current.CreatedAt
	record.UpdatedAt = time.Now().UTC()
	selectors, err := json.Marshal(record.Selectors)
	if err != nil {
		return contextscope.NamedContextRecord{}, fmt.Errorf("encode named context selectors: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE named_contexts
SET title = ?, description = ?, category = ?, visibility = ?,
    default_scope = ?, managed = ?, selectors_json = ?, updated_at = ?
WHERE id = ?
`, record.Title, record.Description, record.Category, record.Visibility,
		record.DefaultScope, record.Managed, string(selectors),
		formatTime(record.UpdatedAt), record.ID)
	if isUniqueConstraint(err) {
		return contextscope.NamedContextRecord{}, contextscope.ErrNamedContextConflict
	}
	if err != nil {
		return contextscope.NamedContextRecord{}, fmt.Errorf("update named context: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return contextscope.NamedContextRecord{}, fmt.Errorf("update named context: %w", err)
	}
	if affected != 1 {
		return contextscope.NamedContextRecord{}, contextscope.ErrNamedContextNotFound
	}
	return record, nil
}

// DeleteNamedContextRecord removes an editable definition.
func (s *Store) DeleteNamedContextRecord(ctx context.Context, id string) error {
	current, err := s.GetNamedContextRecord(ctx, id)
	if err != nil {
		return err
	}
	viewer := namedContextViewer(ctx)
	if (current.Managed && !viewer.Admin) || (!current.Managed && current.OwnerID != viewer.ID) {
		return contextscope.ErrNamedContextForbidden
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM named_contexts WHERE id = ?`, current.ID)
	if err != nil {
		return fmt.Errorf("delete named context: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete named context: %w", err)
	}
	if affected != 1 {
		return contextscope.ErrNamedContextNotFound
	}
	return nil
}

type namedContextScanner func(...any) error

func scanNamedContext(scan namedContextScanner) (contextscope.NamedContextRecord, error) {
	var (
		record        contextscope.NamedContextRecord
		managed       bool
		selectorsJSON string
		createdAt     string
		updatedAt     string
	)
	err := scan(
		&record.ID,
		&record.Title,
		&record.Description,
		&record.Category,
		&record.Visibility,
		&record.DefaultScope,
		&record.OwnerID,
		&managed,
		&selectorsJSON,
		&createdAt,
		&updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return record, contextscope.ErrNamedContextNotFound
	}
	if err != nil {
		return record, fmt.Errorf("read named context: %w", err)
	}
	record.Managed = managed
	if err := json.Unmarshal([]byte(selectorsJSON), &record.Selectors); err != nil {
		return record, fmt.Errorf("decode named context selectors: %w", err)
	}
	record.CreatedAt = parseTime(createdAt)
	record.UpdatedAt = parseTime(updatedAt)
	return record, nil
}

func newNamedContextID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate named context ID: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}

func isUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint") ||
		strings.Contains(message, "constraint failed: unique")
}
