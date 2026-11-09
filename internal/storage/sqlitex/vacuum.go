package sqlitex

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Returning free space to the filesystem (W-dbspace).
//
// SQLite never shrinks a database file by itself: rows deleted by retention
// leave free pages that later inserts reuse, but the file keeps its peak
// size. On server (2026-10-28) hub.db was 5.2 GB of which 3.1 GB were free
// pages left by the W-dbhygiene command prune. With auto_vacuum=INCREMENTAL
// SQLite tracks where pages live, and PRAGMA incremental_vacuum(N) moves N
// pages from the end of the file into free slots and truncates it, while
// the database stays in use. Switching an existing database to that mode
// takes one full VACUUM, which rewrites the whole file and blocks every
// other connection, so PrepareReclaim does it only at startup and only when
// there is a lot to win; until then free pages are simply reused.

// SQLite's auto_vacuum modes.
const (
	AutoVacuumNone        = 0
	AutoVacuumFull        = 1
	AutoVacuumIncremental = 2
)

// SpaceStats describes a database file's pages.
type SpaceStats struct {
	AutoVacuum int
	PageSize   int64
	Pages      int64
	FreePages  int64
}

// FreeBytes is the space held by free pages.
func (s SpaceStats) FreeBytes() int64 { return s.FreePages * s.PageSize }

// LiveBytes is the space held by pages in use.
func (s SpaceStats) LiveBytes() int64 { return (s.Pages - s.FreePages) * s.PageSize }

// queryer is satisfied by *sql.DB and *sql.Conn.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Space reads db's page counts and auto_vacuum mode.
func Space(ctx context.Context, db queryer) (SpaceStats, error) {
	var s SpaceStats
	for _, p := range []struct {
		pragma string
		dst    any
	}{{"auto_vacuum", &s.AutoVacuum}, {"page_size", &s.PageSize}, {"page_count", &s.Pages}, {"freelist_count", &s.FreePages}} {
		if err := db.QueryRowContext(ctx, `PRAGMA `+p.pragma).Scan(p.dst); err != nil {
			return s, fmt.Errorf("sqlitex: pragma %s: %w", p.pragma, err)
		}
	}
	return s, nil
}

// ReclaimPolicy decides whether PrepareReclaim may VACUUM a database that
// is not yet in incremental mode.
type ReclaimPolicy struct {
	// A database whose live data is at most SmallBytes (a new or nearly
	// empty one) is converted at once; the VACUUM takes milliseconds.
	// Default 16 MiB.
	SmallBytes int64
	// A larger one is converted only when its free pages are at least
	// MinFreeFraction of the file (0.25) and MinFreeBytes (256 MiB).
	MinFreeFraction float64
	MinFreeBytes    int64
	// DiskAvail reports the bytes available to this process on the
	// filesystem holding dir. VACUUM writes a temporary copy of the live
	// data and then the whole database through the WAL, so a large
	// database is converted only when the database's directory has twice
	// its live data free (plus MinFreeBytes) and the temp directory once.
	// Nil: large databases are never converted.
	DiskAvail func(dir string) (uint64, error)
}

func (p *ReclaimPolicy) defaults() {
	if p.SmallBytes <= 0 {
		p.SmallBytes = 16 << 20
	}
	if p.MinFreeFraction <= 0 {
		p.MinFreeFraction = 0.25
	}
	if p.MinFreeBytes <= 0 {
		p.MinFreeBytes = 256 << 20
	}
}

// ReclaimReport says what PrepareReclaim found and did.
type ReclaimReport struct {
	Before   SpaceStats
	After    SpaceStats
	Vacuumed bool
	Took     time.Duration
	// Skipped explains why a database not in incremental mode was left
	// as it is ("" when it was converted or already incremental).
	Skipped string
}

// PrepareReclaim puts the database at path (open as db) in
// auto_vacuum=INCREMENTAL mode so ReclaimFree can shrink it later. Call it
// at startup, before the database serves anyone: converting an existing
// database runs a full VACUUM, which blocks all other connections for its
// duration (about a second per few hundred MB). A database already in
// incremental mode is left alone; a large one is converted only when
// policy allows it (much free space, enough disk), otherwise the next
// start tries again.
func PrepareReclaim(ctx context.Context, db *sql.DB, path string, policy ReclaimPolicy) (ReclaimReport, error) {
	policy.defaults()
	var rep ReclaimReport
	// The pending mode lives on the connection that set it, so the PRAGMA
	// and the VACUUM must run on the same one.
	conn, err := db.Conn(ctx)
	if err != nil {
		return rep, err
	}
	defer conn.Close()
	if rep.Before, err = Space(ctx, conn); err != nil {
		return rep, err
	}
	rep.After = rep.Before
	if rep.Before.AutoVacuum == AutoVacuumIncremental {
		return rep, nil
	}
	if reason := convertSkip(rep.Before, path, policy); reason != "" {
		rep.Skipped = reason
		return rep, nil
	}
	start := time.Now()
	if _, err := conn.ExecContext(ctx, `PRAGMA auto_vacuum=INCREMENTAL`); err != nil {
		return rep, fmt.Errorf("sqlitex: set auto_vacuum: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
		return rep, fmt.Errorf("sqlitex: vacuum: %w", err)
	}
	// VACUUM in WAL mode writes every page through the WAL; copy it back
	// and truncate it so the space shows up now.
	if _, err := conn.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return rep, fmt.Errorf("sqlitex: checkpoint after vacuum: %w", err)
	}
	rep.Vacuumed, rep.Took = true, time.Since(start)
	if rep.After, err = Space(ctx, conn); err != nil {
		return rep, err
	}
	return rep, nil
}

// convertSkip returns why a database in another auto_vacuum mode should
// not be VACUUMed now ("" to convert it).
func convertSkip(s SpaceStats, path string, p ReclaimPolicy) string {
	live := s.LiveBytes()
	if live <= p.SmallBytes {
		return ""
	}
	total := s.Pages * s.PageSize
	if s.FreeBytes() < p.MinFreeBytes || float64(s.FreeBytes()) < p.MinFreeFraction*float64(total) {
		return fmt.Sprintf("only %d MiB of %d MiB free", s.FreeBytes()>>20, total>>20)
	}
	if p.DiskAvail == nil {
		return "free disk space unknown"
	}
	need := map[string]int64{filepath.Dir(path): 2*live + p.MinFreeBytes}
	if tmp := os.TempDir(); tmp != "" {
		need[tmp] += live
	}
	for dir, n := range need {
		avail, err := p.DiskAvail(dir)
		if err != nil {
			return fmt.Sprintf("free disk space in %s unknown: %v", dir, err)
		}
		if avail < uint64(n) { //nolint:gosec // n is positive
			return fmt.Sprintf("VACUUM needs %d MiB free in %s, %d MiB available", n>>20, dir, avail>>20)
		}
	}
	return ""
}

// reclaimStepPages bounds one incremental_vacuum statement (and so one
// write transaction): 2048 pages is 8 MiB at the default page size.
const reclaimStepPages = 2048

// ReclaimFree returns free pages to the filesystem, keeping keepBytes of
// them for reuse (so a table that churns does not shrink and regrow the
// file every pass) and releasing at most maxBytes per call. It runs in
// short write transactions with a pause between them, like Batched, and
// does nothing unless the database is in incremental mode
// (PrepareReclaim). It returns the bytes released.
func ReclaimFree(ctx context.Context, db *sql.DB, keepBytes, maxBytes int64) (int64, error) {
	s, err := Space(ctx, db)
	if err != nil || s.AutoVacuum != AutoVacuumIncremental || s.PageSize <= 0 {
		return 0, err
	}
	want := min(s.FreePages-keepBytes/s.PageSize, maxBytes/s.PageSize)
	if want <= 0 {
		return 0, nil
	}
	released := func() int64 {
		after, err := Space(context.WithoutCancel(ctx), db)
		if err != nil {
			return 0
		}
		return max(s.Pages-after.Pages, 0) * s.PageSize
	}
	for done := int64(0); done < want; {
		if done > 0 {
			t := time.NewTimer(batchPause)
			select {
			case <-ctx.Done():
				t.Stop()
				return released(), ctx.Err()
			case <-t.C:
			}
		}
		n := min(want-done, reclaimStepPages)
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`PRAGMA incremental_vacuum(%d)`, n)); err != nil {
			return released(), fmt.Errorf("sqlitex: incremental_vacuum: %w", err)
		}
		done += n
	}
	return released(), nil
}
