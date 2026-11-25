// Package store owns the SQLite database: opening it with the right
// pragmas, applying the embedded goose migrations, and a few generic helpers
// (transactions, settings, ids, time conversion).
//
// It deliberately has no per-table query code. The packages that own a
// table (deploy for models and deployments, auth for api_keys) write their
// own small repositories against the *DB handle from here. The schema and
// its conventions are documented in migrations/00001_init.sql.
//
// The driver is modernc.org/sqlite, so binaries build with CGO_ENABLED=0.
// The database runs in WAL mode with a busy timeout, foreign keys on, and
// BEGIN IMMEDIATE for transactions: SQLite allows one writer at a time, and
// taking the write lock up front turns would-be deadlocks between two
// read-then-write transactions into a short wait instead of SQLITE_BUSY.
// The deployment port allocator relies on that: it reads the used ports and
// inserts the new row in one transaction.
package store
