# PostgreSQL backend and SQLite migration

RepoKarta uses SQLite by default. PostgreSQL 18 or newer is an optional
metadata backend intended for shared deployments and managed PostgreSQL
services such as Amazon RDS. The catalogue, access policy, conversations,
audit evidence, identities, and other relational state use the same
application Store API on both backends.

Zoekt indexes, generated Wiki pages, maps, SCIP artifacts, logs, SAML key
material, and conversation attachments remain in `-data-dir`. PostgreSQL does
not replace or relocate those files.

## Start directly on PostgreSQL

Create a dedicated empty database and a role that owns it. RepoKarta needs
normal connection, table, index, trigger, sequence, and schema-migration DDL
permissions within that database. It does not need a server-wide administrator
role or permission to create databases.

Put one standard PostgreSQL URL in a permission-restricted file:

```text
postgresql://repokarta:password@database.example.com:5432/repokarta?sslmode=verify-full
```

Then set:

```text
REPOKARTA_DATABASE_URL_FILE=/etc/repokarta/postgres-url
```

On Windows PowerShell:

```powershell
$env:REPOKARTA_DATABASE_URL_FILE = 'C:\ProgramData\RepoKarta\postgres-url'
.\repokarta.exe serve -data-dir 'C:\ProgramData\RepoKarta' C:\Repositories
```

`REPOKARTA_DATABASE_URL` may be used instead when the process environment is
already a protected secret boundary. The URL-file setting takes precedence.
For a remote service, configure certificate verification in the URL and keep
the CA material readable by the RepoKarta service account.

RepoKarta creates a `repokarta_schema_migrations` ledger and applies
PostgreSQL-specialized migrations to the same `store.SchemaVersion` used by
SQLite. PostgreSQL startup uses a transaction-scoped advisory lock, so
concurrent starts cannot apply the same migration concurrently. Migrations are
forward-only.

## Migrate an existing SQLite instance

Plan a maintenance window. The command intentionally does not perform a live
dual write.

1. Stop every RepoKarta process using the SQLite database.
2. Back up the complete RepoKarta data directory.
3. Create a dedicated, empty PostgreSQL database.
4. Put its URL in a mode-0600 or otherwise permission-restricted file.
5. Run the migration with the same data directory:

```powershell
.\repokarta.exe database migrate-sqlite-to-postgres `
  -data-dir 'C:\ProgramData\RepoKarta' `
  -postgres-url-file 'C:\ProgramData\RepoKarta\postgres-url'
```

Linux or macOS:

```sh
repokarta database migrate-sqlite-to-postgres \
  -data-dir /var/lib/repokarta \
  -postgres-url-file /etc/repokarta/postgres-url
```

Use `-sqlite-path` only when the source is not
`<data-dir>/repokarta.db`.

The command:

- upgrades the SQLite source to the current schema;
- applies the matching PostgreSQL schema;
- reads all source tables from one consistent SQLite transaction snapshot;
- refuses a destination containing any RepoKarta application rows;
- copies all durable tables inside one PostgreSQL transaction;
- preserves primary and foreign-key IDs;
- resets PostgreSQL identity sequences;
- checks the destination row count after every table; and
- rolls back the entire PostgreSQL copy on an error or interruption.

The SQLite file is never deleted or modified beyond normal forward schema
migrations. Filesystem artifacts are not copied because the command continues
to use the same data directory.

After success, configure `REPOKARTA_DATABASE_URL_FILE` for normal starts and
run the shared-deployment smoke checks. Keep the SQLite backup until the
PostgreSQL database and data-directory backup policy has been verified.

## Operational boundaries

- Do not run SQLite and PostgreSQL instances concurrently against the same
  data directory.
- There is no automatic PostgreSQL-to-SQLite downgrade or continuous
  replication.
- A destination with existing application rows is rejected even when those
  rows appear compatible.
- Database migrations and the data migration are forward-only. Restore the
  pre-migration backup to roll back.
- Back up PostgreSQL and the data directory at one maintenance point; neither
  backup alone is a complete RepoKarta instance.
