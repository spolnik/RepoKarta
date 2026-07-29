package store

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	postgresIdentityColumn = regexp.MustCompile(`(?m)^(\s*)id INTEGER PRIMARY KEY(?: AUTOINCREMENT)?`)
	postgresIntegerType    = regexp.MustCompile(`\bINTEGER\b`)
	postgresRealType       = regexp.MustCompile(`\bREAL\b`)
)

func migration(version int, backend Backend) (string, error) {
	var statement string
	switch version {
	case 1:
		statement = schemaV1
	case 2:
		statement = schemaV2
	case 3:
		statement = schemaV3
	case 4:
		statement = schemaV4
	case 5:
		statement = schemaV5
	case 6:
		statement = schemaV6
	case 7:
		statement = schemaV7
	case 8:
		statement = schemaV8
	case 9:
		statement = schemaV9
	case 10:
		statement = schemaV10
	case 11:
		statement = schemaV11
	case 12:
		statement = schemaV12
	case 13:
		statement = schemaV13
	case 14:
		statement = schemaV14
	case 15:
		statement = schemaV15
	case 16:
		statement = schemaV16
	case 17:
		statement = schemaV17
	case 18:
		statement = schemaV18
	case 19:
		statement = schemaV19
	case 20:
		statement = schemaV20
	case 21:
		statement = schemaV21
	case 22:
		statement = schemaV22
	case 23:
		statement = schemaV23
	case 24:
		statement = schemaV24
	default:
		return "", fmt.Errorf("missing migration for schema version %d", version)
	}
	if backend == BackendPostgres {
		statement = postgresMigration(version, statement)
	}
	return statement, nil
}

func migrate(db *database) error {
	if db.backend == BackendPostgres {
		return migratePostgres(db)
	}
	return migrateSQLite(db)
}

func migrateSQLite(db *database) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read SQLite schema version: %w", err)
	}
	if err := validateSchemaVersion(version); err != nil {
		return err
	}
	for version < currentSchemaVersion {
		next := version + 1
		statement, err := migration(next, BackendSQLite)
		if err != nil {
			return err
		}
		if next == 21 {
			var repositoryTableCount int
			if err := db.QueryRow(`
SELECT COUNT(*) FROM sqlite_master
WHERE type = 'table' AND name = 'repositories'`).Scan(&repositoryTableCount); err != nil {
				return fmt.Errorf("inspect repository table before migration 21: %w", err)
			}
			if repositoryTableCount == 0 {
				// Some early test/development databases recorded a schema
				// version without creating the catalogue. Keep those sparse
				// databases openable; fresh catalogues still receive V21.
				statement = "SELECT 1;"
			}
		}
		if next == 23 {
			var conversationTableCount int
			if err := db.QueryRow(`
SELECT COUNT(*) FROM sqlite_master
WHERE type = 'table' AND name = 'conversations'`).Scan(&conversationTableCount); err != nil {
				return fmt.Errorf("inspect conversation table before migration 23: %w", err)
			}
			if conversationTableCount == 0 {
				// Preserve support for sparse historical test/development
				// databases that recorded a later version without chat tables.
				statement = "SELECT 1;"
			}
		}
		if next == 24 {
			var repositoryTableCount int
			if err := db.QueryRow(`
SELECT COUNT(*) FROM sqlite_master
WHERE type = 'table' AND name = 'repositories'`).Scan(&repositoryTableCount); err != nil {
				return fmt.Errorf("inspect repository table before migration 24: %w", err)
			}
			if repositoryTableCount == 0 {
				statement = "SELECT 1;"
			}
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin SQLite schema migration %d: %w", next, err)
		}
		if _, err := tx.Exec(statement); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply SQLite schema migration %d: %w", next, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", next)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record SQLite schema migration %d: %w", next, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit SQLite schema migration %d: %w", next, err)
		}
		version = next
	}
	return nil
}

func migratePostgres(db *database) error {
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS repokarta_schema_migrations (
    version BIGINT PRIMARY KEY,
    applied_at TEXT NOT NULL
)`); err != nil {
		return fmt.Errorf("create PostgreSQL migration ledger: %w", err)
	}
	var version int
	if err := db.QueryRow(
		"SELECT COALESCE(MAX(version), 0) FROM repokarta_schema_migrations",
	).Scan(&version); err != nil {
		return fmt.Errorf("read PostgreSQL schema version: %w", err)
	}
	if err := validateSchemaVersion(version); err != nil {
		return err
	}
	for version < currentSchemaVersion {
		next := version + 1
		statement, err := migration(next, BackendPostgres)
		if err != nil {
			return err
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin PostgreSQL schema migration %d: %w", next, err)
		}
		// Serialize concurrent first starts without requiring a global database
		// lock or elevated managed-service permissions.
		if _, err := tx.Exec("SELECT pg_advisory_xact_lock(1380993876)"); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("lock PostgreSQL schema migrations: %w", err)
		}
		var alreadyApplied int
		if err := tx.QueryRow(
			"SELECT COUNT(*) FROM repokarta_schema_migrations WHERE version = ?",
			next,
		).Scan(&alreadyApplied); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("recheck PostgreSQL schema migration %d: %w", next, err)
		}
		if alreadyApplied == 0 {
			if _, err := tx.Exec(statement); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("apply PostgreSQL schema migration %d: %w", next, err)
			}
			if _, err := tx.Exec(
				"INSERT INTO repokarta_schema_migrations(version, applied_at) VALUES (?, ?)",
				next,
				time.Now().UTC().Format(time.RFC3339Nano),
			); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("record PostgreSQL schema migration %d: %w", next, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit PostgreSQL schema migration %d: %w", next, err)
		}
		version = next
	}
	return nil
}

func validateSchemaVersion(version int) error {
	if version > currentSchemaVersion {
		return fmt.Errorf(
			"database schema version %d is newer than supported version %d",
			version,
			currentSchemaVersion,
		)
	}
	return nil
}

func postgresMigration(version int, statement string) string {
	if version == 12 {
		statement = strings.ReplaceAll(
			statement,
			"UNIQUE(provider, group_value COLLATE NOCASE)",
			"UNIQUE(provider, group_value)",
		)
		statement += `
CREATE UNIQUE INDEX identity_role_mappings_provider_group_nocase
ON identity_role_mappings(provider, lower(group_value));`
	}
	if version == 15 {
		statement = strings.ReplaceAll(
			statement,
			"canonical_id TEXT NOT NULL UNIQUE COLLATE NOCASE",
			"canonical_id TEXT NOT NULL UNIQUE",
		)
		statement += `
CREATE UNIQUE INDEX repository_acquisitions_canonical_id_nocase
ON repository_acquisitions(lower(canonical_id));`
	}
	statement = postgresNoCaseExpression.ReplaceAllString(statement, "lower($1)")
	statement = postgresIdentityColumn.ReplaceAllString(
		statement,
		"${1}id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY",
	)
	statement = postgresIntegerType.ReplaceAllString(statement, "BIGINT")
	statement = postgresRealType.ReplaceAllString(statement, "DOUBLE PRECISION")
	if version == 9 {
		statement = strings.Replace(statement,
			"INSERT OR IGNORE INTO repository_access (repository_id, owner_id, visibility, updated_at)\nSELECT id, 'local:admin', 'private', '' FROM repositories;",
			"INSERT INTO repository_access (repository_id, owner_id, visibility, updated_at)\nSELECT id, 'local:admin', 'private', '' FROM repositories\nON CONFLICT DO NOTHING;",
			1,
		)
		triggerStart := strings.Index(statement, "CREATE TRIGGER IF NOT EXISTS repositories_access_insert")
		if triggerStart >= 0 {
			statement = statement[:triggerStart] + `
CREATE OR REPLACE FUNCTION repokarta_repository_access_insert()
RETURNS trigger
LANGUAGE plpgsql
AS $repokarta$
BEGIN
    INSERT INTO repository_access (
        repository_id, owner_id, visibility, updated_at
    ) VALUES (NEW.id, 'local:admin', 'private', '')
    ON CONFLICT DO NOTHING;
    RETURN NEW;
END;
$repokarta$;
DROP TRIGGER IF EXISTS repositories_access_insert ON repositories;
CREATE TRIGGER repositories_access_insert
AFTER INSERT ON repositories
FOR EACH ROW EXECUTE FUNCTION repokarta_repository_access_insert();`
		}
	}
	return statement
}
