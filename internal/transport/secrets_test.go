package transport

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

const synthSessionToken = "pys_SYNTHETIC-session-token-for-journal-test"

// The command journal never stores a start_run session token; the handler
// still receives it in memory.
func TestCommandJournalOmitsSessionToken(t *testing.T) {
	e := newEnv(t)
	sr := contracts.StartRunCommand{RunID: "run_synth-1", Session: contracts.Session{SessionID: "ses_synth-1"}, SessionToken: synthSessionToken}
	payload, _ := json.Marshal(sr)
	cmd := contracts.NodeCommand{CommandID: contracts.NewID(contracts.PrefixCommand), Seq: 1, Kind: contracts.CmdStartRun, IssuedAt: time.Now().UTC(), Payload: payload}
	e.hub.commands = []contracts.NodeCommand{cmd}
	e.start()
	var mu sync.Mutex
	var gotToken string
	h := &countingHandler{fn: func(c contracts.NodeCommand) (contracts.CommandResult, error) {
		var got contracts.StartRunCommand
		_ = json.Unmarshal(c.Payload, &got)
		mu.Lock()
		gotToken = got.SessionToken
		mu.Unlock()
		return contracts.CommandResult{Outcome: contracts.OutcomeSucceeded}, nil
	}}
	db := openDB(t, "node.db")
	runLink(t, e.newLink(db, h, nil))
	waitFor(t, "start_run executed", func() bool { return h.count(cmd.CommandID) == 1 })
	mu.Lock()
	if gotToken != synthSessionToken {
		t.Fatalf("handler got session token %q", gotToken)
	}
	mu.Unlock()
	var raw string
	if err := db.QueryRow(`SELECT command FROM nodelink_commands WHERE command_id=?`, cmd.CommandID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, synthSessionToken) || strings.Contains(raw, `"session_token"`) {
		t.Fatalf("journal stores the session token: %s", raw)
	}
}

// A journaled start_run whose in-memory secret was lost (node restarted
// between recording and executing) is not executed without its token: it
// fails clearly and can be retried.
func TestRecordedCommandWithoutSecretFailsClosed(t *testing.T) {
	e := newEnv(t)
	e.start()
	db := openDB(t, "node.db")
	h := &countingHandler{}
	link := e.newLink(db, h, nil)
	// Simulate a journal entry recorded by a previous process.
	sr := contracts.StartRunCommand{RunID: "run_synth-2", Session: contracts.Session{SessionID: "ses_synth-2"}, SessionToken: synthSessionToken}
	payload, _ := json.Marshal(sr)
	cmd := contracts.NodeCommand{CommandID: contracts.NewID(contracts.PrefixCommand), Seq: 7, Kind: contracts.CmdStartRun, IssuedAt: time.Now().UTC(), Payload: payload}
	jcmd, secret := journalCopy(cmd)
	if !secret {
		t.Fatal("start_run with a token not detected as secret-bearing")
	}
	raw, _ := json.Marshal(jcmd)
	if _, err := db.Exec(`INSERT INTO nodelink_commands(command_id, seq, kind, command, state, received_at) VALUES(?,?,?,?,?,?)`,
		cmd.CommandID, cmd.Seq, string(cmd.Kind), raw, cmdRecorded, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	runLink(t, link)
	waitFor(t, "failed result for the secret-less command", func() bool {
		var ok bool
		e.hub.snapshot(func(hb *fakeHub) {
			for _, r := range hb.results {
				if r.CommandID == cmd.CommandID {
					ok = r.Outcome == contracts.OutcomeFailed && r.Error != nil && r.Error.Code == contracts.ErrUnavailable
				}
			}
		})
		return ok
	})
	if h.count(cmd.CommandID) != 0 {
		t.Fatal("start_run executed without its session token")
	}
	_ = context.Background()
}
