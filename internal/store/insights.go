package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/insights"
)

func (s *Store) SaveInsightRun(ctx context.Context, run insights.Run, observations []insights.Observation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	metadata, err := json.Marshal(run.Metadata)
	if err != nil {
		return fmt.Errorf("encode insight run metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO insight_runs (
    id, repository_id, revision, branch, tool, tool_version, source_kind,
    source_ref, rule_pack, configuration, license, status, status_message,
    confidence, observed_at, ingested_at, metadata_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.RepositoryID, run.Revision, run.Branch, run.Tool,
		run.ToolVersion, run.SourceKind, run.SourceRef, run.RulePack,
		run.Configuration, run.License, run.Status, run.StatusMessage,
		run.Confidence, formatTime(run.ObservedAt), formatTime(run.IngestedAt),
		string(metadata)); err != nil {
		return fmt.Errorf("store insight run: %w", err)
	}
	for _, observation := range observations {
		observationMetadata, marshalErr := json.Marshal(observation.Metadata)
		if marshalErr != nil {
			return fmt.Errorf("encode insight observation metadata: %w", marshalErr)
		}
		codeFlow, marshalErr := json.Marshal(observation.CodeFlows)
		if marshalErr != nil {
			return fmt.Errorf("encode insight code flow: %w", marshalErr)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO insight_observations (
    run_id, repository_id, revision, branch, kind, key, value, unit, severity,
    message, path, start_line, end_line, language, owner, fingerprint,
    suppressed, state, confidence, metadata_json, code_flow_json, source_url,
    observed_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			run.ID, run.RepositoryID, run.Revision, run.Branch, observation.Kind,
			observation.Key, observation.Value, observation.Unit,
			observation.Severity, observation.Message, observation.Path,
			observation.StartLine, observation.EndLine, observation.Language,
			observation.Owner, observation.Fingerprint, observation.Suppressed,
			observation.State, observation.Confidence, string(observationMetadata),
			string(codeFlow), observation.SourceURL, formatTime(observation.ObservedAt)); err != nil {
			return fmt.Errorf("store insight observation: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) ListInsightRuns(ctx context.Context, filter insights.Filter) ([]insights.Run, error) {
	query := `
SELECT r.id, r.repository_id, repositories.name, r.revision, r.branch, r.tool,
       r.tool_version, r.source_kind, r.source_ref, r.rule_pack,
       r.configuration, r.license, r.status, r.status_message, r.confidence,
       r.observed_at, r.ingested_at, r.metadata_json,
       (SELECT COUNT(*) FROM insight_observations o WHERE o.run_id = r.id)
FROM insight_runs r
JOIN repositories ON repositories.id = r.repository_id`
	where, arguments := insightWhere("r", filter, false)
	query += where + " ORDER BY r.observed_at DESC, r.id DESC LIMIT ?"
	arguments = append(arguments, insightLimit(filter.Limit))
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var output []insights.Run
	for rows.Next() {
		var run insights.Run
		var observed, ingested, metadata string
		if err := rows.Scan(
			&run.ID, &run.RepositoryID, &run.Repository, &run.Revision,
			&run.Branch, &run.Tool, &run.ToolVersion, &run.SourceKind,
			&run.SourceRef, &run.RulePack, &run.Configuration, &run.License,
			&run.Status, &run.StatusMessage, &run.Confidence, &observed,
			&ingested, &metadata, &run.ObservationCount,
		); err != nil {
			return nil, err
		}
		run.ObservedAt = parseTime(observed)
		run.IngestedAt = parseTime(ingested)
		_ = json.Unmarshal([]byte(metadata), &run.Metadata)
		output = append(output, run)
	}
	return output, rows.Err()
}

func (s *Store) ListInsightObservations(ctx context.Context, filter insights.Filter) ([]insights.Observation, error) {
	query := `
SELECT o.id, o.run_id, o.repository_id, repositories.name, o.revision, o.branch,
       r.tool, r.tool_version, o.kind, o.key, o.value, o.unit, o.severity, o.message, o.path,
       o.start_line, o.end_line, o.language, o.owner, o.fingerprint,
       o.suppressed, o.state, o.confidence, o.metadata_json, o.code_flow_json,
       o.source_url, o.observed_at
FROM insight_observations o
JOIN repositories ON repositories.id = o.repository_id
JOIN insight_runs r ON r.id = o.run_id`
	where, arguments := insightWhere("o", filter, true)
	query += where + " ORDER BY o.observed_at DESC, o.id DESC LIMIT ?"
	arguments = append(arguments, insightLimit(filter.Limit))
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var output []insights.Observation
	for rows.Next() {
		var observation insights.Observation
		var value sql.NullFloat64
		var metadata, codeFlow, observed string
		if err := rows.Scan(
			&observation.ID, &observation.RunID, &observation.RepositoryID,
			&observation.Repository, &observation.Revision, &observation.Branch,
			&observation.Tool, &observation.ToolVersion,
			&observation.Kind, &observation.Key, &value, &observation.Unit,
			&observation.Severity, &observation.Message, &observation.Path,
			&observation.StartLine, &observation.EndLine, &observation.Language,
			&observation.Owner, &observation.Fingerprint,
			&observation.Suppressed, &observation.State,
			&observation.Confidence, &metadata, &codeFlow,
			&observation.SourceURL, &observed,
		); err != nil {
			return nil, err
		}
		if value.Valid {
			observation.Value = &value.Float64
		}
		observation.ObservedAt = parseTime(observed)
		_ = json.Unmarshal([]byte(metadata), &observation.Metadata)
		_ = json.Unmarshal([]byte(codeFlow), &observation.CodeFlows)
		output = append(output, observation)
	}
	return output, rows.Err()
}

func insightWhere(alias string, filter insights.Filter, observations bool) (string, []any) {
	var conditions []string
	var arguments []any
	if filter.RepositoryID > 0 {
		conditions = append(conditions, alias+".repository_id = ?")
		arguments = append(arguments, filter.RepositoryID)
	} else if len(filter.RepositoryIDs) > 0 {
		conditions = append(conditions, alias+".repository_id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(filter.RepositoryIDs)), ",")+")")
		for _, id := range filter.RepositoryIDs {
			arguments = append(arguments, id)
		}
	}
	addEqual := func(column, value string) {
		if strings.TrimSpace(value) != "" {
			conditions = append(conditions, "lower("+alias+"."+column+") = lower(?)")
			arguments = append(arguments, strings.TrimSpace(value))
		}
	}
	addEqual("revision", filter.Revision)
	addEqual("branch", filter.Branch)
	if observations {
		addEqual("language", filter.Language)
		addEqual("key", filter.Rule)
		addEqual("severity", filter.Severity)
		addEqual("owner", filter.Owner)
		addEqual("kind", filter.Kind)
		if filter.Directory != "" {
			conditions = append(conditions, "lower("+alias+".path) LIKE lower(?)")
			arguments = append(arguments, strings.Trim(strings.ReplaceAll(filter.Directory, "\\", "/"), "/")+"/%")
		}
		if filter.File != "" {
			conditions = append(conditions, "lower("+alias+".path) LIKE lower(?)")
			arguments = append(arguments, "%"+strings.ReplaceAll(filter.File, "\\", "/")+"%")
		}
		if filter.Tool != "" {
			conditions = append(conditions, "lower(r.tool) = lower(?)")
			arguments = append(arguments, strings.TrimSpace(filter.Tool))
		}
	} else {
		addEqual("tool", filter.Tool)
	}
	if !filter.Since.IsZero() {
		conditions = append(conditions, alias+".observed_at >= ?")
		arguments = append(arguments, formatTime(filter.Since))
	}
	if !filter.Until.IsZero() {
		conditions = append(conditions, alias+".observed_at <= ?")
		arguments = append(arguments, formatTime(filter.Until))
	}
	if !filter.IncludeQuarantined {
		conditions = append(conditions, "r.status <> 'quarantined'")
	}
	if len(conditions) == 0 {
		return "", arguments
	}
	return " WHERE " + strings.Join(conditions, " AND "), arguments
}

func insightLimit(value int) int {
	if value <= 0 {
		return 500
	}
	if value > 5000 {
		return 5000
	}
	return value
}

func (s *Store) DeleteOldInsightRuns(ctx context.Context, repositoryID int64, tool string, keep int) error {
	if keep < 1 {
		keep = 1
	}
	_, err := s.db.ExecContext(ctx, `
DELETE FROM insight_runs
WHERE repository_id = ? AND lower(tool) = lower(?) AND id NOT IN (
    SELECT id FROM insight_runs
    WHERE repository_id = ? AND lower(tool) = lower(?)
    ORDER BY observed_at DESC, id DESC
    LIMIT ?
)`, repositoryID, tool, repositoryID, tool, keep)
	return err
}

func (s *Store) ListInsightThresholds(ctx context.Context, repositoryID int64) ([]insights.Threshold, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, repository_id, key, operator, value, severity, enabled, updated_at
FROM insight_thresholds
WHERE repository_id IN (0, ?)
ORDER BY repository_id, key COLLATE NOCASE`, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var output []insights.Threshold
	for rows.Next() {
		var threshold insights.Threshold
		var updated string
		if err := rows.Scan(&threshold.ID, &threshold.RepositoryID, &threshold.Key,
			&threshold.Operator, &threshold.Value, &threshold.Severity,
			&threshold.Enabled, &updated); err != nil {
			return nil, err
		}
		threshold.UpdatedAt = parseTime(updated)
		output = append(output, threshold)
	}
	return output, rows.Err()
}

func (s *Store) UpsertInsightThreshold(ctx context.Context, threshold insights.Threshold) (insights.Threshold, error) {
	threshold.UpdatedAt = time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO insight_thresholds(repository_id, key, operator, value, severity, enabled, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(repository_id, key) DO UPDATE SET
    operator = excluded.operator,
    value = excluded.value,
    severity = excluded.severity,
    enabled = excluded.enabled,
    updated_at = excluded.updated_at`,
		threshold.RepositoryID, threshold.Key, threshold.Operator, threshold.Value,
		threshold.Severity, threshold.Enabled, formatTime(threshold.UpdatedAt)); err != nil {
		return insights.Threshold{}, err
	}
	err := s.db.QueryRowContext(ctx, `
SELECT id FROM insight_thresholds WHERE repository_id = ? AND key = ?`,
		threshold.RepositoryID, threshold.Key).Scan(&threshold.ID)
	return threshold, err
}

func (s *Store) UpsertSonarConnection(ctx context.Context, connection insights.SonarConnection) (insights.SonarConnection, error) {
	connection.UpdatedAt = time.Now().UTC()
	if connection.State == "" {
		connection.State = insights.StatusStale
	}
	if connection.NextPollAt.IsZero() {
		connection.NextPollAt = connection.UpdatedAt
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO sonar_connections(
    repository_id, base_url, project_key, token_env, poll_interval_minutes,
    retention_runs, enabled, state, status_message, last_polled_at, next_poll_at,
    failure_count, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(repository_id) DO UPDATE SET
    base_url = excluded.base_url,
    project_key = excluded.project_key,
    token_env = excluded.token_env,
    poll_interval_minutes = excluded.poll_interval_minutes,
    retention_runs = excluded.retention_runs,
    enabled = excluded.enabled,
    updated_at = excluded.updated_at`,
		connection.RepositoryID, connection.BaseURL, connection.ProjectKey,
		connection.TokenEnv, connection.PollIntervalMinutes, connection.RetentionRuns, connection.Enabled,
		connection.State, connection.StatusMessage,
		formatOptionalTime(connection.LastPolledAt), formatOptionalTime(connection.NextPollAt),
		connection.FailureCount, formatTime(connection.UpdatedAt)); err != nil {
		return insights.SonarConnection{}, err
	}
	err := s.db.QueryRowContext(ctx, `SELECT id FROM sonar_connections WHERE repository_id = ?`,
		connection.RepositoryID).Scan(&connection.ID)
	return connection, err
}

func (s *Store) ListSonarConnections(ctx context.Context, dueOnly bool) ([]insights.SonarConnection, error) {
	query := `
SELECT c.id, c.repository_id, r.name, c.base_url, c.project_key, c.token_env,
       c.poll_interval_minutes, c.retention_runs, c.enabled, c.state, c.status_message,
       c.last_polled_at, c.next_poll_at, c.failure_count, c.updated_at
FROM sonar_connections c
JOIN repositories r ON r.id = c.repository_id`
	var arguments []any
	if dueOnly {
		query += ` WHERE c.enabled = 1 AND (c.next_poll_at = '' OR c.next_poll_at <= ?)`
		arguments = append(arguments, formatTime(time.Now().UTC()))
	}
	query += ` ORDER BY r.name COLLATE NOCASE`
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var output []insights.SonarConnection
	for rows.Next() {
		var connection insights.SonarConnection
		var last, next, updated string
		if err := rows.Scan(
			&connection.ID, &connection.RepositoryID, &connection.Repository,
			&connection.BaseURL, &connection.ProjectKey, &connection.TokenEnv,
			&connection.PollIntervalMinutes, &connection.RetentionRuns, &connection.Enabled,
			&connection.State, &connection.StatusMessage, &last, &next,
			&connection.FailureCount, &updated,
		); err != nil {
			return nil, err
		}
		connection.LastPolledAt = parseTime(last)
		connection.NextPollAt = parseTime(next)
		connection.UpdatedAt = parseTime(updated)
		output = append(output, connection)
	}
	return output, rows.Err()
}

func (s *Store) UpdateSonarConnectionState(ctx context.Context, connection insights.SonarConnection) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE sonar_connections
SET state = ?, status_message = ?, last_polled_at = ?, next_poll_at = ?,
    failure_count = ?, updated_at = ?
WHERE id = ?`,
		connection.State, connection.StatusMessage,
		formatOptionalTime(connection.LastPolledAt),
		formatOptionalTime(connection.NextPollAt), connection.FailureCount,
		formatTime(time.Now().UTC()), connection.ID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errors.New("SonarQube connection not found")
	}
	return nil
}

func formatOptionalTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return formatTime(value)
}
