package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jhh3/pyre/internal/contracts"
)

// brokenLinkConn returns a linkConn whose writes fail, like a connection the
// network cut right after a command frame arrived.
func brokenLinkConn(t *testing.T) *linkConn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err == nil {
			c.CloseNow()
		}
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ws, _, err := websocket.Dial(ctx, "ws"+srv.URL[len("http"):], nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.CloseNow()
	lc := &linkConn{ws: ws, ctx: ctx, cancel: cancel, ackCh: make(chan struct{}, 1), timeout: time.Second}
	if err := lc.write(contracts.NodeFrame{Type: contracts.FramePing}); err == nil {
		t.Fatal("test precondition: writes on the broken connection must fail")
	}
	return lc
}

func drain(ch chan struct{}) {
	select {
	case <-ch:
	default:
	}
}

// Regression (tests/resilience TestLostAcknowledgementsThenCut flake): a
// command journaled on a connection that breaks before its CommandAck is
// written must still be executed. Previously the failed ack write returned
// before waking the command worker; the hub then never resent the command
// (LastCommandSeq already covered it), so it stayed "recorded" until the
// node restarted.
func TestCommandRecordedWhenAckWriteFailsIsExecuted(t *testing.T) {
	db := openDB(t, "node.db")
	h := &countingHandler{}
	l, err := NewNodeLink(NodeLinkConfig{HubURL: "https://127.0.0.1:1", NodeID: "node_synth-wake", Token: "pyn_synthetic-token-for-test", DB: db, Handler: h})
	if err != nil {
		t.Fatal(err)
	}
	lc := brokenLinkConn(t)
	cmd := contracts.NodeCommand{CommandID: contracts.NewID(contracts.PrefixCommand), Seq: 4, Kind: contracts.CmdAction, IssuedAt: time.Now().UTC(), Payload: json.RawMessage(`{}`)}

	drain(l.cmdKick)
	if err := l.receiveCommand(lc, cmd); err != nil {
		t.Fatalf("receive: %v", err)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM nodelink_commands WHERE command_id=?`, cmd.CommandID).Scan(&state); err != nil || state != cmdRecorded {
		t.Fatalf("journal state = %q, %v", state, err)
	}
	if seq, _ := l.LastCommandSeq(context.Background()); seq != 4 {
		t.Fatalf("LastCommandSeq = %d (the hub will not resend seq <= this)", seq)
	}
	if len(l.cmdKick) != 1 {
		t.Fatal("command recorded but the worker was not woken (ack write failure skipped the wake-up)")
	}

	// A duplicate delivery of a command still waiting to run wakes it too.
	drain(l.cmdKick)
	if err := l.receiveCommand(lc, cmd); err != nil {
		t.Fatalf("receive duplicate: %v", err)
	}
	if len(l.cmdKick) != 1 {
		t.Fatal("duplicate of a recorded command did not wake the worker")
	}

	// The worker, once woken, executes it exactly once and queues the result.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); l.commandWorker(ctx) }()
	waitFor(t, "command executed", func() bool {
		var st string
		_ = db.QueryRow(`SELECT state FROM nodelink_commands WHERE command_id=?`, cmd.CommandID).Scan(&st)
		return st == cmdDone
	})
	cancel()
	<-done
	if n := h.count(cmd.CommandID); n != 1 {
		t.Fatalf("handler ran %d times, want 1", n)
	}
	if n, _ := l.OutboxLen(context.Background()); n != 1 {
		t.Fatalf("result not queued: outbox=%d", n)
	}
}
