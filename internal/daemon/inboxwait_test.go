package daemon

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

func deliverTo(session, msgID string) contracts.NodeCommand {
	return command(contracts.CmdDeliver, contracts.DeliverCommand{SessionID: session, Message: contracts.MessageEnvelope{SchemaVersion: 1,
		MessageID: msgID, OperationID: "op_" + msgID, Sender: contracts.Principal{Kind: contracts.PrincipalOwner, ID: "o"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: session}, Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: "synthetic"}}})
}

// W-localwait: a session's local inbox wait wakes on a deliver command for
// that session (not another's), on a hub link (re)connect, and otherwise
// times out; a session may only wait on its own inbox.
func TestLocalInboxWait(t *testing.T) {
	dir := shortDir(t)
	hub := &fakeHub{byOp: map[string]contracts.MessageView{}}
	ma := mock.NewAdapter()
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{ma}, Upstream: hub.client, OwnerToken: "pyre_owner_test_token_0000000000", OutboxFlush: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = n.d.ServeLocal(ctx) }()
	sock := n.d.SocketPath()
	waitFor(t, "socket", func() bool { _, err := os.Stat(sock); return err == nil })

	sa := startCmd("ses_wa", "run_wa", contracts.HarnessMock, t.TempDir())
	sa.SessionToken = "pyre_s_session_token_wait_a_00000000"
	n.start(sa)
	sb := startCmd("ses_wb", "run_wb", contracts.HarnessMock, t.TempDir())
	sb.SessionToken = "pyre_s_session_token_wait_b_00000000"
	n.start(sb)
	agentA := socketClient(sock, sa.SessionToken)

	// Only its own inbox.
	if _, err := agentA.InboxWait(ctx, contracts.InboxWaitRequest{SessionID: "ses_wb"}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("waiting on another session's inbox = %v", err)
	}

	// First call (no epoch) answers at once: the caller lists, then arms.
	r0, err := agentA.InboxWait(ctx, contracts.InboxWaitRequest{SessionID: "ses_wa", WaitSeconds: 30})
	if err != nil || !r0.Woken || r0.Epoch == "" {
		t.Fatalf("first wait = %+v %v", r0, err)
	}

	// Timeout: nothing for this session.
	start := time.Now()
	r1, err := agentA.InboxWait(ctx, contracts.InboxWaitRequest{SessionID: "ses_wa", Epoch: r0.Epoch, AfterGeneration: r0.Generation, WaitSeconds: 1})
	if err != nil || r1.Woken || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("timeout wait = %+v %v after %s", r1, err, time.Since(start))
	}

	type res struct {
		r   contracts.InboxWaitResponse
		err error
		at  time.Time
	}
	arm := func(after contracts.InboxWaitResponse) chan res {
		ch := make(chan res, 1)
		go func() {
			r, err := agentA.InboxWait(ctx, contracts.InboxWaitRequest{SessionID: "ses_wa", Epoch: after.Epoch, AfterGeneration: after.Generation, WaitSeconds: 20})
			ch <- res{r, err, time.Now()}
		}()
		waitFor(t, "waiter armed", func() bool { return n.d.inboxWait.waiting() == 1 })
		return ch
	}

	// Mail for another session does not wake A; mail for A does.
	done := arm(r1)
	n.do(deliverTo("ses_wb", "msg_wait_b"))
	select {
	case got := <-done:
		t.Fatalf("woken by another session's mail: %+v", got)
	case <-time.After(300 * time.Millisecond):
	}
	n.do(deliverTo("ses_wa", "msg_wait_a"))
	var got res
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deliver for the session did not wake its wait")
	}
	if got.err != nil || !got.r.Woken || got.r.Generation <= r1.Generation {
		t.Fatalf("woken wait = %+v", got)
	}

	// A signal between two waits is not lost: the next wait returns at once.
	n.do(deliverTo("ses_wa", "msg_wait_a2"))
	r2, err := agentA.InboxWait(ctx, contracts.InboxWaitRequest{SessionID: "ses_wa", Epoch: got.r.Epoch, AfterGeneration: got.r.Generation, WaitSeconds: 20})
	if err != nil || !r2.Woken {
		t.Fatalf("signal between waits lost: %+v %v", r2, err)
	}

	// Hub link reconnect wakes every waiter.
	done = arm(r2)
	n.d.SetHubLink(false)
	n.d.SetHubLink(true)
	select {
	case got = <-done:
		if got.err != nil || !got.r.Woken {
			t.Fatalf("reconnect wake = %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hub reconnect did not wake the wait")
	}

	// A different epoch (pyred restarted) answers at once.
	r3, err := agentA.InboxWait(ctx, contracts.InboxWaitRequest{SessionID: "ses_wa", Epoch: "stale", AfterGeneration: got.r.Generation, WaitSeconds: 20})
	if err != nil || !r3.Woken || r3.Epoch != got.r.Epoch {
		t.Fatalf("epoch mismatch = %+v %v", r3, err)
	}
	if n.d.inboxWait.waiting() != 0 {
		t.Fatalf("leaked waiters: %d", n.d.inboxWait.waiting())
	}
}
