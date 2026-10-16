package hub

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/terminal"
)

type fakeRelay struct {
	mu     sync.Mutex
	grants []string // principal|run|ro
}

func (f *fakeRelay) IssueTerminalTicket(ctx context.Context, p contracts.Principal, runID string, readOnly bool) (string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ro := "rw"
	if readOnly {
		ro = "ro"
	}
	f.grants = append(f.grants, p.ID+"|"+runID+"|"+ro)
	return "synthetic-relay-ticket", time.Now().Add(time.Minute), nil
}

func TestWebTerminalAttachAndBackend(t *testing.T) {
	x := newHarness(t)
	relay := &fakeRelay{}
	x.h.cfg.Terminal = relay
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_1", nodeMac)
	runID := v.Run.RunID
	var mu sync.Mutex
	var lastAttach contracts.AttachCommand
	x.ch.onSend = func(node string, cmd contracts.NodeCommand) {
		if cmd.Kind == contracts.CmdAttach {
			var ac contracts.AttachCommand
			json.Unmarshal(cmd.Payload, &ac)
			mu.Lock()
			lastAttach = ac
			mu.Unlock()
			go x.result(node, cmd, contracts.OutcomeSucceeded, contracts.AttachDescriptor{Mechanism: contracts.AttachLocalTmux,
				Command: []string{"tmux", "-S", "/synthetic/tmux.sock", "attach-session", "-t", "pyre"}, ReadOnly: ac.ReadOnly})
		}
	}
	last := func() contracts.AttachCommand { mu.Lock(); defer mu.Unlock(); return lastAttach }
	attach := func(p contracts.Principal, readOnly bool, pref ...contracts.AttachMechanism) (contracts.AttachDescriptor, error) {
		return x.h.AttachSession(as(p), contracts.AttachRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
			SessionID: v.Session.SessionID, ClientNodeID: "node_synth-phone", Preferred: pref, ReadOnly: readOnly})
	}

	d, err := attach(owner, false, contracts.AttachWebTerminal, contracts.AttachSSHTmux)
	if err != nil {
		t.Fatal(err)
	}
	if d.Mechanism != contracts.AttachWebTerminal || d.URL != contracts.PathTerminal || d.Ticket != "synthetic-relay-ticket" || d.Command != nil || d.ReadOnly || d.RunID != runID {
		t.Fatalf("web descriptor = %+v", d)
	}
	// The relay's ticket needs no node round trip (terminal_open asks the
	// node when the client dials).
	if n := len(x.ch.sentKinds(nodeMac, contracts.CmdAttach)); n != 0 {
		t.Fatalf("web attach of a native-terminal run sent %d attach commands to the node", n)
	}
	// A run without a native terminal: the node is asked for its local
	// descriptor only (client = run's node, local_tmux).
	if _, err := x.db.Exec(`UPDATE runs SET run=json_set(run, '$.capabilities', json('["transcript.read","attach.web_terminal"]')) WHERE run_id=?`, runID); err != nil {
		t.Fatal(err)
	}
	if d, err = attach(owner, false, contracts.AttachWebTerminal, contracts.AttachSSHTmux); err != nil || d.Mechanism != contracts.AttachWebTerminal {
		t.Fatalf("web attach via the node = %+v %v", d, err)
	}
	if la := last(); la.ClientNodeID != "node_synth-mac" || len(la.Preferred) != 1 || la.Preferred[0] != contracts.AttachLocalTmux {
		t.Fatalf("relay must ask the node for its local descriptor only (client = run's node, local_tmux): %+v", la)
	}
	if _, err := x.db.Exec(`UPDATE runs SET run=json_set(run, '$.capabilities', json(?)) WHERE run_id=?`, string(mustMarshal(claudeCaps)), runID); err != nil {
		t.Fatal(err)
	}
	relay.mu.Lock()
	relay.grants = relay.grants[:1]
	relay.mu.Unlock()
	// A client preferring a native terminal keeps the node's descriptor.
	d, err = attach(owner, false, contracts.AttachLocalTmux, contracts.AttachWebTerminal)
	if err != nil || d.Mechanism != contracts.AttachLocalTmux || d.Ticket != "" || last().ClientNodeID != "node_synth-phone" {
		t.Fatalf("native descriptor = %+v %v", d, err)
	}

	// Input lease: another holder blocks an interactive web terminal only.
	if _, err := x.h.AcquireLease(as(phone), contracts.LeaseRequest{Mutation: contracts.Mutation{OperationID: "op_lease"}, RunID: runID}); err != nil {
		t.Fatal(err)
	}
	_, err = attach(owner, false, contracts.AttachWebTerminal)
	wantCode(t, err, contracts.ErrConflict)
	if d, err = attach(owner, true, contracts.AttachWebTerminal); err != nil || !d.ReadOnly || d.Mechanism != contracts.AttachWebTerminal || d.Lease == nil {
		t.Fatalf("read-only web attach = %+v %v", d, err)
	}
	if d, err = attach(phone, false, contracts.AttachWebTerminal); err != nil || d.ReadOnly {
		t.Fatalf("lease holder web attach = %+v %v", d, err)
	}
	relay.mu.Lock()
	grants := append([]string{}, relay.grants...)
	relay.mu.Unlock()
	want := []string{owner.ID + "|" + runID + "|rw", owner.ID + "|" + runID + "|ro", phone.ID + "|" + runID + "|rw"}
	if len(grants) != len(want) {
		t.Fatalf("grants = %v", grants)
	}
	for i := range want {
		if grants[i] != want[i] {
			t.Fatalf("grants = %v, want %v", grants, want)
		}
	}

	// Backend: prepare / input checks follow the lease.
	_, err = x.h.PrepareTerminal(context.Background(), owner, runID, false)
	wantCode(t, err, contracts.ErrConflict)
	if node, err := x.h.PrepareTerminal(context.Background(), owner, runID, true); err != nil || node != nodeMac {
		t.Fatalf("prepare read-only = %q %v", node, err)
	}
	if err := x.h.TerminalInputAllowed(context.Background(), phone, runID); err != nil {
		t.Fatal(err)
	}
	wantCode(t, x.h.TerminalInputAllowed(context.Background(), owner, runID), contracts.ErrConflict)

	// terminal_open rides the durable node queue with a short expiry.
	x.ch.onSend = nil
	tc := contracts.TerminalOpenCommand{StreamID: "tstr_synthetic", RunID: runID, Cols: 80, Rows: 24}
	if _, err := x.h.QueueTerminalOpen(context.Background(), phone, nodeLin, tc, 15*time.Second); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("queue for the wrong node: %v", err)
	}
	id, err := x.h.QueueTerminalOpen(context.Background(), phone, nodeMac, tc, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sent := x.ch.sentKinds(nodeMac, terminal.CmdTerminalOpen)
	if len(sent) != 1 || sent[0].CommandID != id || sent[0].ExpiresAt == nil || sent[0].ExpiresAt.Sub(sent[0].IssuedAt) != 15*time.Second {
		t.Fatalf("sent = %+v", sent)
	}
	var got contracts.TerminalOpenCommand
	if json.Unmarshal(sent[0].Payload, &got) != nil || got != tc {
		t.Fatalf("payload = %s", sent[0].Payload)
	}
	if done, err := x.h.TerminalOpenResult(context.Background(), id, 50*time.Millisecond); done || err != nil {
		t.Fatalf("pending result = %v %v", done, err)
	}
	b, _ := json.Marshal(contracts.CommandResult{CommandID: id, Kind: terminal.CmdTerminalOpen, Outcome: contracts.OutcomeFailed,
		Error: contracts.Errorf(contracts.ErrNotFound, "run not on this node"), At: x.clock.Now()})
	x.seq[nodeMac]++
	if _, err := x.h.HandleReport(nodeCtx(nodeMac), nodeMac, contracts.NodeReport{OriginSeq: x.seq[nodeMac], Kind: contracts.ReportCommandResult, At: x.clock.Now(), Payload: b}); err != nil {
		t.Fatal(err)
	}
	done, err := x.h.TerminalOpenResult(context.Background(), id, time.Second)
	if !done || !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("failed result = %v %v", done, err)
	}

	// An offline node cannot serve a stream.
	x.disconnect(nodeMac)
	_, err = x.h.PrepareTerminal(context.Background(), phone, runID, false)
	wantCode(t, err, contracts.ErrUnavailable)
}

func TestPrefersWebTerminal(t *testing.T) {
	cases := []struct {
		pref []contracts.AttachMechanism
		want bool
	}{
		{nil, false},
		{[]contracts.AttachMechanism{contracts.AttachWebTerminal}, true},
		{[]contracts.AttachMechanism{"future_mechanism", contracts.AttachWebTerminal, contracts.AttachSSHTmux}, true},
		{[]contracts.AttachMechanism{contracts.AttachSSHTmux, contracts.AttachWebTerminal}, false},
		{[]contracts.AttachMechanism{contracts.AttachReadOnly, contracts.AttachWebTerminal}, false},
	}
	for _, c := range cases {
		if got := prefersWebTerminal(c.pref); got != c.want {
			t.Errorf("prefersWebTerminal(%v) = %v", c.pref, got)
		}
	}
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
