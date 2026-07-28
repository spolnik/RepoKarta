package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

var postgresNoCaseExpression = regexp.MustCompile(
	`([A-Za-z_][A-Za-z0-9_.]*) COLLATE NOCASE`,
)

// Backend identifies the durable metadata database implementation.
type Backend string

const (
	BackendSQLite   Backend = "sqlite"
	BackendPostgres Backend = "postgres"
)

// Config selects a metadata database. SQLite remains the zero-configuration
// default; PostgreSQL accepts a normal PostgreSQL URL, including managed
// services such as Amazon RDS.
type Config struct {
	Backend       Backend
	SQLitePath    string
	PostgresURL   string
	DataDirectory string
}

// Store persists RepoKarta-owned metadata. Repository source and derived
// indexes remain filesystem-backed and read-only with respect to source.
type Store struct {
	db                    *database
	backend               Backend
	conversationDirectory string
}

type database struct {
	*sql.DB
	backend Backend
}

type transaction struct {
	*sql.Tx
	backend Backend
}

type databaseErrorCode interface {
	Code() int
}

type databaseSQLState interface {
	SQLState() string
}

func isUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	var state databaseSQLState
	if errors.As(err, &state) {
		return state.SQLState() == "23505"
	}
	var coded databaseErrorCode
	if errors.As(err, &coded) {
		code := coded.Code()
		return code == 2067 || code&0xff == 19
	}
	return false
}

// Open preserves the original zero-configuration SQLite API.
func Open(path string) (*Store, error) {
	return OpenConfig(Config{
		Backend:       BackendSQLite,
		SQLitePath:    path,
		DataDirectory: filepath.Dir(path),
	})
}

// OpenConfig opens and migrates the selected database without changing the
// Store methods used by the rest of RepoKarta.
func OpenConfig(config Config) (*Store, error) {
	backend := config.Backend
	if backend == "" {
		backend = BackendSQLite
	}
	dataDirectory := strings.TrimSpace(config.DataDirectory)
	if dataDirectory == "" && backend == BackendSQLite {
		dataDirectory = filepath.Dir(config.SQLitePath)
	}
	if dataDirectory == "" {
		return nil, errors.New("database data directory is required")
	}

	var (
		driver string
		dsn    string
	)
	switch backend {
	case BackendSQLite:
		driver = "sqlite"
		dsn = strings.TrimSpace(config.SQLitePath)
		if dsn == "" {
			dsn = filepath.Join(dataDirectory, "repokarta.db")
		}
	case BackendPostgres:
		driver = "pgx"
		dsn = strings.TrimSpace(config.PostgresURL)
		if dsn == "" {
			return nil, errors.New("PostgreSQL database URL is required")
		}
	default:
		return nil, fmt.Errorf("unsupported database backend %q", backend)
	}

	raw, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s database: %w", backend, err)
	}
	db := &database{DB: raw, backend: backend}
	if backend == BackendSQLite {
		raw.SetMaxOpenConns(1)
		if _, err := raw.Exec("PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000;"); err != nil {
			raw.Close()
			return nil, fmt.Errorf("configure SQLite database: %w", err)
		}
	} else {
		raw.SetMaxOpenConns(10)
		raw.SetMaxIdleConns(5)
		if err := raw.Ping(); err != nil {
			raw.Close()
			return nil, fmt.Errorf("connect to PostgreSQL database: %w", err)
		}
	}
	if err := migrate(db); err != nil {
		raw.Close()
		return nil, err
	}

	conversationDirectory := filepath.Join(dataDirectory, "conversations")
	if err := os.MkdirAll(conversationDirectory, 0o700); err != nil {
		raw.Close()
		return nil, fmt.Errorf("create conversation storage: %w", err)
	}
	return &Store{
		db:                    db,
		backend:               backend,
		conversationDirectory: conversationDirectory,
	}, nil
}

// Backend reports which durable metadata backend is in use.
func (s *Store) Backend() Backend {
	return s.backend
}

func (db *database) Exec(query string, args ...any) (sql.Result, error) {
	return db.DB.Exec(rebind(query, db.backend), databaseArguments(args, db.backend)...)
}

func (db *database) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return db.DB.ExecContext(ctx, rebind(query, db.backend), databaseArguments(args, db.backend)...)
}

func (db *database) Query(query string, args ...any) (*sql.Rows, error) {
	return db.DB.Query(rebind(query, db.backend), databaseArguments(args, db.backend)...)
}

func (db *database) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return db.DB.QueryContext(ctx, rebind(query, db.backend), databaseArguments(args, db.backend)...)
}

func (db *database) QueryRow(query string, args ...any) *sql.Row {
	return db.DB.QueryRow(rebind(query, db.backend), databaseArguments(args, db.backend)...)
}

func (db *database) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return db.DB.QueryRowContext(ctx, rebind(query, db.backend), databaseArguments(args, db.backend)...)
}

func (db *database) Begin() (*transaction, error) {
	tx, err := db.DB.Begin()
	if err != nil {
		return nil, err
	}
	return &transaction{Tx: tx, backend: db.backend}, nil
}

func (db *database) BeginTx(ctx context.Context, opts *sql.TxOptions) (*transaction, error) {
	tx, err := db.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &transaction{Tx: tx, backend: db.backend}, nil
}

func (tx *transaction) Exec(query string, args ...any) (sql.Result, error) {
	return tx.Tx.Exec(rebind(query, tx.backend), databaseArguments(args, tx.backend)...)
}

func (tx *transaction) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return tx.Tx.ExecContext(ctx, rebind(query, tx.backend), databaseArguments(args, tx.backend)...)
}

func (tx *transaction) Query(query string, args ...any) (*sql.Rows, error) {
	return tx.Tx.Query(rebind(query, tx.backend), databaseArguments(args, tx.backend)...)
}

func (tx *transaction) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return tx.Tx.QueryContext(ctx, rebind(query, tx.backend), databaseArguments(args, tx.backend)...)
}

func (tx *transaction) QueryRow(query string, args ...any) *sql.Row {
	return tx.Tx.QueryRow(rebind(query, tx.backend), databaseArguments(args, tx.backend)...)
}

func (tx *transaction) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return tx.Tx.QueryRowContext(ctx, rebind(query, tx.backend), databaseArguments(args, tx.backend)...)
}

func databaseArguments(arguments []any, backend Backend) []any {
	if backend != BackendPostgres {
		return arguments
	}
	converted := make([]any, len(arguments))
	for index, argument := range arguments {
		if value, ok := argument.(bool); ok {
			if value {
				converted[index] = int64(1)
			} else {
				converted[index] = int64(0)
			}
			continue
		}
		converted[index] = argument
	}
	return converted
}

// rebind converts SQLite-style positional parameters and case-insensitive
// ordering into PostgreSQL syntax. It deliberately skips quoted strings and
// comments so literal question marks are retained.
func rebind(query string, backend Backend) string {
	if backend != BackendPostgres {
		return query
	}
	query = postgresNoCaseExpression.ReplaceAllString(query, "lower($1)")
	query = strings.ReplaceAll(
		query,
		"ON CONFLICT(provider, group_value)",
		"ON CONFLICT(provider, lower(group_value))",
	)
	var result strings.Builder
	result.Grow(len(query) + 16)
	parameter := 1
	inSingle, inDouble, inLineComment, inBlockComment := false, false, false, false
	for index := 0; index < len(query); index++ {
		current := query[index]
		next := byte(0)
		if index+1 < len(query) {
			next = query[index+1]
		}
		switch {
		case inLineComment:
			result.WriteByte(current)
			if current == '\n' {
				inLineComment = false
			}
		case inBlockComment:
			result.WriteByte(current)
			if current == '*' && next == '/' {
				result.WriteByte(next)
				index++
				inBlockComment = false
			}
		case inSingle:
			result.WriteByte(current)
			if current == '\'' {
				if next == '\'' {
					result.WriteByte(next)
					index++
				} else {
					inSingle = false
				}
			}
		case inDouble:
			result.WriteByte(current)
			if current == '"' {
				if next == '"' {
					result.WriteByte(next)
					index++
				} else {
					inDouble = false
				}
			}
		case current == '-' && next == '-':
			result.WriteString("--")
			index++
			inLineComment = true
		case current == '/' && next == '*':
			result.WriteString("/*")
			index++
			inBlockComment = true
		case current == '\'':
			result.WriteByte(current)
			inSingle = true
		case current == '"':
			result.WriteByte(current)
			inDouble = true
		case current == '?':
			result.WriteByte('$')
			result.WriteString(strconv.Itoa(parameter))
			parameter++
		default:
			result.WriteByte(current)
		}
	}
	return result.String()
}
