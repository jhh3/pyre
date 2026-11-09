package hub

import (
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

func (x *harness) count(table string) int {
	x.t.Helper()
	var n int
	if err := x.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		x.t.Fatal(err)
	}
	return n
}

// A20 soak: finished node commands and operation-idempotency records grew
// without bound (about 2.6 KB per delivered message, never deleted).
// Maintenance prunes them after their retention, at most once per
// PruneInterval, and never touches unfinished commands or recent records.
func TestMaintenancePrunesFinishedCommandsAndOldOperations(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	x.start(owner, "op_p1", nodeMac) // a launch command (left queued/sent) + an operation
	x.start(owner, "op_p2", nodeMac)
	cmds, ops := x.count("commands"), x.count("operations")
	if cmds < 2 || ops < 2 {
		t.Fatalf("setup: %d commands, %d operations", cmds, ops)
	}
	old := persistence.Time(x.clock.Now().Add(-8 * 24 * time.Hour))
	// One finished command completed 8 days ago; the others stay unfinished.
	if _, err := x.db.Exec(`UPDATE commands SET state=?, completed_at=? WHERE rowid=(SELECT MIN(rowid) FROM commands)`, cmdDone, old); err != nil {
		t.Fatal(err)
	}
	// An unfinished command issued 8 days ago is never pruned.
	if _, err := x.db.Exec(`UPDATE commands SET issued_at=? WHERE rowid=(SELECT MAX(rowid) FROM commands)`, old); err != nil {
		t.Fatal(err)
	}
	// One operation record 31 days old, the rest fresh.
	if _, err := x.db.Exec(`UPDATE operations SET created_at=? WHERE operation_id='op_p1'`, sqlitex.FormatTime(time.Now().Add(-31*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := x.h.RunMaintenance(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := x.count("commands"); got != cmds-1 {
		t.Fatalf("commands %d -> %d, want one pruned", cmds, got)
	}
	if got := x.count("operations"); got != ops-1 {
		t.Fatalf("operations %d -> %d, want one pruned", ops, got)
	}
	var n int
	_ = x.db.QueryRow(`SELECT COUNT(*) FROM operations WHERE operation_id='op_p1'`).Scan(&n)
	if n != 0 {
		t.Fatal("the 31-day-old operation is still there")
	}
	// Rate limit: a second finished old command is left until PruneInterval passes.
	if _, err := x.db.Exec(`UPDATE commands SET state=?, completed_at=? WHERE rowid=(SELECT MIN(rowid) FROM commands)`, cmdDone, old); err != nil {
		t.Fatal(err)
	}
	before := x.count("commands")
	if err := x.h.RunMaintenance(t.Context()); err != nil {
		t.Fatal(err)
	}
	if x.count("commands") != before {
		t.Fatal("pruned again before PruneInterval")
	}
	x.clock.Add(time.Hour + time.Second)
	if err := x.h.RunMaintenance(t.Context()); err != nil {
		t.Fatal(err)
	}
	if x.count("commands") != before-1 {
		t.Fatal("not pruned after PruneInterval")
	}
}

// A20 soak: the report dedup prune runs every maintenance pass and must use
// the (node_id, origin_seq) key instead of scanning node_reports; it still
// deletes exactly the rows more than reportDedupWindow below the committed
// sequence.
func TestReportDedupPruneUsesKeyAndKeepsWindow(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	if _, err := x.db.Exec(`UPDATE node_state SET committed_report_seq=? WHERE node_id=?`, reportDedupWindow+50, nodeMac); err != nil {
		t.Fatal(err)
	}
	for _, seq := range []int64{1, 49, 50, 51, reportDedupWindow + 10} {
		if _, err := x.db.Exec(`INSERT OR IGNORE INTO node_reports(node_id, origin_seq, kind, processed_at) VALUES(?,?,?,?)`, nodeMac, seq, "synthetic", "2026-10-04T12:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.h.RunMaintenance(t.Context()); err != nil {
		t.Fatal(err)
	}
	var left []int64
	rows, _ := x.db.Query(`SELECT origin_seq FROM node_reports WHERE node_id=? AND kind='synthetic' ORDER BY origin_seq`, nodeMac)
	for rows.Next() {
		var s int64
		_ = rows.Scan(&s)
		left = append(left, s)
	}
	rows.Close()
	if len(left) != 3 || left[0] != 50 {
		t.Fatalf("kept %v, want [50 51 %d]", left, reportDedupWindow+10)
	}
	var detail string
	var id, parent, unused int
	if err := x.db.QueryRow("EXPLAIN QUERY PLAN "+pruneReportDedupQuery, nodeMac, nodeMac, 5, 500).Scan(&id, &parent, &unused, &detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "USING") || strings.HasPrefix(detail, "SCAN") {
		t.Fatalf("dedup prune plan %q scans node_reports", detail)
	}
}

// W-dbspace: operation records are kept a week (they were kept 30 days:
// about 750 MB on server), the product's own records are never pruned, and
// the retention pass returns free pages beyond the reserve to the
// filesystem once hub.db is in incremental auto-vacuum mode.
func TestRetentionKeepsAWeekOfOperationsAndReclaimsSpace(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	x.start(owner, "op_w1", nodeMac)
	x.start(owner, "op_w2", nodeMac)
	x.start(owner, "op_w3", nodeMac)
	for op, age := range map[string]time.Duration{"op_w1": 8 * 24 * time.Hour, "op_w2": 6 * 24 * time.Hour} {
		if _, err := x.db.Exec(`UPDATE operations SET created_at=? WHERE operation_id=?`, sqlitex.FormatTime(time.Now().Add(-age)), op); err != nil {
			t.Fatal(err)
		}
	}
	kept := map[string]int{}
	for _, table := range []string{"sessions", "runs", "messages", "message_receipts"} {
		kept[table] = x.count(table)
	}

	// hub.db as assemble opens it: incremental mode, then 12 MiB freed.
	if _, err := sqlitex.PrepareReclaim(t.Context(), x.db, x.path, sqlitex.ReclaimPolicy{}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.db.Exec(`CREATE TABLE scratch(v BLOB); WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 3072)
		INSERT INTO scratch(v) SELECT randomblob(4000) FROM n; DELETE FROM scratch`); err != nil {
		t.Fatal(err)
	}
	oldKeep, oldMax := reclaimKeepBytes, reclaimMaxBytes
	reclaimKeepBytes, reclaimMaxBytes = 2<<20, 8<<20
	t.Cleanup(func() { reclaimKeepBytes, reclaimMaxBytes = oldKeep, oldMax })
	before, _ := sqlitex.Space(t.Context(), x.db)

	if err := x.h.RunMaintenance(t.Context()); err != nil {
		t.Fatal(err)
	}
	var ops []string
	rows, err := x.db.Query(`SELECT operation_id FROM operations WHERE operation_id LIKE 'op_w%' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ops = append(ops, id)
	}
	rows.Close()
	if strings.Join(ops, ",") != "op_w2,op_w3" {
		t.Fatalf("operations left %v, want the 8-day-old one pruned and the rest kept", ops)
	}
	for table, n := range kept {
		if got := x.count(table); got != n {
			t.Fatalf("%s %d -> %d: retention touched a kept table", table, n, got)
		}
	}
	mid, _ := sqlitex.Space(t.Context(), x.db)
	if released := (before.Pages - mid.Pages) * before.PageSize; released < 8<<20 || released > 9<<20 {
		t.Fatalf("first pass released %d bytes, want the 8 MiB bound (before %+v after %+v)", released, before, mid)
	}

	// The next pass (after PruneInterval) releases down to the reserve.
	x.clock.Add(time.Hour + time.Second)
	if err := x.h.RunMaintenance(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, _ := sqlitex.Space(t.Context(), x.db)
	if after.FreeBytes() > 2<<20 || after.FreeBytes() < 2<<20-int64(after.PageSize) {
		t.Fatalf("free after second pass %d bytes, want the 2 MiB reserve", after.FreeBytes())
	}
}
