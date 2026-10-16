//go:build !windows

package resilience

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/transport"
	"github.com/jhh3/pyre/tests/resilience/faultnet"
	"github.com/jhh3/pyre/tests/resilience/refhub"
)

// TestHubCrashRestartQueuedReportsAndCommands (A05/A19 at the node-channel
// level): reports enqueued while the hub is down, commands queued while the
// node is down, and a hub crash in the middle of draining a burst. After
// restart every report is committed exactly once in origin order, every
// command executes exactly once, and the registry never duplicates the node.
func TestHubCrashRestartQueuedReportsAndCommands(t *testing.T) {
	ctx := ctxT(t)
	hub, _ := newHub(t, nil)
	nodeID, tok, err := hub.EnrollNode(ctx, "crash-node")
	if err != nil {
		t.Fatal(err)
	}
	h := newCountingHandler()
	node := newNode(t, nodeID, tok, hub.URL, h, nil)
	node.start()
	waitFor(t, 10*time.Second, "node connected", func() bool { return hub.Connected(nodeID) })

	next := 0
	enqueue := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			if err := node.enqueue(ctx, next); err != nil {
				t.Fatalf("enqueue %d: %v", next, err)
			}
			next++
		}
	}
	committedEvents := func() int {
		rs, err := hub.Reports(ctx, nodeID)
		if err != nil {
			return -1
		}
		return len(eventIndexes(t, rs))
	}

	// Phase 1: steady state.
	enqueue(50)
	waitFor(t, 10*time.Second, "50 reports committed", func() bool { return committedEvents() == 50 && node.outbox(ctx) == 0 })

	// Phase 2: node goes down; hub queues commands for it.
	node.stop()
	waitFor(t, 5*time.Second, "hub sees node gone", func() bool { return !hub.Connected(nodeID) })
	var cmds []contracts.NodeCommand
	for i := 0; i < 5; i++ {
		c, err := hub.QueueCommand(ctx, nodeID, contracts.CmdAction, map[string]int{"n": i}, nil)
		if err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, c)
	}

	// Phase 3: hub crashes; node restarts and keeps producing durable
	// reports into its local outbox while the hub is unreachable.
	hub.Kill()
	node.start()
	enqueue(100)
	time.Sleep(300 * time.Millisecond)
	if got := node.outbox(ctx); got != 100 {
		t.Fatalf("outbox while hub down = %d, want 100 (local acceptance must be durable)", got)
	}
	if node.Link().Connected() {
		t.Fatal("link claims connected while the hub is down")
	}

	// Phase 4: hub restarts on the same address and data.
	if err := hub.Restart(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, "queued reports drained", func() bool { return committedEvents() == 150 && node.outbox(ctx) == 0 })
	waitFor(t, 15*time.Second, "queued commands executed and reported", func() bool {
		res, err := hub.CommandResults(ctx, nodeID)
		return err == nil && len(res) == len(cmds)
	})
	for _, c := range cmds {
		if n := h.count(c.CommandID); n != 1 {
			t.Fatalf("command %s executed %d times", c.CommandID, n)
		}
	}

	// Phase 5: hub crash in the middle of draining a burst.
	node.stop()
	enqueueOffline := 0
	node.start()
	waitFor(t, 10*time.Second, "reconnected", func() bool { return hub.Connected(nodeID) })
	burst := 2000
	go func() {
		time.Sleep(30 * time.Millisecond)
		hub.Kill()
	}()
	enqueue(burst)
	enqueueOffline += burst
	time.Sleep(200 * time.Millisecond)
	if err := hub.Restart(); err != nil {
		t.Fatal(err)
	}
	total := 150 + enqueueOffline
	waitFor(t, 30*time.Second, "burst drained after mid-drain crash", func() bool { return committedEvents() == total && node.outbox(ctx) == 0 })

	rs, err := hub.Reports(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	assertContiguous(t, rs, len(rs))
	assertEachOnce(t, eventIndexes(t, rs), total)

	nodes, err := hub.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("registry has %d nodes after restarts, want 1", len(nodes))
	}
	n, err := hub.Node(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if n.Connection.State != contracts.ConnConnected {
		t.Fatalf("node state = %s after reconnect", n.Connection.State)
	}
	evidence(t, "reports=%d contiguous, commands=%d each executed once, hub restarts=2, registry nodes=1, dup report deliveries since last restart=%d",
		len(rs), len(cmds), hub.Counters().DuplicateReports)
}

// TestNodeCrashDuringCommandIsIndeterminate (A06 at the node-channel level):
// a command that was executing when the node process died is reported as
// outcome_indeterminate after restart and is never re-executed, and the hub
// does not resend it.
func TestNodeCrashDuringCommandIsIndeterminate(t *testing.T) {
	for _, hard := range []bool{false, true} {
		name := "graceful-shutdown"
		if hard {
			name = "hard-crash"
		}
		t.Run(name, func(t *testing.T) {
			ctx := ctxT(t)
			hub, _ := newHub(t, nil)
			nodeID, tok, err := hub.EnrollNode(ctx, "crash-cmd-node")
			if err != nil {
				t.Fatal(err)
			}
			h := newCountingHandler()
			release := make(chan struct{})
			h.block[contracts.CmdDeliver] = release
			h.ignoreCtx = hard
			node := newNode(t, nodeID, tok, hub.URL, h, nil)
			node.start()
			waitFor(t, 10*time.Second, "connected", func() bool { return hub.Connected(nodeID) })

			cmd, err := hub.QueueCommand(ctx, nodeID, contracts.CmdDeliver, map[string]string{"message_id": "msg_synthetic"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case id := <-h.started:
				if id != cmd.CommandID {
					t.Fatalf("started %s", id)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("command never started")
			}
			if hard {
				wait := node.crash() // journal row stays "executing"
				node.start()         // startup recovery must report it
				close(release)       // the abandoned goroutine finishes against a closed DB
				wait()
			} else {
				node.stop() // handler sees cancellation mid-flight
				node.start()
			}
			waitFor(t, 15*time.Second, "result reported", func() bool {
				res, err := hub.CommandResults(ctx, nodeID)
				return err == nil && len(res) == 1
			})
			res, _ := hub.CommandResults(ctx, nodeID)
			r := res[cmd.CommandID]
			if r.Outcome != contracts.OutcomeIndeterminate || contracts.CodeOf(r.Error) != contracts.ErrOutcomeIndeterminate {
				t.Fatalf("outcome = %s (%+v), want indeterminate", r.Outcome, r.Error)
			}
			time.Sleep(500 * time.Millisecond)
			if n := h.count(cmd.CommandID); n != 1 {
				t.Fatalf("handler invoked %d times; an uncertain command must never be replayed", n)
			}
			evidence(t, "%s mid-command -> outcome=%s (%s), handler invocations=1", name, r.Outcome, r.Error.Message)
		})
	}
}

// TestPartitionBlackholeDetectedAndHealed: a silent partition (packets
// vanish, no reset). Both sides must notice within their heartbeat bounds,
// the hub must record the node disconnected (stale, not idle), work queued
// on both sides during the partition must flow after healing, and
// reconnect attempts during the partition must stay bounded.
func TestPartitionBlackholeDetectedAndHealed(t *testing.T) {
	ctx := ctxT(t)
	hub, _ := newHub(t, nil)
	nodeID, tok, err := hub.EnrollNode(ctx, "partition-node")
	if err != nil {
		t.Fatal(err)
	}
	px, err := faultnet.New(hub.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	h := newCountingHandler()
	node := newNode(t, nodeID, tok, func() string { return "http://" + px.Addr() }, h, func(c *transport.NodeLinkConfig) {
		c.SilenceTimeout = time.Second
	})
	node.start()
	waitFor(t, 10*time.Second, "connected", func() bool { return hub.Connected(nodeID) })
	for i := 0; i < 10; i++ {
		if err := node.enqueue(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 10*time.Second, "pre-partition reports", func() bool { rs, _ := hub.Reports(ctx, nodeID); return len(rs) == 10 })

	px.Partition()
	start := time.Now()
	waitFor(t, 5*time.Second, "hub detects silent node", func() bool { return !hub.Connected(nodeID) })
	hubDetect := time.Since(start)
	waitFor(t, 5*time.Second, "node detects silent hub", func() bool { return !node.Link().Connected() })
	nodeDetect := time.Since(start)
	// The registry is updated when the hub's connection handler unwinds
	// (closing handshake bounded at ~2s), slightly after Connected() flips.
	var n contracts.Node
	waitFor(t, 6*time.Second, "registry marks node disconnected", func() bool {
		n, err = hub.Node(ctx, nodeID)
		return err == nil && n.Connection.State == contracts.ConnDisconnected
	})
	registryDetect := time.Since(start)
	if n.Connection.LastSeenAt == nil || time.Since(*n.Connection.LastSeenAt) < 100*time.Millisecond {
		t.Fatalf("hub view during partition = %+v; want disconnected with a stale last_seen_at", n.Connection)
	}
	acceptedAtPartition := px.Stats().Accepted
	var cmds []contracts.NodeCommand
	for i := 0; i < 3; i++ {
		c, err := hub.QueueCommand(ctx, nodeID, contracts.CmdAction, map[string]int{"i": i}, nil)
		if err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, c)
	}
	for i := 10; i < 40; i++ {
		if err := node.enqueue(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(3 * time.Second) // partition lasts a while
	attempts := px.Stats().Accepted - acceptedAtPartition

	px.Heal()
	waitFor(t, 15*time.Second, "post-heal reports", func() bool {
		rs, _ := hub.Reports(ctx, nodeID)
		return len(eventIndexes(t, rs)) == 40 && node.outbox(ctx) == 0
	})
	waitFor(t, 15*time.Second, "post-heal commands", func() bool {
		res, _ := hub.CommandResults(ctx, nodeID)
		return len(res) == 3
	})
	for _, c := range cmds {
		if h.count(c.CommandID) != 1 {
			t.Fatalf("command %s executed %d times", c.CommandID, h.count(c.CommandID))
		}
	}
	rs, _ := hub.Reports(ctx, nodeID)
	assertEachOnce(t, eventIndexes(t, rs), 40)
	// Each attempt during a blackhole waits DialTimeout/SilenceTimeout or
	// backoff, so 3s of partition must not produce a storm.
	if attempts > 25 {
		t.Fatalf("%d connection attempts during a 3s partition: retry storm", attempts)
	}
	evidence(t, "blackhole: hub dropped conn in %v (hb 200ms x3), registry disconnected in %v, node detected in %v (silence 1s); %d reconnect attempts in 3s partition; 40 reports + 3 commands delivered once after heal",
		hubDetect.Round(time.Millisecond), registryDetect.Round(time.Millisecond), nodeDetect.Round(time.Millisecond), attempts)
}

// frameType extracts the node-protocol frame type from a WebSocket payload.
func frameType(f faultnet.Frame) string {
	if f.Opcode != 1 {
		return ""
	}
	var v struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(f.Payload, &v) != nil {
		return ""
	}
	return v.Type
}

// TestDuplicatedAndDelayedFrames: every report frame from the node and
// every command frame from the hub is delivered twice, some delayed. The
// hub commits each report once; the node executes each command once.
func TestDuplicatedAndDelayedFrames(t *testing.T) {
	ctx := ctxT(t)
	hub, _ := newHub(t, nil)
	nodeID, tok, err := hub.EnrollNode(ctx, "dup-node")
	if err != nil {
		t.Fatal(err)
	}
	px, err := faultnet.New(hub.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	var n atomic.Int64
	px.SetFrameFilter(func(f faultnet.Frame) faultnet.Action {
		switch frameType(f) {
		case "report", "command":
			a := faultnet.Action{Duplicate: true}
			if n.Add(1)%7 == 0 {
				a.Delay = 20 * time.Millisecond
			}
			return a
		}
		return faultnet.Action{}
	})
	h := newCountingHandler()
	node := newNode(t, nodeID, tok, func() string { return "http://" + px.Addr() }, h, nil)
	node.start()
	waitFor(t, 10*time.Second, "connected", func() bool { return hub.Connected(nodeID) })
	var cmds []contracts.NodeCommand
	for i := 0; i < 200; i++ {
		if err := node.enqueue(ctx, i); err != nil {
			t.Fatal(err)
		}
		if i%20 == 0 {
			c, err := hub.QueueCommand(ctx, nodeID, contracts.CmdAction, map[string]int{"i": i}, nil)
			if err != nil {
				t.Fatal(err)
			}
			cmds = append(cmds, c)
		}
	}
	waitFor(t, 20*time.Second, "all reports and results", func() bool {
		rs, _ := hub.Reports(ctx, nodeID)
		res, _ := hub.CommandResults(ctx, nodeID)
		return len(eventIndexes(t, rs)) == 200 && len(res) == len(cmds) && node.outbox(ctx) == 0
	})
	rs, _ := hub.Reports(ctx, nodeID)
	assertContiguous(t, rs, len(rs))
	assertEachOnce(t, eventIndexes(t, rs), 200)
	for _, c := range cmds {
		if h.count(c.CommandID) != 1 {
			t.Fatalf("duplicated command frame executed %d times", h.count(c.CommandID))
		}
	}
	c := hub.Counters()
	if c.DuplicateReports == 0 || px.Stats().Duplicated == 0 {
		t.Fatalf("fault not exercised: %+v %+v", c, px.Stats())
	}
	evidence(t, "duplicated %d frames; hub dropped %d duplicate reports; %d commands executed once each", px.Stats().Duplicated, c.DuplicateReports, len(cmds))
}

// TestLostAcknowledgementsThenCut: the hub's report_ack and the node's
// command_ack are lost and the connection is then cut (the realistic shape
// of "acknowledgement lost": TCP never loses single frames, connections
// die). Reports are resent and deduplicated; the hub does not get a second
// execution of a command it never saw acknowledged.
func TestLostAcknowledgementsThenCut(t *testing.T) {
	ctx := ctxT(t)
	hub, _ := newHub(t, nil)
	nodeID, tok, err := hub.EnrollNode(ctx, "lost-ack-node")
	if err != nil {
		t.Fatal(err)
	}
	px, err := faultnet.New(hub.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	var acks, cmdAcks atomic.Int64
	px.SetFrameFilter(func(f faultnet.Frame) faultnet.Action {
		switch frameType(f) {
		case "report_ack":
			if k := acks.Add(1); k%25 == 0 {
				return faultnet.Action{Drop: true, CutAfter: true}
			}
			return faultnet.Action{Drop: true} // every ack lost until the cut
		case "command_ack":
			if cmdAcks.Add(1) <= 3 {
				return faultnet.Action{Drop: true, CutAfter: true}
			}
		}
		return faultnet.Action{}
	})
	h := newCountingHandler()
	node := newNode(t, nodeID, tok, func() string { return "http://" + px.Addr() }, h, nil)
	node.start()
	waitFor(t, 10*time.Second, "connected", func() bool { return hub.Connected(nodeID) })
	var cmds []contracts.NodeCommand
	for i := 0; i < 100; i++ {
		if err := node.enqueue(ctx, i); err != nil {
			t.Fatal(err)
		}
		if i%25 == 0 {
			c, err := hub.QueueCommand(ctx, nodeID, contracts.CmdAction, map[string]int{"i": i}, nil)
			if err != nil {
				t.Fatal(err)
			}
			cmds = append(cmds, c)
		}
	}
	// Acks are dropped forever, so the outbox only shrinks via reconnect +
	// Welcome.CommittedReportSeq. Stop dropping after the burst so the
	// steady state can converge.
	waitFor(t, 20*time.Second, "reports committed despite lost acks", func() bool {
		rs, _ := hub.Reports(ctx, nodeID)
		return len(eventIndexes(t, rs)) == 100
	})
	px.SetFrameFilter(nil)
	px.CutAll()
	waitFor(t, 20*time.Second, "outbox pruned and commands resulted", func() bool {
		res, _ := hub.CommandResults(ctx, nodeID)
		return node.outbox(ctx) == 0 && len(res) == len(cmds)
	})
	rs, _ := hub.Reports(ctx, nodeID)
	assertEachOnce(t, eventIndexes(t, rs), 100)
	for _, c := range cmds {
		if h.count(c.CommandID) != 1 {
			t.Fatalf("command executed %d times after lost ack", h.count(c.CommandID))
		}
	}
	evidence(t, "dropped %d report_acks and %d command_acks with cuts; %d duplicate report deliveries deduped; commands once each",
		acks.Load(), cmdAcks.Load(), hub.Counters().DuplicateReports)
}

// TestHubCommitFailureForcesResend: the hub cannot commit a report
// (storage error) — it must close the connection without acknowledging, and
// the node must resend from the committed watermark with nothing lost.
func TestHubCommitFailureForcesResend(t *testing.T) {
	ctx := ctxT(t)
	var failed atomic.Int64
	hub, logs := newHub(t, func(o *refhub.Options) {
		o.FailReports = func(_ string, r contracts.NodeReport) error {
			if r.OriginSeq%10 == 5 && failed.Load() < 5 {
				failed.Add(1)
				return contracts.Errorf(contracts.ErrUnavailable, "synthetic disk error")
			}
			return nil
		}
	})
	nodeID, tok, err := hub.EnrollNode(ctx, "commit-fail-node")
	if err != nil {
		t.Fatal(err)
	}
	node := newNode(t, nodeID, tok, hub.URL, newCountingHandler(), nil)
	node.start()
	for i := 0; i < 60; i++ {
		if err := node.enqueue(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 20*time.Second, "all committed", func() bool {
		rs, _ := hub.Reports(ctx, nodeID)
		return len(rs) == 60 && node.outbox(ctx) == 0
	})
	rs, _ := hub.Reports(ctx, nodeID)
	assertContiguous(t, rs, 60)
	if failed.Load() < 5 {
		t.Fatalf("injected only %d failures", failed.Load())
	}
	if !strings.Contains(logs.String(), "closing so the node resends") {
		t.Fatalf("hub did not log the forced resend")
	}
	evidence(t, "%d injected commit failures; 60 reports committed contiguously", failed.Load())
}

// TestNodeOutboxBoundedDuringOutage (storage pressure on the node): with
// the hub unreachable the outbox fills to MaxOutbox and then refuses new
// durable reports with capacity_exhausted (never silently drops); it drains
// and accepts again once the hub returns.
func TestNodeOutboxBoundedDuringOutage(t *testing.T) {
	ctx := ctxT(t)
	hub, _ := newHub(t, nil)
	nodeID, tok, err := hub.EnrollNode(ctx, "outbox-node")
	if err != nil {
		t.Fatal(err)
	}
	hub.Kill()
	node := newNode(t, nodeID, tok, hub.URL, newCountingHandler(), func(c *transport.NodeLinkConfig) { c.MaxOutbox = 10 })
	node.start()
	for i := 0; i < 10; i++ {
		if err := node.enqueue(ctx, i); err != nil {
			t.Fatalf("enqueue %d within bound: %v", i, err)
		}
	}
	err = node.enqueue(ctx, 10)
	if !contracts.IsCode(err, contracts.ErrCapacityExhausted) {
		t.Fatalf("enqueue beyond bound = %v, want capacity_exhausted", err)
	}
	if err := hub.Restart(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, "drained", func() bool { return node.outbox(ctx) == 0 })
	if err := node.enqueue(ctx, 10); err != nil {
		t.Fatalf("enqueue after drain: %v", err)
	}
	waitFor(t, 10*time.Second, "11 committed", func() bool { rs, _ := hub.Reports(ctx, nodeID); return len(rs) == 11 })
	evidence(t, "outbox bound 10 enforced with capacity_exhausted; drained after hub restart")
}

// TestCommandExpiresWhileNodeOffline: a command whose expiry passes while
// the node is unreachable is recorded as failed/expired, never executed.
func TestCommandExpiresWhileNodeOffline(t *testing.T) {
	ctx := ctxT(t)
	hub, _ := newHub(t, nil)
	nodeID, tok, err := hub.EnrollNode(ctx, "expiry-node")
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(200 * time.Millisecond)
	cmd, err := hub.QueueCommand(ctx, nodeID, contracts.CmdStartRun, map[string]string{"synthetic": "start"}, &exp)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	h := newCountingHandler()
	node := newNode(t, nodeID, tok, hub.URL, h, nil)
	node.start()
	waitFor(t, 10*time.Second, "result", func() bool { res, _ := hub.CommandResults(ctx, nodeID); return len(res) == 1 })
	res, _ := hub.CommandResults(ctx, nodeID)
	r := res[cmd.CommandID]
	if r.Outcome != contracts.OutcomeFailed || contracts.CodeOf(r.Error) != contracts.ErrExpired || h.count(cmd.CommandID) != 0 {
		t.Fatalf("expired command: %+v, executions=%d", r, h.count(cmd.CommandID))
	}
	evidence(t, "expired command -> failed/expired, 0 executions")
}
