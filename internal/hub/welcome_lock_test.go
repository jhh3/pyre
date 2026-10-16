package hub

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// dbTokens writes on its own pooled connection like the real auth store.
type dbTokens struct {
	fakeTokens
	db *sql.DB
}

func (t *dbTokens) IssueSessionToken(ctx context.Context, sessionID string) (string, error) {
	if _, err := t.db.ExecContext(ctx, `INSERT INTO synth_tokens(session_id) VALUES (?)`, sessionID); err != nil {
		return "", err
	}
	return t.fakeTokens.IssueSessionToken(ctx, sessionID)
}

// A launch re-queued by a disconnect must not fail the next hello: minting
// its session token inside the welcome transaction waited on that
// transaction's own write lock until busy_timeout (server, 2026-10-08).
func TestWelcomeWithQueuedLaunchDoesNotSelfLock(t *testing.T) {
	x := newHarness(t)
	if _, err := x.db.Exec(`CREATE TABLE synth_tokens(session_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	x.h.cfg.Tokens = &dbTokens{db: x.db}
	x.connect(nodeLin, 0)
	if _, err := x.h.StartSession(as(owner), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op-welcome-lock"}, NodeID: nodeLin, Harness: contracts.HarnessClaude, Cwd: "/home/synth/work"}); err != nil {
		t.Fatal(err)
	}
	x.disconnect(nodeLin) // the unacked start_run goes back to queued

	start := time.Now()
	if _, err := x.h.HandleHello(nodeCtx(nodeLin), nodeLin, helloFor(nodeLin, 4)); err != nil {
		t.Fatal(err)
	}
	cmds, err := x.h.WelcomeCommands(context.Background(), nodeLin)
	if err != nil {
		t.Fatalf("welcome commands: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("welcome took %v (lock wait)", d)
	}
	if len(cmds) != 1 || cmds[0].Kind != contracts.CmdStartRun {
		t.Fatalf("welcome commands = %+v", cmds)
	}
}
