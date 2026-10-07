package resilience

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/supervisor"
	"github.com/jhh3/pyre/tests/resilience/faultnet"
	"github.com/jhh3/pyre/tests/resilience/stack"
)

// Real-stack scenarios: pyre-hub and pyred (or the test-binary daemon role
// with the shell adapter) as separate processes, killed with SIGKILL and
// restarted on the same data. Skipped with -short.

func skipShort(t *testing.T) {
	if testing.Short() {
		t.Skip("real-stack scenario (builds and runs pyre-hub/pyred); skipped with -short")
	}
}

func startSession(t *testing.T, cl *client.Client, nodeID string, h contracts.Harness, cwd, title string) contracts.SessionView {
	t.Helper()
	res, err := cl.StartSession(stack.OwnerCtx(t), contracts.StartSessionRequest{NodeID: nodeID, Harness: h, Cwd: cwd, Title: title})
	if err != nil {
		t.Fatalf("start %s session: %v", h, err)
	}
	sid := res.Session.Session.SessionID
	var sv contracts.SessionView
	stack.WaitFor(t, 30*time.Second, "run live", func() bool {
		sv, err = cl.GetSession(context.Background(), contracts.GetRequest{ID: sid})
		return err == nil && sv.Run != nil && (sv.Run.State == contracts.RunWaitingInput || sv.Run.State == contracts.RunRunning)
	})
	return sv
}

func countMarker(t *testing.T, cl *client.Client, sid, msgID string) int {
	t.Helper()
	rs, err := cl.ReadSession(context.Background(), contracts.ReadSessionRequest{SessionID: sid, Limit: 500})
	if err != nil {
		return -1
	}
	n := 0
	for _, e := range rs.Entries {
		if e.MessageID == msgID || strings.Contains(e.Text, contracts.MessageMarker(msgID)) {
			n++
		}
	}
	return n
}

func msgState(cl *client.Client, id string) (contracts.MessageView, error) {
	return cl.GetMessage(context.Background(), contracts.GetRequest{ID: id})
}

// TestStackA05MessageSurvivesHubRestartWhileRecipientDisconnected: the
// recipient node is cut off (proxy refuses), a message is sent and stored,
// the hub is SIGKILLed and restarted, then the recipient reconnects. The
// message stays durable (stored, queued) across the restart and is delivered
// to the runtime exactly once. A retried send with the same operation_id
// after the restart does not create a second message.
func TestStackA05MessageSurvivesHubRestartWhileRecipientDisconnected(t *testing.T) {
	skipShort(t)
	dir := stack.ShortDir(t)
	bins := stack.Build(t)
	hub := stack.StartHub(t, bins, dir)
	px, err := faultnet.New(hub.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	node := stack.EnrollNode(t, hub, dir, "a05")
	node.HubURL = "https://" + px.Addr()
	node.Extra = []string{"--dev-mock"}
	node.Start(t)
	node.WaitConnected(t, 30*time.Second)
	cl := hub.Client()
	sv := startSession(t, cl, node.NodeID, contracts.HarnessMock, dir, "a05-recipient")
	sid := sv.Session.SessionID

	// Cut the recipient off.
	px.SetMode(faultnet.ModeRefuse)
	px.CutAll()
	stack.WaitFor(t, 15*time.Second, "hub sees node disconnected", func() bool {
		n, err := cl.GetNode(context.Background(), contracts.GetRequest{ID: node.NodeID})
		return err == nil && n.Connection.State == contracts.ConnDisconnected
	})
	req := contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sid}, Body: contracts.MessageBody{Text: "synthetic a05 message"}}
	m, err := cl.SendMessage(stack.OwnerCtx(t), req)
	if err != nil {
		t.Fatal(err)
	}
	if m.Status.Storage != contracts.StorageStored || m.Status.Runtime == contracts.DeliveryAccepted {
		t.Fatalf("send while recipient offline: %+v", m.Status)
	}

	hub.Proc.Kill() // crash
	hub.Start(t)
	cl = hub.Client()
	got, err := msgState(cl, m.Envelope.MessageID)
	if err != nil || got.Status.Storage != contracts.StorageStored || got.Status.Runtime == contracts.DeliveryAccepted {
		t.Fatalf("after hub crash: %+v %v (message must stay stored and undelivered)", got.Status, err)
	}
	// The sender retries the same operation after the hub came back.
	again, err := cl.SendMessage(stack.OwnerCtx(t), req)
	if err != nil || again.Envelope.MessageID != m.Envelope.MessageID {
		t.Fatalf("retried send after restart: %v id=%s want %s", err, again.Envelope.MessageID, m.Envelope.MessageID)
	}

	px.SetMode(faultnet.ModePass)
	node.WaitConnected(t, 60*time.Second)
	stack.WaitFor(t, 60*time.Second, "delivery accepted", func() bool {
		v, err := msgState(cl, m.Envelope.MessageID)
		return err == nil && v.Status.Runtime == contracts.DeliveryAccepted
	})
	time.Sleep(2 * time.Second)
	if n := countMarker(t, cl, sid, m.Envelope.MessageID); n != 1 {
		t.Fatalf("message reached the runtime %d times, want exactly 1", n)
	}
	inbox, err := cl.ListMessages(context.Background(), contracts.ListMessagesRequest{Box: "inbox", Address: &contracts.Address{Kind: contracts.AddrSession, ID: sid}})
	if err != nil || len(inbox.Items) != 1 {
		t.Fatalf("recipient inbox has %d messages (%v), want 1", len(inbox.Items), err)
	}
	final, _ := msgState(cl, m.Envelope.MessageID)
	evidence(t, "A05: stored while recipient offline, survived hub SIGKILL+restart, retried send deduplicated, delivered once (runtime=%s evidence=%d receipts, bound run %s)",
		final.Status.Runtime, len(final.Status.Receipts), final.Status.BoundRunID)
}

// TestStackHubRestartWithQueuedCommandAndLiveRun (A19): a control command
// queued for a disconnected node survives a hub crash and is executed once
// when the node returns; the live run is untouched by the hub outage and
// the hub never reassigns it.
func TestStackHubRestartWithQueuedCommandAndLiveRun(t *testing.T) {
	skipShort(t)
	dir := stack.ShortDir(t)
	bins := stack.Build(t)
	hub := stack.StartHub(t, bins, dir)
	px, err := faultnet.New(hub.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	node := stack.EnrollNode(t, hub, dir, "a19")
	node.HubURL = "https://" + px.Addr()
	node.Extra = []string{"--dev-mock"}
	node.Start(t)
	node.WaitConnected(t, 30*time.Second)
	cl := hub.Client()
	keep := startSession(t, cl, node.NodeID, contracts.HarnessMock, dir, "a19-keep")
	stop := startSession(t, cl, node.NodeID, contracts.HarnessMock, dir, "a19-stop")

	px.SetMode(faultnet.ModeRefuse)
	px.CutAll()
	stack.WaitFor(t, 15*time.Second, "node disconnected", func() bool {
		n, err := cl.GetNode(context.Background(), contracts.GetRequest{ID: node.NodeID})
		return err == nil && n.Connection.State == contracts.ConnDisconnected
	})
	rcpt, err := cl.StopSession(stack.OwnerCtx(t), contracts.SessionCommandRequest{SessionID: stop.Session.SessionID, Reason: "synthetic a19"})
	if err != nil {
		t.Fatalf("stop while node offline: %v", err)
	}
	t.Logf("stop receipt while offline: %+v", rcpt)
	// While disconnected, the live run must read as stale/unknown, never failed.
	kv, _ := cl.GetSession(context.Background(), contracts.GetRequest{ID: keep.Session.SessionID})
	if kv.Run == nil || kv.Run.State.Terminal() || !kv.Freshness.Stale && !kv.Run.Freshness.Stale {
		t.Fatalf("live run during node outage = %+v (freshness %+v)", kv.Run, kv.Freshness)
	}

	hub.Proc.Kill()
	hub.Start(t)
	cl = hub.Client()
	px.SetMode(faultnet.ModePass)
	node.WaitConnected(t, 60*time.Second)
	stack.WaitFor(t, 60*time.Second, "queued stop executed", func() bool {
		v, err := cl.GetSession(context.Background(), contracts.GetRequest{ID: stop.Session.SessionID})
		return err == nil && v.Run != nil && v.Run.State.Terminal()
	})
	kv, err = cl.GetSession(context.Background(), contracts.GetRequest{ID: keep.Session.SessionID})
	if err != nil || kv.Run == nil || kv.Run.RunID != keep.Run.RunID || kv.Run.State.Terminal() {
		t.Fatalf("kept run after hub crash = %+v %v", kv.Run, err)
	}
	for _, s := range []contracts.SessionView{keep, stop} {
		runs, err := cl.ListRuns(context.Background(), contracts.ListRunsRequest{SessionID: s.Session.SessionID})
		if err != nil || len(runs.Items) != 1 {
			t.Fatalf("session %s has %d runs after restarts (%v); the hub must never relaunch or reassign", s.Session.Title, len(runs.Items), err)
		}
	}
	nodes, _ := cl.ListNodes(context.Background(), contracts.ListNodesRequest{})
	if len(nodes.Items) != 1 {
		t.Fatalf("%d nodes after restarts", len(nodes.Items))
	}
	evidence(t, "A19: stop queued while node offline survived hub SIGKILL and ran once on reconnect; other run kept id %s state %s; 1 run per session; 1 node",
		kv.Run.RunID, kv.Run.State)
}

func needTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
}

// TestStackDaemonCrashRecoversLiveShellRun: the real pyred is SIGKILLed
// while a real shell run (isolated tmux server) is live. The run keeps
// running, and the restarted daemon recovers the same run (same run ID,
// same tmux server PID) without launching a duplicate.
func TestStackDaemonCrashRecoversLiveShellRun(t *testing.T) {
	skipShort(t)
	needTmux(t)
	dir := stack.ShortDir(t)
	bins := stack.Build(t)
	hub := stack.StartHub(t, bins, dir)
	node := stack.EnrollNode(t, hub, dir, "shell") // the real pyred: its native shell adapter on <state>/tmux/pyre.sock
	tm := supervisor.Tmux{Socket: stack.TmuxSocket(node.State), OwnedRoot: node.State}
	t.Cleanup(func() { _ = tm.Kill(context.Background()) })
	node.Start(t)
	node.WaitConnected(t, 30*time.Second)
	cl := hub.Client()
	sv := startSession(t, cl, node.NodeID, contracts.HarnessShell, dir, "live-shell")
	pid, err := tm.ServerPID(context.Background())
	if err != nil || pid == 0 {
		t.Fatalf("tmux server pid: %v (%d)\n%s", err, pid, node.Proc.Tail(3000))
	}

	node.Proc.Kill()
	if !supervisor.Alive(pid) {
		t.Fatal("killing the daemon killed the run's tmux server")
	}
	stack.WaitFor(t, 20*time.Second, "hub sees daemon gone", func() bool {
		n, err := cl.GetNode(context.Background(), contracts.GetRequest{ID: node.NodeID})
		return err == nil && n.Connection.State == contracts.ConnDisconnected
	})
	v, _ := cl.GetSession(context.Background(), contracts.GetRequest{ID: sv.Session.SessionID})
	if v.Run == nil || v.Run.State.Terminal() {
		t.Fatalf("run marked terminal while only the daemon died: %+v", v.Run)
	}

	node.Start(t)
	node.WaitConnected(t, 30*time.Second)
	time.Sleep(3 * time.Second) // allow recovery + observation reports
	v, err = cl.GetSession(context.Background(), contracts.GetRequest{ID: sv.Session.SessionID})
	if err != nil || v.Run == nil || v.Run.RunID != sv.Run.RunID || v.Run.State.Terminal() {
		t.Fatalf("after daemon restart: %+v %v\n%s", v.Run, err, node.Proc.Tail(3000))
	}
	runs, _ := cl.ListRuns(context.Background(), contracts.ListRunsRequest{SessionID: sv.Session.SessionID})
	if len(runs.Items) != 1 {
		t.Fatalf("%d runs after daemon crash; recovery must never relaunch", len(runs.Items))
	}
	pid2, _ := tm.ServerPID(context.Background())
	if pid2 != pid {
		t.Fatalf("tmux server pid changed %d -> %d (relaunched)", pid, pid2)
	}
	out, _ := exec.Command("tmux", "-S", stack.TmuxSocket(node.State), "list-sessions", "-F", "#{session_name}").CombinedOutput()
	if lines := strings.Fields(strings.TrimSpace(string(out))); len(lines) != 1 {
		t.Fatalf("tmux sessions after recovery: %q", out)
	}
	evidence(t, "daemon SIGKILL with live shell run: tmux pid %d survived, run %s recovered (state %s), 1 run, 1 tmux session", pid, v.Run.RunID, v.Run.State)
}

// TestStackCrashAfterSubmitIsIndeterminate (A06): the daemon dies after the
// runtime accepted a message and before the receipt was recorded. After
// restart the delivery is reported indeterminate, bound to the original
// run, and is not silently retried or requeued. An explicit reissue needs
// accept_duplicate_risk.
func TestStackCrashAfterSubmitIsIndeterminate(t *testing.T) {
	skipShort(t)
	dir := stack.ShortDir(t)
	bins := stack.Build(t)
	hub := stack.StartHub(t, bins, dir)
	node := stack.EnrollNode(t, hub, dir, "crash")
	node.Exec = []string{os.Args[0]}
	node.Env = []string{stack.RoleEnv + "=" + stack.RoleDaemon}
	node.Extra = []string{"--mock"}
	node.Start(t)
	node.WaitConnected(t, 30*time.Second)
	cl := hub.Client()
	sv := startSession(t, cl, node.NodeID, contracts.HarnessMock, dir, "crash-target")
	m, err := cl.SendMessage(stack.OwnerCtx(t), contracts.SendMessageRequest{Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sv.Session.SessionID},
		Body: contracts.MessageBody{Text: "synthetic " + stack.CrashMarker}})
	if err != nil {
		t.Fatal(err)
	}
	stack.WaitFor(t, 30*time.Second, "daemon crashed in the delivery window", func() bool { return !node.Proc.Running() })
	node.Start(t)
	node.WaitConnected(t, 30*time.Second)
	var v contracts.MessageView
	stack.WaitFor(t, 60*time.Second, "delivery reconciled", func() bool {
		v, err = msgState(cl, m.Envelope.MessageID)
		return err == nil && v.Status.Runtime != contracts.DeliveryQueued && v.Status.Runtime != contracts.DeliveryNodeReceived && v.Status.Runtime != contracts.DeliveryAttempting
	})
	if v.Status.Runtime != contracts.DeliveryIndeterminate {
		t.Fatalf("after crash-after-submit: runtime=%s reason=%q; want indeterminate (absence of evidence never proves non-execution)", v.Status.Runtime, v.Status.RuntimeReason)
	}
	if v.Status.BoundRunID != sv.Run.RunID {
		t.Fatalf("bound run %q, want original %q", v.Status.BoundRunID, sv.Run.RunID)
	}
	time.Sleep(3 * time.Second)
	if v2, _ := msgState(cl, m.Envelope.MessageID); v2.Status.Runtime != contracts.DeliveryIndeterminate {
		t.Fatalf("indeterminate delivery changed to %s without evidence or a decision", v2.Status.Runtime)
	}
	if _, err := cl.ReissueMessage(stack.OwnerCtx(t), contracts.ReissueMessageRequest{MessageID: m.Envelope.MessageID}); err == nil {
		t.Fatal("reissue without accept_duplicate_risk was allowed")
	}
	re, err := cl.ReissueMessage(stack.OwnerCtx(t), contracts.ReissueMessageRequest{MessageID: m.Envelope.MessageID, AcceptDuplicateRisk: true})
	if err != nil || re.Envelope.Reissues != m.Envelope.MessageID || re.Envelope.MessageID == m.Envelope.MessageID {
		t.Fatalf("explicit reissue: %+v %v", re.Envelope, err)
	}
	evidence(t, "A06: daemon SIGKILL after runtime submit -> %s (%s), bound to %s; reissue requires accept_duplicate_risk and creates %s",
		v.Status.Runtime, v.Status.RuntimeReason, v.Status.BoundRunID, re.Envelope.MessageID)
}

// TestStackMockIndeterminateMarkerResolved: via the real pyred --dev-mock,
// a lost runtime receipt surfaces as indeterminate in the hub, survives a
// hub restart, and is resolved only by an explicit decision.
func TestStackMockIndeterminateMarkerResolved(t *testing.T) {
	skipShort(t)
	dir := stack.ShortDir(t)
	bins := stack.Build(t)
	hub := stack.StartHub(t, bins, dir)
	node := stack.EnrollNode(t, hub, dir, "indet")
	node.Extra = []string{"--dev-mock"}
	node.Start(t)
	node.WaitConnected(t, 30*time.Second)
	cl := hub.Client()
	sv := startSession(t, cl, node.NodeID, contracts.HarnessMock, dir, "indet")
	m, err := cl.SendMessage(stack.OwnerCtx(t), contracts.SendMessageRequest{Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sv.Session.SessionID},
		Body: contracts.MessageBody{Text: "synthetic [mock:indeterminate]"}})
	if err != nil {
		t.Fatal(err)
	}
	stack.WaitFor(t, 60*time.Second, "indeterminate", func() bool {
		v, err := msgState(cl, m.Envelope.MessageID)
		return err == nil && v.Status.Runtime == contracts.DeliveryIndeterminate
	})
	hub.Proc.Kill()
	hub.Start(t)
	cl = hub.Client()
	if v, err := msgState(cl, m.Envelope.MessageID); err != nil || v.Status.Runtime != contracts.DeliveryIndeterminate {
		t.Fatalf("after hub restart: %+v %v", v.Status, err)
	}
	att, _ := cl.ListAttention(context.Background(), contracts.ListAttentionRequest{Kinds: []contracts.AttentionKind{contracts.AttnIndeterminate}})
	r, err := cl.ResolveDelivery(stack.OwnerCtx(t), contracts.ResolveDeliveryRequest{MessageID: m.Envelope.MessageID, Resolution: contracts.DeliveryAccepted, Note: "checked transcript"})
	if err != nil || r.Status.Runtime != contracts.DeliveryAccepted {
		t.Fatalf("resolve: %+v %v", r.Status, err)
	}
	evidence(t, "lost receipt -> indeterminate (attention items: %d), survived hub restart, resolved explicitly", len(att.Items))
}

// TestStackQueuedFollowUpLatency: two messages sent back-to-back to one
// idle session. The second queues behind the first (the runtime is busy
// after accepting input) and must be delivered promptly once the runtime is
// idle again — within a few active poll intervals (Poll.Interval 2s), not
// after the idle backoff ceiling (Poll.IdleMax 30s).
func TestStackQueuedFollowUpLatency(t *testing.T) {
	skipShort(t)
	dir := stack.ShortDir(t)
	bins := stack.Build(t)
	hub := stack.StartHub(t, bins, dir)
	node := stack.EnrollNode(t, hub, dir, "followup")
	node.Extra = []string{"--dev-mock"}
	node.Start(t)
	node.WaitConnected(t, 30*time.Second)
	cl := hub.Client()
	sv := startSession(t, cl, node.NodeID, contracts.HarnessMock, dir, "followup")
	time.Sleep(35 * time.Second) // idle long enough for the run's poll interval to back off fully
	var ids []string
	for i := 0; i < 2; i++ {
		m, err := cl.SendMessage(stack.OwnerCtx(t), contracts.SendMessageRequest{Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sv.Session.SessionID},
			Body: contracts.MessageBody{Text: "synthetic follow-up"}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.Envelope.MessageID)
	}
	start := time.Now()
	acceptedAt := make([]time.Duration, 2)
	for i, id := range ids {
		stack.WaitFor(t, 90*time.Second, "accepted", func() bool {
			v, err := msgState(cl, id)
			return err == nil && v.Status.Runtime == contracts.DeliveryAccepted
		})
		acceptedAt[i] = time.Since(start)
	}
	gap := acceptedAt[1] - acceptedAt[0]
	t.Logf("first accepted after %v, second after %v (gap %v)", acceptedAt[0].Round(time.Millisecond), acceptedAt[1].Round(time.Millisecond), gap.Round(time.Millisecond))
	if gap > 10*time.Second {
		t.Fatalf("a message queued behind a busy run was delivered %v after the previous one (expected ~2-6s)", gap.Round(time.Second))
	}
	evidence(t, "queued follow-up delivered %v after the first", gap.Round(time.Millisecond))
}
