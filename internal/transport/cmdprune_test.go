package transport

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *stepClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// A20 soak: finished command records were pruned only when pyred started,
// so a long-running node kept one row per command. A running link prunes
// them hourly; unacknowledged results and recent records stay.
func TestRunningLinkPrunesFinishedCommandsHourly(t *testing.T) {
	db := openDB(t, "node.db")
	clk := &stepClock{t: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	l, err := NewNodeLink(NodeLinkConfig{HubURL: "https://127.0.0.1:1", NodeID: "node_synth-prune", Token: "pyn_synthetic-token-for-test",
		DB: db, Handler: &countingHandler{}, Clock: clk, CommandRetention: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	insert := func(id string, resultSeq int64, finished time.Time) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO nodelink_commands(command_id, seq, kind, command, state, result_seq, received_at, finished_at) VALUES(?,?,?,?,?,?,?,?)`,
			id, resultSeq, "deliver", []byte(`{}`), cmdDone, resultSeq, sqlitex.FormatTime(finished), sqlitex.FormatTime(finished)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE nodelink_state SET value=10 WHERE key='acked_seq'`); err != nil {
		t.Fatal(err)
	}
	old := clk.Now().Add(-48 * time.Hour)
	insert("cmd_synth-old-acked", 5, old)
	insert("cmd_synth-old-unacked", 11, old)
	insert("cmd_synth-recent", 6, clk.Now().Add(-time.Hour))
	count := func() int {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM nodelink_commands`).Scan(&n)
		return n
	}
	l.maybePruneCommands(ctx)
	if n := count(); n != 2 {
		t.Fatalf("after first prune: %d rows, want 2 (unacked + recent)", n)
	}
	insert("cmd_synth-old-acked-2", 7, old)
	l.maybePruneCommands(ctx)
	if n := count(); n != 3 {
		t.Fatalf("pruned again within the interval: %d rows", n)
	}
	clk.add(commandPruneInterval)
	l.maybePruneCommands(ctx)
	if n := count(); n != 2 {
		t.Fatalf("not pruned after the interval: %d rows", n)
	}
}
