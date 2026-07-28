package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/access"
	"github.com/spolnik/RepoKarta/internal/searchworkspace"
)

func searchViewer(ctx context.Context) access.Viewer {
	if viewer, ok := access.ViewerFromContext(ctx); ok {
		if strings.TrimSpace(viewer.ID) == "" {
			viewer.ID = "anonymous"
		}
		return viewer
	}
	return access.Viewer{ID: "local:admin", Admin: true}
}

func newSearchWorkspaceID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate search workspace ID: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func (s *Store) AddRecentSearch(
	ctx context.Context,
	record searchworkspace.RecentRecord,
) error {
	viewer := searchViewer(ctx)
	now := time.Now().UTC()
	if !record.ExecutedAt.IsZero() {
		now = record.ExecutedAt.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin recent search update: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO search_history(author_id, request_json, result_count, executed_at)
VALUES (?, ?, ?, ?)
`, viewer.ID, record.RequestJSON, max(0, record.ResultCount), formatTime(now)); err != nil {
		return fmt.Errorf("record recent search: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM search_history
WHERE author_id = ? AND id NOT IN (
    SELECT id FROM search_history
    WHERE author_id = ?
    ORDER BY executed_at DESC, id DESC
    LIMIT 50
)
`, viewer.ID, viewer.ID); err != nil {
		return fmt.Errorf("bound recent search history: %w", err)
	}
	return tx.Commit()
}

func (s *Store) ListRecentSearches(
	ctx context.Context,
	limit int,
) ([]searchworkspace.RecentRecord, error) {
	viewer := searchViewer(ctx)
	limit = min(max(limit, 1), 50)
	rows, err := s.db.QueryContext(ctx, `
SELECT id, author_id, request_json, result_count, executed_at
FROM search_history
WHERE author_id = ?
ORDER BY executed_at DESC, id DESC
LIMIT ?
`, viewer.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent searches: %w", err)
	}
	defer rows.Close()
	output := make([]searchworkspace.RecentRecord, 0)
	for rows.Next() {
		var record searchworkspace.RecentRecord
		var executed string
		if err := rows.Scan(&record.ID, &record.AuthorID, &record.RequestJSON, &record.ResultCount, &executed); err != nil {
			return nil, fmt.Errorf("read recent search: %w", err)
		}
		record.ExecutedAt = parseTime(executed)
		output = append(output, record)
	}
	return output, rows.Err()
}

func (s *Store) ListSavedSearchRecords(
	ctx context.Context,
) ([]searchworkspace.SavedRecord, error) {
	viewer := searchViewer(ctx)
	rows, err := s.db.QueryContext(ctx, `
SELECT id, author_id, title, description, visibility, managed, revision_policy,
       request_json, created_at, updated_at
FROM saved_searches
WHERE author_id = ? OR visibility = 'shared'
ORDER BY managed DESC, title COLLATE NOCASE, id
`, viewer.ID)
	if err != nil {
		return nil, fmt.Errorf("list saved searches: %w", err)
	}
	defer rows.Close()
	output := make([]searchworkspace.SavedRecord, 0)
	for rows.Next() {
		record, err := scanSavedSearch(rows.Scan)
		if err != nil {
			return nil, err
		}
		output = append(output, record)
	}
	return output, rows.Err()
}

func (s *Store) GetSavedSearchRecord(
	ctx context.Context,
	id string,
) (searchworkspace.SavedRecord, error) {
	viewer := searchViewer(ctx)
	return scanSavedSearch(s.db.QueryRowContext(ctx, `
SELECT id, author_id, title, description, visibility, managed, revision_policy,
       request_json, created_at, updated_at
FROM saved_searches
WHERE id = ? AND (author_id = ? OR visibility = 'shared')
`, strings.TrimSpace(id), viewer.ID).Scan)
}

func (s *Store) CreateSavedSearchRecord(
	ctx context.Context,
	record searchworkspace.SavedRecord,
) (searchworkspace.SavedRecord, error) {
	viewer := searchViewer(ctx)
	if record.ID == "" {
		id, err := newSearchWorkspaceID()
		if err != nil {
			return record, err
		}
		record.ID = id
	}
	record.AuthorID = viewer.ID
	record.Managed = record.Visibility == "shared"
	if record.Managed && !viewer.Admin {
		return record, searchworkspace.ErrForbidden
	}
	now := time.Now().UTC()
	record.CreatedAt, record.UpdatedAt = now, now
	_, err := s.db.ExecContext(ctx, `
INSERT INTO saved_searches(
    id, author_id, title, description, visibility, managed, revision_policy,
    request_json, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, record.ID, record.AuthorID, record.Title, record.Description, record.Visibility,
		record.Managed, record.RevisionPolicy, record.RequestJSON,
		formatTime(now), formatTime(now))
	if isUniqueConstraint(err) {
		return record, searchworkspace.ErrConflict
	}
	if err != nil {
		return record, fmt.Errorf("create saved search: %w", err)
	}
	return record, nil
}

func (s *Store) UpdateSavedSearchRecord(
	ctx context.Context,
	id string,
	record searchworkspace.SavedRecord,
) (searchworkspace.SavedRecord, error) {
	current, err := s.GetSavedSearchRecord(ctx, id)
	if err != nil {
		return record, err
	}
	viewer := searchViewer(ctx)
	if (current.Managed && !viewer.Admin) || (!current.Managed && current.AuthorID != viewer.ID) {
		return record, searchworkspace.ErrForbidden
	}
	record.ID = current.ID
	record.AuthorID = current.AuthorID
	record.CreatedAt = current.CreatedAt
	record.Managed = record.Visibility == "shared"
	if record.Managed && !viewer.Admin {
		return record, searchworkspace.ErrForbidden
	}
	record.UpdatedAt = time.Now().UTC()
	_, err = s.db.ExecContext(ctx, `
UPDATE saved_searches
SET title = ?, description = ?, visibility = ?, managed = ?, revision_policy = ?,
    request_json = ?, updated_at = ?
WHERE id = ?
`, record.Title, record.Description, record.Visibility, record.Managed,
		record.RevisionPolicy, record.RequestJSON, formatTime(record.UpdatedAt), record.ID)
	if err != nil {
		return record, fmt.Errorf("update saved search: %w", err)
	}
	return record, nil
}

func (s *Store) DeleteSavedSearchRecord(ctx context.Context, id string) error {
	current, err := s.GetSavedSearchRecord(ctx, id)
	if err != nil {
		return err
	}
	viewer := searchViewer(ctx)
	if (current.Managed && !viewer.Admin) || (!current.Managed && current.AuthorID != viewer.ID) {
		return searchworkspace.ErrForbidden
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM saved_searches WHERE id = ?`, current.ID)
	return err
}

func (s *Store) UpsertSearchMonitorRecord(
	ctx context.Context,
	record searchworkspace.MonitorRecord,
) (searchworkspace.MonitorRecord, error) {
	saved, err := s.GetSavedSearchRecord(ctx, record.SavedSearchID)
	if err != nil {
		return record, err
	}
	viewer := searchViewer(ctx)
	if saved.AuthorID != viewer.ID && !viewer.Admin {
		return record, searchworkspace.ErrForbidden
	}
	var current searchworkspace.MonitorRecord
	current, err = s.GetSearchMonitorBySavedSearch(ctx, record.SavedSearchID)
	now := time.Now().UTC()
	if errors.Is(err, searchworkspace.ErrNotFound) {
		id, generateErr := newSearchWorkspaceID()
		if generateErr != nil {
			return record, generateErr
		}
		record.ID, record.AuthorID = id, saved.AuthorID
		record.CreatedAt, record.UpdatedAt = now, now
		_, err = s.db.ExecContext(ctx, `
INSERT INTO search_monitors(
    id, saved_search_id, author_id, enabled, history_limit, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?)
`, record.ID, record.SavedSearchID, record.AuthorID, record.Enabled,
			record.HistoryLimit, formatTime(now), formatTime(now))
		return record, err
	}
	if err != nil {
		return record, err
	}
	current.Enabled = record.Enabled
	current.HistoryLimit = record.HistoryLimit
	current.UpdatedAt = now
	_, err = s.db.ExecContext(ctx, `
UPDATE search_monitors SET enabled = ?, history_limit = ?, updated_at = ? WHERE id = ?
`, current.Enabled, current.HistoryLimit, formatTime(now), current.ID)
	return current, err
}

func (s *Store) GetSearchMonitorBySavedSearch(
	ctx context.Context,
	savedSearchID string,
) (searchworkspace.MonitorRecord, error) {
	viewer := searchViewer(ctx)
	return scanSearchMonitor(s.db.QueryRowContext(ctx, `
SELECT id, saved_search_id, author_id, enabled, history_limit, created_at, updated_at
FROM search_monitors
WHERE saved_search_id = ? AND (author_id = ? OR ?)
`, strings.TrimSpace(savedSearchID), viewer.ID, viewer.Admin).Scan)
}

func (s *Store) GetSearchMonitorRecord(
	ctx context.Context,
	id string,
) (searchworkspace.MonitorRecord, error) {
	viewer := searchViewer(ctx)
	return scanSearchMonitor(s.db.QueryRowContext(ctx, `
SELECT id, saved_search_id, author_id, enabled, history_limit, created_at, updated_at
FROM search_monitors
WHERE id = ? AND (author_id = ? OR ?)
`, strings.TrimSpace(id), viewer.ID, viewer.Admin).Scan)
}

func (s *Store) AddSearchMonitorRun(
	ctx context.Context,
	record searchworkspace.RunRecord,
	historyLimit int,
) (searchworkspace.RunRecord, error) {
	monitor, err := s.GetSearchMonitorRecord(ctx, record.MonitorID)
	if err != nil {
		return record, err
	}
	record.CreatedAt = time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return record, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
INSERT INTO search_monitor_runs(
    monitor_id, revision_key, result_keys_json, added_json, removed_json,
    match_count, status, notification_status, error, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, monitor.ID, record.RevisionKey, record.ResultKeysJSON, record.AddedJSON,
		record.RemovedJSON, record.MatchCount, record.Status,
		record.NotificationStatus, record.Error, formatTime(record.CreatedAt))
	if err != nil {
		return record, fmt.Errorf("record search monitor run: %w", err)
	}
	record.ID, _ = result.LastInsertId()
	if _, err := tx.ExecContext(ctx, `
DELETE FROM search_monitor_runs
WHERE monitor_id = ? AND id NOT IN (
    SELECT id FROM search_monitor_runs
    WHERE monitor_id = ?
    ORDER BY created_at DESC, id DESC
    LIMIT ?
)
`, monitor.ID, monitor.ID, min(max(historyLimit, 1), 100)); err != nil {
		return record, fmt.Errorf("bound search monitor history: %w", err)
	}
	return record, tx.Commit()
}

func (s *Store) ListSearchMonitorRuns(
	ctx context.Context,
	monitorID string,
	limit int,
) ([]searchworkspace.RunRecord, error) {
	monitor, err := s.GetSearchMonitorRecord(ctx, monitorID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, monitor_id, revision_key, result_keys_json, added_json, removed_json,
       match_count, status, notification_status, error, created_at
FROM search_monitor_runs
WHERE monitor_id = ?
ORDER BY created_at DESC, id DESC
LIMIT ?
`, monitor.ID, min(max(limit, 1), 100))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	output := make([]searchworkspace.RunRecord, 0)
	for rows.Next() {
		record, err := scanMonitorRun(rows.Scan)
		if err != nil {
			return nil, err
		}
		output = append(output, record)
	}
	return output, rows.Err()
}

type workspaceScanner func(...any) error

func scanSavedSearch(scan workspaceScanner) (searchworkspace.SavedRecord, error) {
	var record searchworkspace.SavedRecord
	var created, updated string
	if err := scan(
		&record.ID, &record.AuthorID, &record.Title, &record.Description,
		&record.Visibility, &record.Managed, &record.RevisionPolicy,
		&record.RequestJSON, &created, &updated,
	); errors.Is(err, sql.ErrNoRows) {
		return record, searchworkspace.ErrNotFound
	} else if err != nil {
		return record, fmt.Errorf("read saved search: %w", err)
	}
	record.CreatedAt, record.UpdatedAt = parseTime(created), parseTime(updated)
	return record, nil
}

func scanSearchMonitor(scan workspaceScanner) (searchworkspace.MonitorRecord, error) {
	var record searchworkspace.MonitorRecord
	var created, updated string
	if err := scan(
		&record.ID, &record.SavedSearchID, &record.AuthorID, &record.Enabled,
		&record.HistoryLimit, &created, &updated,
	); errors.Is(err, sql.ErrNoRows) {
		return record, searchworkspace.ErrNotFound
	} else if err != nil {
		return record, fmt.Errorf("read search monitor: %w", err)
	}
	record.CreatedAt, record.UpdatedAt = parseTime(created), parseTime(updated)
	return record, nil
}

func scanMonitorRun(scan workspaceScanner) (searchworkspace.RunRecord, error) {
	var record searchworkspace.RunRecord
	var created string
	if err := scan(
		&record.ID, &record.MonitorID, &record.RevisionKey,
		&record.ResultKeysJSON, &record.AddedJSON, &record.RemovedJSON,
		&record.MatchCount, &record.Status, &record.NotificationStatus,
		&record.Error, &created,
	); err != nil {
		return record, fmt.Errorf("read search monitor run: %w", err)
	}
	record.CreatedAt = parseTime(created)
	return record, nil
}
