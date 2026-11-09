package sqlitex

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// fillAndDelete writes about mb MiB of rows and deletes them again, leaving
// their pages free.
func fillAndDelete(t *testing.T, db *sql.DB, mb int) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS blob(id INTEGER PRIMARY KEY, v BLOB)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < ?)
		INSERT INTO blob(v) SELECT randomblob(4000) FROM n`, mb*256); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM blob`); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareReclaimNewDatabaseConvertsAtOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "n.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(ctx, db, Migration{Namespace: "a", Version: 1, Name: "init", SQL: `CREATE TABLE x(id TEXT PRIMARY KEY)`}); err != nil {
		t.Fatal(err)
	}
	rep, err := PrepareReclaim(ctx, db, path, ReclaimPolicy{}) // no DiskAvail: only small databases convert
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Vacuumed || rep.After.AutoVacuum != AutoVacuumIncremental {
		t.Fatalf("small database not converted: %+v", rep)
	}
	// Every connection of the pool sees the persisted mode, and a second
	// call does nothing.
	s, err := Space(ctx, db)
	if err != nil || s.AutoVacuum != AutoVacuumIncremental {
		t.Fatalf("auto_vacuum=%d err=%v", s.AutoVacuum, err)
	}
	if rep, err = PrepareReclaim(ctx, db, path, ReclaimPolicy{}); err != nil || rep.Vacuumed {
		t.Fatalf("second prepare: %+v %v", rep, err)
	}
}

func TestPrepareReclaimLargeDatabaseNeedsFreeSpaceAndDisk(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "l.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// 4 MiB live, 6 MiB free; thresholds scaled down to these sizes.
	fillAndDelete(t, db, 6)
	if _, err := db.ExecContext(ctx, `CREATE TABLE live(v BLOB); WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 1024)
		INSERT INTO live(v) SELECT randomblob(4000) FROM n`); err != nil {
		t.Fatal(err)
	}
	pol := ReclaimPolicy{SmallBytes: 1 << 20, MinFreeFraction: 0.25, MinFreeBytes: 1 << 20}

	rep, err := PrepareReclaim(ctx, db, path, pol)
	if err != nil || rep.Vacuumed || !strings.Contains(rep.Skipped, "unknown") {
		t.Fatalf("converted without a disk check: %+v %v", rep, err)
	}
	pol.DiskAvail = func(string) (uint64, error) { return 1 << 20, nil }
	if rep, err = PrepareReclaim(ctx, db, path, pol); err != nil || rep.Vacuumed || !strings.Contains(rep.Skipped, "needs") {
		t.Fatalf("converted without enough disk: %+v %v", rep, err)
	}
	high := pol
	high.MinFreeFraction = 0.9
	high.DiskAvail = func(string) (uint64, error) { return 1 << 40, nil }
	if rep, err = PrepareReclaim(ctx, db, path, high); err != nil || rep.Vacuumed || !strings.Contains(rep.Skipped, "free") {
		t.Fatalf("converted with too little free space: %+v %v", rep, err)
	}

	pol.DiskAvail = high.DiskAvail
	rep, err = PrepareReclaim(ctx, db, path, pol)
	if err != nil || !rep.Vacuumed {
		t.Fatalf("not converted: %+v %v", rep, err)
	}
	if rep.After.AutoVacuum != AutoVacuumIncremental || rep.After.FreePages != 0 || rep.After.Pages >= rep.Before.Pages {
		t.Fatalf("before %+v after %+v", rep.Before, rep.After)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM live`).Scan(&n); err != nil || n != 1024 {
		t.Fatalf("live rows %d %v", n, err)
	}
}

func TestReclaimFreeKeepsReserveAndBoundsEachCall(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "r.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Not in incremental mode: nothing happens.
	fillAndDelete(t, db, 2)
	if n, err := ReclaimFree(ctx, db, 0, 1<<30); err != nil || n != 0 {
		t.Fatalf("reclaimed %d in auto_vacuum=NONE (%v)", n, err)
	}
	if _, err := PrepareReclaim(ctx, db, path, ReclaimPolicy{}); err != nil {
		t.Fatal(err)
	}
	fillAndDelete(t, db, 20)
	before, _ := Space(ctx, db)
	if before.FreeBytes() < 19<<20 {
		t.Fatalf("free %d", before.FreeBytes())
	}
	// At most 10 MiB per call (more than one step of reclaimStepPages).
	n, err := ReclaimFree(ctx, db, 2<<20, 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	mid, _ := Space(ctx, db)
	// The file also loses a few pointer-map pages.
	if n < 10<<20 || n > 10<<20+64<<10 || n != (before.Pages-mid.Pages)*before.PageSize {
		t.Fatalf("released %d: before %+v after %+v", n, before, mid)
	}
	// Then down to the 2 MiB reserve, and no further.
	if _, err := ReclaimFree(ctx, db, 2<<20, 1<<30); err != nil {
		t.Fatal(err)
	}
	after, _ := Space(ctx, db)
	if after.FreeBytes() != 2<<20 {
		t.Fatalf("free after reclaim %d, want the 2 MiB reserve", after.FreeBytes())
	}
	if n, err := ReclaimFree(ctx, db, 2<<20, 1<<30); err != nil || n != 0 {
		t.Fatalf("reclaimed %d below the reserve (%v)", n, err)
	}
}
