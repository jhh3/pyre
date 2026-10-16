package hub

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// W-dbhygiene: finished commands are deleted after 7 days and their large
// payloads/results dropped after 1 day, through the commands_finished
// index; unfinished and recent commands are untouched.
func TestCommandRetentionAndCompaction(t *testing.T) {
	x := newHarness(t)
	now := x.clock.Now()
	big := `{"entries":"` + strings.Repeat("t", 4096) + `"}`
	small := `{"ok":true}`
	type row struct {
		id, state      string
		issued, done   time.Duration // ago; done 0 = NULL completed_at
		payload, resul string
	}
	rows := []row{
		{"cmd_old", cmdDone, 9 * 24 * time.Hour, 8 * 24 * time.Hour, big, big},             // deleted
		{"cmd_cancelled", cmdCancelled, 8 * 24 * time.Hour, 0, big, ""},                    // deleted (issued_at)
		{"cmd_compact", cmdDone, 3 * 24 * time.Hour, 2 * 24 * time.Hour, big, big},         // compacted
		{"cmd_smallold", cmdExpired, 3 * 24 * time.Hour, 2 * 24 * time.Hour, small, small}, // kept as is
		{"cmd_recent", cmdDone, 2 * time.Hour, time.Hour, big, big},                        // kept as is
		{"cmd_queued", cmdQueued, 9 * 24 * time.Hour, 0, big, ""},                          // never touched
	}
	for i, r := range rows {
		var done, res any
		if r.done > 0 {
			done = persistence.Time(now.Add(-r.done))
		}
		if r.resul != "" {
			res = r.resul
		}
		if _, err := x.db.Exec(`INSERT INTO commands(command_id,node_id,seq,kind,actor,issued_at,payload,state,completed_at,result) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			r.id, nodeMac, 900+i, string(contracts.CmdReadTranscript), "owner:owner", persistence.Time(now.Add(-r.issued)), r.payload, r.state, done, res); err != nil {
			t.Fatal(err)
		}
	}
	st, err := x.h.RunRetention(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.CommandsDeleted != 2 {
		t.Fatalf("deleted %d, want 2", st.CommandsDeleted)
	}
	get := func(id string) (payload string, result sql.NullString, ok bool) {
		err := x.db.QueryRow(`SELECT payload, result FROM commands WHERE command_id=?`, id).Scan(&payload, &result)
		return payload, result, err == nil
	}
	for _, id := range []string{"cmd_old", "cmd_cancelled"} {
		if _, _, ok := get(id); ok {
			t.Fatalf("%s survived retention", id)
		}
	}
	if p, r, ok := get("cmd_compact"); !ok || p != "{}" || r.Valid {
		t.Fatalf("cmd_compact: %v %q %v", ok, p[:min(len(p), 20)], r.Valid)
	}
	if p, r, ok := get("cmd_smallold"); !ok || p != small || r.String != small {
		t.Fatalf("cmd_smallold changed: %q %q", p, r.String)
	}
	for _, id := range []string{"cmd_recent", "cmd_queued"} {
		if p, _, ok := get(id); !ok || p != big {
			t.Fatalf("%s changed", id)
		}
	}
	// A second pass finds nothing new to delete.
	if st, err := x.h.RunRetention(context.Background()); err != nil || st.CommandsDeleted != 0 {
		t.Fatalf("second pass: %+v %v", st, err)
	}
}

// W-dbhygiene: command.result events no longer copy large results (the
// command record keeps them; commands.get and the waiting caller read it).
func TestCommandResultEventOmitsLargeResult(t *testing.T) {
	x := newHarness(t)
	ctx := context.Background()
	emit := func(id string, result string) contracts.CommandResult {
		t.Helper()
		var res contracts.CommandResult
		err := sqlitex.Tx(ctx, x.db, func(tx *sql.Tx) error {
			return x.h.emitCommandResult(ctx, tx, commandRec{NodeCommand: contracts.NodeCommand{CommandID: id, Kind: contracts.CmdReadTranscript}},
				contracts.CommandResult{CommandID: id, Kind: contracts.CmdReadTranscript, Outcome: contracts.OutcomeSucceeded, Result: contracts.RawJSON(result)}, "hub", 0)
		})
		if err != nil {
			t.Fatal(err)
		}
		var payload string
		if err := x.db.QueryRow(`SELECT payload FROM events WHERE kind=? AND object_id=?`, string(contracts.EvCommandResult), id).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		if err := persistence.FromJSON(payload, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	if r := emit("cmd_bigres", `{"entries":"`+strings.Repeat("t", 8192)+`"}`); len(r.Result) != 0 || r.Outcome != contracts.OutcomeSucceeded {
		t.Fatalf("large result copied into the event: %d bytes", len(r.Result))
	}
	if r := emit("cmd_smallres", `{"ok":true}`); string(r.Result) != `{"ok":true}` {
		t.Fatalf("small result dropped: %q", r.Result)
	}
}
