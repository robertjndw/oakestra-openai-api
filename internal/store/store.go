package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

//go:embed migrations/*.sql
var migrations embed.FS

// FileName is the database file created inside the data directory.
const FileName = "ooa.db"

// busyTimeout bounds how long a statement waits for another writer. The
// deployment loop and admin requests may hold short write transactions at
// the same moment; this is not a substitute for keeping them short.
const busyTimeout = 10 * time.Second

// DB is the database handle. It embeds *sql.DB, so every database/sql
// method is available directly.
type DB struct {
	*sql.DB
	path string
}

// Open creates dir if needed, opens (or creates) dir/ooa.db and applies all
// pending migrations. The caller must Close the returned DB.
func Open(ctx context.Context, dir string) (*DB, error) {
	if dir == "" {
		return nil, errors.New("store: data dir is empty")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("store: create data dir: %w", err)
	}
	path := filepath.Join(dir, FileName)

	sqlDB, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}

	db := &DB{DB: sqlDB, path: path}
	if err := db.migrate(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// Path returns the database file path.
func (db *DB) Path() string { return db.path }

// dsn builds the modernc.org/sqlite connection string. Pragmas are set per
// connection by the driver, so every pooled connection gets them, not just
// the first one.
func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	q.Add("_pragma", "foreign_keys(1)")
	// NORMAL is durable across application crashes in WAL mode; only an OS
	// crash can lose the last transactions, and the deployment loop
	// re-adopts services by name after such a loss.
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	return "file:" + path + "?" + q.Encode()
}

func (db *DB) migrate(ctx context.Context) error {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return fmt.Errorf("store: migrations fs: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, db.DB, sub, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return fmt.Errorf("store: migration provider: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// Tx runs fn inside a transaction. The transaction commits if fn returns
// nil and rolls back otherwise, including when fn panics (the panic is
// re-raised after the rollback). Keep fn short and free of network calls:
// it holds SQLite's single write lock for its whole duration.
func (db *DB) Tx(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// Querier is satisfied by *sql.DB, *sql.Tx and *DB, so repository helpers
// can run either standalone or inside a transaction.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

var (
	_ Querier = (*sql.DB)(nil)
	_ Querier = (*sql.Tx)(nil)
	_ Querier = (*DB)(nil)
)
