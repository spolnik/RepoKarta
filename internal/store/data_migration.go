package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// MigrationReport describes an atomic SQLite-to-PostgreSQL copy.
type MigrationReport struct {
	Tables int
	Rows   int64
}

var migrationTables = []string{
	"app_settings",
	"repository_acquisitions",
	"repositories",
	"repository_access",
	"repository_access_grants",
	"conversations",
	"conversation_messages",
	"conversation_message_images",
	"conversation_message_citations",
	"conversation_shares",
	"audit_events",
	"identities",
	"identity_groups",
	"identity_group_members",
	"identity_role_mappings",
	"insight_runs",
	"insight_observations",
	"insight_thresholds",
	"sonar_connections",
	"repository_acquisition_events",
	"dependency_registry_observations",
	"named_contexts",
	"runtime_topology_observations",
	"repository_scip_indexes",
	"search_history",
	"saved_searches",
	"search_monitors",
	"search_monitor_runs",
	"code_sessions",
	"code_actions",
	"code_approvals",
}

var identityTables = []string{
	"repositories",
	"conversation_messages",
	"conversation_message_images",
	"conversation_message_citations",
	"audit_events",
	"identity_role_mappings",
	"insight_observations",
	"insight_thresholds",
	"sonar_connections",
	"repository_acquisitions",
	"repository_acquisition_events",
	"runtime_topology_observations",
	"search_history",
	"search_monitor_runs",
	"code_actions",
}

// MigrateSQLiteToPostgres upgrades the SQLite source, requires an empty
// PostgreSQL destination, copies every durable table in one destination
// transaction, resets identity sequences, and verifies row counts before
// commit. Filesystem artifacts stay in the configured data directory.
func MigrateSQLiteToPostgres(
	ctx context.Context,
	sqlitePath string,
	postgresURL string,
	dataDirectory string,
) (MigrationReport, error) {
	source, err := Open(sqlitePath)
	if err != nil {
		return MigrationReport{}, fmt.Errorf("open SQLite migration source: %w", err)
	}
	defer source.Close()
	destination, err := OpenConfig(Config{
		Backend:       BackendPostgres,
		PostgresURL:   postgresURL,
		DataDirectory: dataDirectory,
	})
	if err != nil {
		return MigrationReport{}, fmt.Errorf("open PostgreSQL migration destination: %w", err)
	}
	defer destination.Close()
	return migrateData(ctx, source.db, destination.db)
}

func migrateData(ctx context.Context, source, destination *database) (MigrationReport, error) {
	sourceTx, err := source.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return MigrationReport{}, fmt.Errorf("begin SQLite migration snapshot: %w", err)
	}
	defer sourceTx.Rollback()
	var sourceSchemaRows int
	if err := sourceTx.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM sqlite_master",
	).Scan(&sourceSchemaRows); err != nil {
		return MigrationReport{}, fmt.Errorf("establish SQLite migration snapshot: %w", err)
	}

	tx, err := destination.BeginTx(ctx, nil)
	if err != nil {
		return MigrationReport{}, fmt.Errorf("begin PostgreSQL data migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(1380993877)"); err != nil {
		return MigrationReport{}, fmt.Errorf("lock PostgreSQL data migration: %w", err)
	}
	for _, table := range migrationTables {
		var count int64
		if err := tx.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM "+table,
		).Scan(&count); err != nil {
			return MigrationReport{}, fmt.Errorf("inspect PostgreSQL table %s: %w", table, err)
		}
		if count != 0 {
			return MigrationReport{}, fmt.Errorf(
				"PostgreSQL destination is not empty: table %s contains %d rows",
				table,
				count,
			)
		}
	}

	var report MigrationReport
	for _, table := range migrationTables {
		// The repository insert trigger creates default access rows. Remove
		// those defaults immediately before copying the authoritative policies.
		if table == "repository_access" {
			if _, err := tx.ExecContext(ctx, "DELETE FROM repository_access"); err != nil {
				return MigrationReport{}, fmt.Errorf("prepare repository access migration: %w", err)
			}
		}
		count, err := copyTable(ctx, sourceTx, tx, table)
		if err != nil {
			return MigrationReport{}, err
		}
		report.Tables++
		report.Rows += count
	}
	for _, table := range identityTables {
		if _, err := tx.ExecContext(ctx, `
SELECT setval(
    pg_get_serial_sequence(?, 'id'),
    COALESCE((SELECT MAX(id) FROM `+table+`), 1),
    (SELECT COUNT(*) > 0 FROM `+table+`)
)`, table); err != nil {
			return MigrationReport{}, fmt.Errorf("reset PostgreSQL identity for %s: %w", table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return MigrationReport{}, fmt.Errorf("commit PostgreSQL data migration: %w", err)
	}
	return report, nil
}

func copyTable(
	ctx context.Context,
	source interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	},
	destination *transaction,
	table string,
) (int64, error) {
	rows, err := source.QueryContext(ctx, "SELECT * FROM "+table)
	if err != nil {
		return 0, fmt.Errorf("read SQLite table %s: %w", table, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return 0, fmt.Errorf("read SQLite columns for %s: %w", table, err)
	}
	quotedColumns := make([]string, len(columns))
	placeholders := make([]string, len(columns))
	for index, column := range columns {
		quotedColumns[index] = `"` + strings.ReplaceAll(column, `"`, `""`) + `"`
		placeholders[index] = "?"
	}
	insert := "INSERT INTO " + table + " (" + strings.Join(quotedColumns, ", ") +
		") VALUES (" + strings.Join(placeholders, ", ") + ")"
	var count int64
	for rows.Next() {
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for index := range values {
			destinations[index] = &values[index]
		}
		if err := rows.Scan(destinations...); err != nil {
			return 0, fmt.Errorf("scan SQLite row from %s: %w", table, err)
		}
		if _, err := destination.ExecContext(ctx, insert, values...); err != nil {
			return 0, fmt.Errorf("copy SQLite row into PostgreSQL table %s: %w", table, err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate SQLite table %s: %w", table, err)
	}
	var destinationCount int64
	if err := destination.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM "+table,
	).Scan(&destinationCount); err != nil {
		return 0, fmt.Errorf("verify PostgreSQL table %s: %w", table, err)
	}
	if destinationCount != count {
		return 0, errors.New("PostgreSQL row-count verification failed for " + table)
	}
	return count, nil
}
