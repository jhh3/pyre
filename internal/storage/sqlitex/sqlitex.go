// Package sqlitex opens SQLite databases with Pyre's standard pragmas and
// runs namespaced, append-only migrations. Each domain package (hub core,
// projects, attention, node outbox, ...) owns its own migration namespace so
// independent packages never edit a shared migration registry.
package sqlitex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver; cross-compiles without cgo
)

// maxConns bounds each file database's connection pool.
const maxConns = 8

// Open opens (creating if needed) a SQLite database at path with WAL,
// foreign keys, and a busy timeout. Use ":memory:" only for tests (a single
// connection is enforced so the in-memory DB is shared).
func Open(path string) (*sql.DB, error) {
	// synchronous(FULL): every acknowledged commit survives power loss (in
	// WAL mode NORMAL may lose the last commits), which the durable
	// stored/acknowledged semantics require. On macOS fsync does not flush
	// the drive cache; fullfsync uses F_FULLFSYNC.
	q := "_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=synchronous(FULL)&_txlock=immediate"
	if runtime.GOOS == "darwin" {
		q += "&_pragma=fullfsync(1)&_pragma=checkpoint_fullfsync(1)"
	}
	var dsn string
	switch {
	case path == ":memory:":
		dsn = "file::memory:?cache=shared&" + q
	case strings.Contains(path, "?"):
		dsn = "file:" + path + "&" + q
	default:
		dsn = "file:" + path + "?" + q
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	} else {
		// SQLite serializes writers; a small pool keeps readers concurrent.
		// Keep every pooled connection idle instead of database/sql's
		// default of 2: each new SQLite connection re-runs the pragmas and
		// re-parses the whole schema on first use. With long-polls and node
		// reports in parallel the hub opened connections continuously; in
		// the A20 soak that was 30% of its CPU. Idle ones close after a
		// while so a quiet daemon does not hold page caches forever.
		db.SetMaxOpenConns(maxConns)
		db.SetMaxIdleConns(maxConns)
		db.SetConnMaxIdleTime(5 * time.Minute)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Migration is one append-only schema step in a namespace. Never edit or
// reorder a released migration; add a new one.
type Migration struct {
	Namespace string // e.g. "hub", "projects", "attention", "node"
	Version   int    // 1, 2, 3 ... within the namespace
	Name      string
	SQL       string // may contain multiple statements
}

// Migrate applies pending migrations transactionally, in (namespace order
// given, version) order. It refuses to run if the database records a newer
// version in a namespace than this binary knows (downgrade protection).
func Migrate(ctx context.Context, db *sql.DB, migrations ...Migration) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		namespace TEXT NOT NULL,
		version INTEGER NOT NULL,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL,
		PRIMARY KEY (namespace, version))`); err != nil {
		return fmt.Errorf("sqlitex: create schema_migrations: %w", err)
	}
	known := map[string]int{}
	for _, m := range migrations {
		if m.Namespace == "" || m.Version <= 0 {
			return fmt.Errorf("sqlitex: invalid migration %q/%d", m.Namespace, m.Version)
		}
		if m.Version > known[m.Namespace] {
			known[m.Namespace] = m.Version
		}
	}
	for ns, max := range known {
		var cur sql.NullInt64
		if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations WHERE namespace=?`, ns).Scan(&cur); err != nil {
			return err
		}
		if cur.Valid && int(cur.Int64) > max {
			return fmt.Errorf("sqlitex: database namespace %q is at version %d but this binary only knows %d (refusing to run an older binary against a newer schema)", ns, cur.Int64, max)
		}
	}
	for _, m := range migrations {
		var exists int
		err := db.QueryRowContext(ctx, `SELECT 1 FROM schema_migrations WHERE namespace=? AND version=?`, m.Namespace, m.Version).Scan(&exists)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			tx.Rollback()
			return fmt.Errorf("sqlitex: migration %s/%d %s: %w", m.Namespace, m.Version, m.Name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(namespace,version,name,applied_at) VALUES(?,?,?,?)`,
			m.Namespace, m.Version, m.Name, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Tx runs fn in a transaction, committing on nil error. If fn's error is
// (or wraps) a *StoredRejection from Idempotent, the transaction is
// committed so the rejection persists for replay, and the rejection's
// *contracts.Error is returned. Any other error rolls back.
func Tx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		var sr *StoredRejection
		if errors.As(err, &sr) {
			if cerr := tx.Commit(); cerr != nil {
				return cerr
			}
			return sr.Err
		}
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Backup writes a transactionally consistent copy of db to dest using
// VACUUM INTO (safe on a live database).
func Backup(ctx context.Context, db *sql.DB, dest string) error {
	_, err := db.ExecContext(ctx, `VACUUM INTO ?`, dest)
	return err
}

// TimeFormat is the canonical stored time format (UTC, sortable).
const TimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// FormatTime formats t for storage.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeFormat) }

// ParseTime parses a stored time ("" -> zero).
func ParseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(TimeFormat, s)
}
