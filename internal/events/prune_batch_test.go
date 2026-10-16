package events

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// W-dbhygiene: a backlog is pruned in transactions of at most
// sqlitex.PruneBatch events, oldest first, and pruned_through ends at the
// retention boundary.
func TestPruneBacklogInBatches(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	db, l := open(t, filepath.Join(t.TempDir(), "e.db"), c, Retention{MaxEvents: 100})
	defer db.Close()
	n := 3*sqlitex.PruneBatch + 77
	cursors := appendN(t, db, l, n, contracts.EvSessionUpdated, "ses_synth")
	got, err := l.Prune(context.Background())
	if err != nil || got != int64(n-100) {
		t.Fatalf("prune: %d %v, want %d", got, err, n-100)
	}
	var left int
	var pruned int64
	_ = db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&left)
	_ = db.QueryRow(`SELECT pruned_through FROM events_meta`).Scan(&pruned)
	if left != 100 || pruned != cursors[n-101] {
		t.Fatalf("left %d pruned_through %d, want 100 and %d", left, pruned, cursors[n-101])
	}
	if _, err := l.ListEvents(context.Background(), contracts.ListEventsRequest{AfterCursor: cursors[n-101]}); err != nil {
		t.Fatalf("reader at the boundary: %v", err)
	}
	if _, err := l.ListEvents(context.Background(), contracts.ListEventsRequest{AfterCursor: cursors[n-102]}); !contracts.IsCode(err, contracts.ErrResetRequired) {
		t.Fatalf("reader before the boundary: %v", err)
	}
}
