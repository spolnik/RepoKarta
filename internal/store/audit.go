package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/audit"
)

const (
	auditRetentionKey         = "audit_retention_v1"
	defaultAuditRetentionDays = 365
	defaultAuditMaximumEvents = 100000
)

// AppendAuditEvent appends one normalized event. No update API exists.
func (s *Store) AppendAuditEvent(ctx context.Context, event audit.Event) error {
	return appendAuditEvent(ctx, s.db, event)
}

type auditExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func appendAuditEvent(ctx context.Context, executor auditExecer, event audit.Event) error {
	event = audit.Normalize(event)
	metadata, err := json.Marshal(event.Metadata)
	if err != nil {
		return fmt.Errorf("encode audit metadata: %w", err)
	}
	_, err = executor.ExecContext(ctx, `
INSERT INTO audit_events (
    actor_id, actor_name, action, target_type, target_id, outcome,
    authentication_provider, correlation_id, metadata_json, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ActorID, event.ActorName, event.Action, event.TargetType,
		event.TargetID, event.Outcome, event.Provider, event.CorrelationID,
		string(metadata), formatTime(event.CreatedAt))
	if err != nil {
		return fmt.Errorf("append audit event: %w", err)
	}
	return nil
}

// AuditEvents returns a bounded descending page and its retention boundary.
func (s *Store) AuditEvents(ctx context.Context, filter audit.Filter) (audit.Page, error) {
	if filter.Limit <= 0 {
		filter.Limit = audit.DefaultLimit
	}
	if filter.Limit > audit.MaximumLimit {
		filter.Limit = audit.MaximumLimit
	}
	query := `
SELECT id, actor_id, actor_name, action, target_type, target_id, outcome,
       authentication_provider, correlation_id, metadata_json, created_at
FROM audit_events
WHERE 1 = 1`
	arguments := make([]any, 0, 8)
	if value := strings.TrimSpace(filter.ActorID); value != "" {
		query += ` AND lower(actor_id) = lower(?)`
		arguments = append(arguments, value)
	}
	if value := strings.TrimSpace(filter.Action); value != "" {
		query += ` AND lower(action) = lower(?)`
		arguments = append(arguments, value)
	}
	if value := strings.TrimSpace(filter.Outcome); value != "" {
		query += ` AND lower(outcome) = lower(?)`
		arguments = append(arguments, value)
	}
	if !filter.Since.IsZero() {
		query += ` AND created_at >= ?`
		arguments = append(arguments, formatTime(filter.Since))
	}
	if !filter.Until.IsZero() {
		query += ` AND created_at <= ?`
		arguments = append(arguments, formatTime(filter.Until))
	}
	if filter.BeforeID > 0 {
		query += ` AND id < ?`
		arguments = append(arguments, filter.BeforeID)
	}
	if value := strings.TrimSpace(filter.Query); value != "" {
		value = "%" + strings.ToLower(value) + "%"
		query += ` AND (
            lower(actor_id) LIKE ? OR lower(actor_name) LIKE ? OR
            lower(action) LIKE ? OR lower(target_type) LIKE ? OR
            lower(target_id) LIKE ? OR lower(correlation_id) LIKE ?
        )`
		for range 6 {
			arguments = append(arguments, value)
		}
	}
	query += ` ORDER BY id DESC LIMIT ?`
	arguments = append(arguments, filter.Limit+1)
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return audit.Page{}, fmt.Errorf("search audit events: %w", err)
	}
	defer rows.Close()
	events := make([]audit.Event, 0, filter.Limit+1)
	for rows.Next() {
		var event audit.Event
		var metadata, created string
		if err := rows.Scan(
			&event.ID, &event.ActorID, &event.ActorName, &event.Action,
			&event.TargetType, &event.TargetID, &event.Outcome, &event.Provider,
			&event.CorrelationID, &metadata, &created,
		); err != nil {
			return audit.Page{}, err
		}
		event.CreatedAt = parseTime(created)
		if err := json.Unmarshal([]byte(metadata), &event.Metadata); err != nil {
			event.Metadata = map[string]string{"decode_error": "metadata unavailable"}
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return audit.Page{}, err
	}
	page := audit.Page{Events: events}
	if len(page.Events) > filter.Limit {
		page.Events = page.Events[:filter.Limit]
		page.Truncated = true
		page.NextBefore = page.Events[len(page.Events)-1].ID
	}
	page.Retention, err = s.AuditRetention(ctx)
	return page, err
}

// AuditRetention returns configured limits and the actual retained window.
func (s *Store) AuditRetention(ctx context.Context) (audit.Retention, error) {
	retention := audit.Retention{
		Days:      defaultAuditRetentionDays,
		MaxEvents: defaultAuditMaximumEvents,
	}
	if raw, ok, err := s.AppSetting(ctx, auditRetentionKey); err != nil {
		return retention, err
	} else if ok {
		_ = json.Unmarshal([]byte(raw), &retention)
	}
	var oldest, newest sql.NullString
	if err := s.db.QueryRowContext(ctx, `
SELECT MIN(created_at), MAX(created_at), COUNT(*) FROM audit_events`).
		Scan(&oldest, &newest, &retention.EventCount); err != nil {
		return retention, err
	}
	if oldest.Valid {
		retention.OldestEventAt = parseTime(oldest.String)
		retention.CompleteSince = retention.OldestEventAt
	}
	if newest.Valid {
		retention.NewestEventAt = parseTime(newest.String)
	}
	return retention, nil
}

// SetAuditRetention persists bounded policy and purges only events outside it.
func (s *Store) SetAuditRetention(ctx context.Context, days, maxEvents int) (int64, error) {
	if days < 1 || days > 3650 {
		return 0, errors.New("audit retention days must be between 1 and 3650")
	}
	if maxEvents < 100 || maxEvents > 10000000 {
		return 0, errors.New("audit maximum events must be between 100 and 10000000")
	}
	raw, _ := json.Marshal(audit.Retention{Days: days, MaxEvents: maxEvents})
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO app_settings(key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, auditRetentionKey, string(raw)); err != nil {
		return 0, err
	}
	cutoff := formatTime(time.Now().UTC().AddDate(0, 0, -days))
	result, err := tx.ExecContext(ctx, `DELETE FROM audit_events WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	removed, _ := result.RowsAffected()
	result, err = tx.ExecContext(ctx, `
DELETE FROM audit_events
WHERE id NOT IN (SELECT id FROM audit_events ORDER BY id DESC LIMIT ?)`, maxEvents)
	if err != nil {
		return 0, err
	}
	excess, _ := result.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return removed + excess, nil
}
