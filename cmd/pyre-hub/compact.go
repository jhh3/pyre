//go:build !windows

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/jhh3/pyre/internal/hub"
	"github.com/jhh3/pyre/internal/hub/assemble"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// LockFile in the data directory is held (flock) by a serving hub and by
// compact, so compact refuses to run against a live hub and two hubs never
// serve one data directory.
const LockFile = "hub.lock"

// errLocked: another process holds the data directory lock.
var errLocked = errors.New("the hub data directory is in use (a pyre-hub serve or compact holds " + LockFile + "); stop the hub first")

// lockDataDir takes the data directory lock without waiting. The returned
// function releases it.
func lockDataDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, LockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// cmdCompact runs the retention pass to completion and VACUUMs hub.db, for
// a one-time shrink of a database that grew before retention was bounded
// (W-dbhygiene). Offline only.
func cmdCompact(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("compact", stderr)
	data := fs.String("data", "", "hub data directory (the hub must be stopped)")
	dryRun := fs.Bool("dry-run", false, "only report table sizes by kind; change nothing")
	noVacuum := fs.Bool("no-vacuum", false, "run retention but skip VACUUM (the file keeps its size; freed pages are reused)")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if *data == "" || fs.NArg() > 0 {
		return usageError{"--data is required"}
	}
	path := filepath.Join(*data, assemble.DBFile)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no hub database at %s: %w", path, err)
	}
	unlock, err := lockDataDir(*data)
	if err != nil {
		return err
	}
	defer unlock()
	db, err := sqlitex.Open(path)
	if err != nil {
		return err
	}
	defer db.Close()
	before := dbFileSize(path)
	fmt.Fprintf(stdout, "%s: %s\n", path, human(before))
	if err := reportSizes(ctx, db, stdout); err != nil {
		return err
	}
	if *dryRun {
		return nil
	}
	h, err := hub.New(hub.Config{DB: db, DataDir: *data})
	if err != nil {
		return err
	}
	defer h.Close()
	var total hub.RetentionStats
	for pass := 0; pass < 1000; pass++ {
		st, err := h.RunRetention(ctx)
		if err != nil {
			return fmt.Errorf("retention: %w", err)
		}
		total.CommandsDeleted += st.CommandsDeleted
		total.CommandsCompacted += st.CommandsCompacted
		total.OperationsDeleted += st.OperationsDeleted
		total.EventsDeleted += st.EventsDeleted
		total.AuditDeleted += st.AuditDeleted
		total.ReportDedup += st.ReportDedup
		// A pass is bounded (sqlitex.DefaultMaxBatches transactions per
		// table); repeat while any table hit that bound.
		capped := func(n int64) bool { return n >= sqlitex.PruneBatch*sqlitex.DefaultMaxBatches }
		if !capped(st.CommandsDeleted) && !capped(st.CommandsCompacted) && !capped(st.OperationsDeleted) &&
			!capped(st.EventsDeleted) && !capped(st.AuditDeleted) && !capped(st.ReportDedup) {
			break
		}
	}
	fmt.Fprintf(stdout, "retention: %d finished commands deleted, %d compacted; %d events, %d operation records, %d audit entries, %d report dedup rows deleted\n",
		total.CommandsDeleted, total.CommandsCompacted, total.EventsDeleted, total.OperationsDeleted, total.AuditDeleted, total.ReportDedup)
	if *noVacuum {
		return nil
	}
	fmt.Fprintln(stdout, "vacuuming (needs free disk space about the size of the remaining data)...")
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	after := dbFileSize(path)
	fmt.Fprintf(stdout, "%s: %s -> %s\n", path, human(before), human(after))
	return nil
}

// reportSizes prints the bytes held by events and commands by kind (a full
// scan; offline only).
func reportSizes(ctx context.Context, db *sql.DB, w io.Writer) error {
	type q struct{ title, sql string }
	for _, x := range []q{
		{"events by kind", `SELECT kind, COUNT(*), COALESCE(SUM(length(CAST(payload AS BLOB))),0) FROM events GROUP BY kind ORDER BY 3 DESC`},
		{"commands by kind/state", `SELECT kind || '/' || state, COUNT(*), COALESCE(SUM(length(CAST(payload AS BLOB)) + COALESCE(length(CAST(result AS BLOB)),0)),0) FROM commands GROUP BY kind, state ORDER BY 3 DESC`},
		{"operations", `SELECT op, COUNT(*), COALESCE(SUM(COALESCE(length(response),0) + COALESCE(length(error),0)),0) FROM operations GROUP BY op ORDER BY 3 DESC LIMIT 10`},
	} {
		rows, err := db.QueryContext(ctx, x.sql)
		if err != nil {
			return fmt.Errorf("%s: %w", x.title, err)
		}
		fmt.Fprintf(w, "%s:\n", x.title)
		for rows.Next() {
			var k string
			var n, b int64
			if err := rows.Scan(&k, &n, &b); err != nil {
				rows.Close()
				return err
			}
			fmt.Fprintf(w, "  %-40s %8d rows %10s\n", k, n, human(b))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

func dbFileSize(path string) int64 {
	var n int64
	for _, p := range []string{path, path + "-wal"} {
		if fi, err := os.Stat(p); err == nil {
			n += fi.Size()
		}
	}
	return n
}

func human(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}
