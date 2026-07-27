package store

import (
	"context"
	"fmt"
	"time"

	"github.com/spolnik/RepoKarta/internal/dependencies"
)

// ListDependencyObservations returns the small, fleet-wide registry cache.
func (s *Store) ListDependencyObservations(ctx context.Context) ([]dependencies.Observation, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT ecosystem, registry, package, latest_stable, status, error, etag,
       last_modified, observed_at, expires_at
FROM dependency_registry_observations
ORDER BY ecosystem, registry, package`)
	if err != nil {
		return nil, fmt.Errorf("list dependency registry observations: %w", err)
	}
	defer rows.Close()
	output := make([]dependencies.Observation, 0)
	for rows.Next() {
		var observation dependencies.Observation
		var observedAt, expiresAt string
		if err := rows.Scan(
			&observation.Ecosystem,
			&observation.Registry,
			&observation.Package,
			&observation.LatestStable,
			&observation.Status,
			&observation.Error,
			&observation.ETag,
			&observation.LastModified,
			&observedAt,
			&expiresAt,
		); err != nil {
			return nil, fmt.Errorf("scan dependency registry observation: %w", err)
		}
		observation.ObservedAt, err = time.Parse(time.RFC3339Nano, observedAt)
		if err != nil {
			return nil, fmt.Errorf("parse dependency observation time: %w", err)
		}
		observation.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAt)
		if err != nil {
			return nil, fmt.Errorf("parse dependency expiry time: %w", err)
		}
		output = append(output, observation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dependency registry observations: %w", err)
	}
	return output, nil
}

// UpsertDependencyObservation atomically replaces one registry cache row.
func (s *Store) UpsertDependencyObservation(
	ctx context.Context,
	observation dependencies.Observation,
) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO dependency_registry_observations (
    ecosystem, registry, package, latest_stable, status, error, etag,
    last_modified, observed_at, expires_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(ecosystem, registry, package) DO UPDATE SET
    latest_stable = excluded.latest_stable,
    status = excluded.status,
    error = excluded.error,
    etag = excluded.etag,
    last_modified = excluded.last_modified,
    observed_at = excluded.observed_at,
    expires_at = excluded.expires_at`,
		observation.Ecosystem,
		observation.Registry,
		observation.Package,
		observation.LatestStable,
		observation.Status,
		observation.Error,
		observation.ETag,
		observation.LastModified,
		observation.ObservedAt.UTC().Format(time.RFC3339Nano),
		observation.ExpiresAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("upsert dependency registry observation: %w", err)
	}
	return nil
}

// ListRuntimeTopologyObservations returns observations whose recorded window
// overlaps the requested interval.
func (s *Store) ListRuntimeTopologyObservations(
	ctx context.Context,
	observedFrom, observedTo time.Time,
) ([]dependencies.RuntimeTopologyObservation, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT provider, environment, source_name, source_kind, target_name, target_kind,
       protocol, interaction, transport, observed_from, observed_to,
       request_count, error_count, latency_p95_ms, imported_at
FROM runtime_topology_observations
WHERE observed_to >= ? AND observed_from <= ?
ORDER BY observed_to DESC, source_name, target_name`,
		observedFrom.UTC().Format(time.RFC3339Nano),
		observedTo.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, fmt.Errorf("list runtime topology observations: %w", err)
	}
	defer rows.Close()
	output := make([]dependencies.RuntimeTopologyObservation, 0)
	for rows.Next() {
		var observation dependencies.RuntimeTopologyObservation
		var from, to, importedAt string
		if err := rows.Scan(
			&observation.Provider,
			&observation.Environment,
			&observation.SourceName,
			&observation.SourceKind,
			&observation.TargetName,
			&observation.TargetKind,
			&observation.Protocol,
			&observation.Interaction,
			&observation.Transport,
			&from,
			&to,
			&observation.RequestCount,
			&observation.ErrorCount,
			&observation.LatencyP95MS,
			&importedAt,
		); err != nil {
			return nil, fmt.Errorf("scan runtime topology observation: %w", err)
		}
		observation.ObservedFrom, err = time.Parse(time.RFC3339Nano, from)
		if err != nil {
			return nil, fmt.Errorf("parse runtime topology start: %w", err)
		}
		observation.ObservedTo, err = time.Parse(time.RFC3339Nano, to)
		if err != nil {
			return nil, fmt.Errorf("parse runtime topology end: %w", err)
		}
		observation.ImportedAt, err = time.Parse(time.RFC3339Nano, importedAt)
		if err != nil {
			return nil, fmt.Errorf("parse runtime topology import time: %w", err)
		}
		output = append(output, observation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runtime topology observations: %w", err)
	}
	return output, nil
}

// UpsertRuntimeTopologyObservations imports one bounded provider window and
// prunes expired windows in the same transaction.
func (s *Store) UpsertRuntimeTopologyObservations(
	ctx context.Context,
	observations []dependencies.RuntimeTopologyObservation,
	retainAfter time.Time,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin runtime topology import: %w", err)
	}
	defer tx.Rollback()
	for _, observation := range observations {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO runtime_topology_observations (
    provider, environment, source_name, source_kind, target_name, target_kind,
    protocol, interaction, transport, observed_from, observed_to,
    request_count, error_count, latency_p95_ms, imported_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(
    provider, environment, source_name, target_name, protocol,
    interaction, transport, observed_from, observed_to
) DO UPDATE SET
    source_kind = excluded.source_kind,
    target_kind = excluded.target_kind,
    request_count = excluded.request_count,
    error_count = excluded.error_count,
    latency_p95_ms = excluded.latency_p95_ms,
    imported_at = excluded.imported_at`,
			observation.Provider,
			observation.Environment,
			observation.SourceName,
			observation.SourceKind,
			observation.TargetName,
			observation.TargetKind,
			observation.Protocol,
			observation.Interaction,
			observation.Transport,
			observation.ObservedFrom.UTC().Format(time.RFC3339Nano),
			observation.ObservedTo.UTC().Format(time.RFC3339Nano),
			observation.RequestCount,
			observation.ErrorCount,
			observation.LatencyP95MS,
			observation.ImportedAt.UTC().Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("upsert runtime topology observation: %w", err)
		}
	}
	if _, err := tx.ExecContext(
		ctx,
		"DELETE FROM runtime_topology_observations WHERE observed_to < ?",
		retainAfter.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("prune runtime topology observations: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit runtime topology import: %w", err)
	}
	return nil
}
