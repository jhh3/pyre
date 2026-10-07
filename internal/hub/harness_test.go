package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// ---- fakes ----

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeChannel is a NodeChannel that records sent commands.
type fakeChannel struct {
	mu        sync.Mutex
	connected map[string]bool
	failSend  map[string]bool
	sent      map[string][]contracts.NodeCommand
	onSend    func(nodeID string, cmd contracts.NodeCommand)
}

func newFakeChannel() *fakeChannel {
	return &fakeChannel{connected: map[string]bool{}, failSend: map[string]bool{}, sent: map[string][]contracts.NodeCommand{}}
}

func (f *fakeChannel) Send(ctx context.Context, nodeID string, cmd contracts.NodeCommand) error {
	f.mu.Lock()
	if !f.connected[nodeID] || f.failSend[nodeID] {
		f.mu.Unlock()
		return contracts.Errorf(contracts.ErrUnavailable, "not connected")
	}
	f.sent[nodeID] = append(f.sent[nodeID], cmd)
	hook := f.onSend
	f.mu.Unlock()
	if hook != nil {
		hook(nodeID, cmd)
	}
	return nil
}

func (f *fakeChannel) Connected(nodeID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected[nodeID]
}

func (f *fakeChannel) setConnected(nodeID string, c bool) {
	f.mu.Lock()
	f.connected[nodeID] = c
	f.mu.Unlock()
}

func (f *fakeChannel) sentTo(nodeID string) []contracts.NodeCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]contracts.NodeCommand{}, f.sent[nodeID]...)
}

func (f *fakeChannel) sentKinds(nodeID string, kind contracts.CommandKind) []contracts.NodeCommand {
	var out []contracts.NodeCommand
	for _, c := range f.sentTo(nodeID) {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

type fakeTokens struct {
	mu      sync.Mutex
	issued  int
	revoked []string
}

func (t *fakeTokens) IssueSessionToken(ctx context.Context, sessionID string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.issued++
	return "synthetic-session-token-" + sessionID, nil
}
func (t *fakeTokens) RevokeSessionTokens(ctx context.Context, sessionID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.revoked = append(t.revoked, sessionID)
	return nil
}
func (t *fakeTokens) IssueTicket(ctx context.Context, purpose string, ref contracts.ObjectRef, ttl time.Duration) (string, time.Time, error) {
	return "synthetic-ticket", time.Now().Add(ttl), nil
}
func (t *fakeTokens) RedeemTicket(ctx context.Context, ticket, purpose string) (contracts.ObjectRef, error) {
	return contracts.ObjectRef{}, contracts.Errorf(contracts.ErrUnsupported, "fake")
}

// ---- harness ----

type harness struct {
	t      *testing.T
	path   string
	db     *sql.DB
	h      *Hub
	ch     *fakeChannel
	tokens *fakeTokens
	clock  *fakeClock
	seq    map[string]uint64
	opts   Options
	fleet  contracts.FleetAPI // optional fleet registry (nil: none)
}

var (
	owner   = contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synth-laptop", Name: "owner (synthetic)"}
	phone   = contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synth-android", Name: "owner phone (synthetic)"}
	nodeMac = "node_synth-mac"
	nodeLin = "node_synth-linux"
)

var claudeCaps = []string{
	contracts.CapAttachNative, contracts.CapAttachReadOnly, contracts.CapInputQueue, contracts.CapInputReconcile, contracts.CapApprovals,
	contracts.CapInterrupt, contracts.CapStop, contracts.CapResumeNative, contracts.CapResumeHandoff, contracts.CapTranscript, contracts.CapScreen,
}
var shellCaps = []string{contracts.CapAttachNative, contracts.CapAttachReadOnly, contracts.CapStop, contracts.CapScreen}

func newHarness(t *testing.T) *harness {
	t.Helper()
	x := &harness{t: t, path: filepath.Join(t.TempDir(), "hub.db"), ch: newFakeChannel(), tokens: &fakeTokens{},
		clock: &fakeClock{t: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}, seq: map[string]uint64{},
		opts: Options{CommandWait: 2 * time.Second, ControlWait: 200 * time.Millisecond, PollInterval: 20 * time.Millisecond}}
	x.open()
	t.Cleanup(func() { x.h.Close(); x.db.Close() })
	return x
}

func (x *harness) open() {
	x.t.Helper()
	db, err := sqlitex.Open(x.path)
	if err != nil {
		x.t.Fatal(err)
	}
	h, err := New(Config{DB: db, Clock: x.clock, Nodes: x.ch, Tokens: x.tokens, Options: x.opts, Fleet: x.fleet})
	if err != nil {
		x.t.Fatal(err)
	}
	x.db, x.h = db, h
}

// restart simulates a hub crash/restart: close everything and reopen the
// same database file. Node connections drop.
func (x *harness) restart() {
	x.t.Helper()
	x.h.Close()
	x.db.Close()
	for n := range x.ch.connected {
		x.ch.setConnected(n, false)
	}
	x.open()
}

func as(p contracts.Principal) context.Context {
	return contracts.WithPrincipal(context.Background(), p)
}

func nodeCtx(id string) context.Context {
	return as(contracts.Principal{Kind: contracts.PrincipalNode, ID: id})
}

func helloFor(nodeID string, maxRuns int, live ...contracts.Run) contracts.NodeHello {
	return contracts.NodeHello{
		NodeID: nodeID, Protocol: 1, MinProtocol: 1, PyredVersion: "0.1.0-synth", Hostname: nodeID[5:], Platform: "linux", Arch: "amd64",
		Harnesses: []contracts.HarnessInfo{
			{Harness: contracts.HarnessClaude, Version: "2.1.289", Available: true, Capabilities: claudeCaps},
			{Harness: contracts.HarnessCodex, Available: false, Reason: "codex executable not found on node PATH"},
			{Harness: contracts.HarnessShell, Version: "tmux 3.5a", Available: true, Capabilities: shellCaps},
		},
		Resources: contracts.NodeResources{CPUCount: 8, MaxRuns: maxRuns},
		LiveRuns:  live,
	}
}

// connect performs hello + ResumeNode, like the transport.
func (x *harness) connect(nodeID string, lastCmdSeq int64, live ...contracts.Run) contracts.NodeWelcome {
	x.t.Helper()
	x.ch.setConnected(nodeID, true)
	hello := helloFor(nodeID, 4, live...)
	hello.LastCommandSeq = lastCmdSeq
	w, err := x.h.HandleHello(nodeCtx(nodeID), nodeID, hello)
	if err != nil {
		x.t.Fatalf("hello %s: %v", nodeID, err)
	}
	// Like the transport: pending commands follow the Welcome.
	cmds, err := x.h.WelcomeCommands(context.Background(), nodeID)
	if err != nil {
		x.t.Fatal(err)
	}
	for _, c := range cmds {
		x.ch.mu.Lock()
		x.ch.sent[nodeID] = append(x.ch.sent[nodeID], c)
		x.ch.mu.Unlock()
	}
	return w
}

func (x *harness) disconnect(nodeID string) {
	x.t.Helper()
	x.ch.setConnected(nodeID, false)
	if err := x.h.HandleDisconnect(nodeCtx(nodeID), nodeID); err != nil {
		x.t.Fatal(err)
	}
}

func (x *harness) report(nodeID string, kind contracts.ReportKind, payload any) contracts.ReportAck {
	x.t.Helper()
	x.seq[nodeID]++
	return x.reportSeq(nodeID, x.seq[nodeID], kind, payload)
}

func (x *harness) reportSeq(nodeID string, seq uint64, kind contracts.ReportKind, payload any) contracts.ReportAck {
	x.t.Helper()
	b, _ := json.Marshal(payload)
	ack, err := x.h.HandleReport(nodeCtx(nodeID), nodeID, contracts.NodeReport{OriginSeq: seq, Kind: kind, At: x.clock.Now(), Payload: b})
	if err != nil {
		x.t.Fatalf("report %s#%d: %v", kind, seq, err)
	}
	return ack
}

func (x *harness) ack(nodeID string, cmd contracts.NodeCommand) {
	x.t.Helper()
	if err := x.h.HandleCommandAck(nodeCtx(nodeID), nodeID, contracts.CommandAck{CommandID: cmd.CommandID, Seq: cmd.Seq}); err != nil {
		x.t.Fatal(err)
	}
}

func (x *harness) result(nodeID string, cmd contracts.NodeCommand, outcome contracts.CommandOutcome, result any) {
	x.t.Helper()
	var raw contracts.RawJSON
	if result != nil {
		raw, _ = json.Marshal(result)
	}
	x.report(nodeID, contracts.ReportCommandResult, contracts.CommandResult{CommandID: cmd.CommandID, Kind: cmd.Kind, Outcome: outcome, Result: raw, At: x.clock.Now()})
}

// start launches a claude session on nodeID and completes the launch with a
// running run. Returns the session view after launch.
func (x *harness) start(p contracts.Principal, op, nodeID string) contracts.SessionView {
	x.t.Helper()
	resp, err := x.h.StartSession(as(p), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: op}, NodeID: nodeID, Harness: contracts.HarnessClaude, Cwd: "/home/synth/work"})
	if err != nil {
		x.t.Fatalf("start: %v", err)
	}
	cmds := x.ch.sentKinds(nodeID, contracts.CmdStartRun)
	cmd := cmds[len(cmds)-1]
	x.ack(nodeID, cmd)
	run := *resp.Session.Run
	run.State = contracts.RunWaitingInput
	run.Adapter = "claude-synth"
	x.result(nodeID, cmd, contracts.OutcomeSucceeded, run)
	v, err := x.h.GetSession(as(owner), contracts.GetRequest{ID: resp.Session.Session.SessionID})
	if err != nil {
		x.t.Fatal(err)
	}
	return v
}

func (x *harness) session(id string) contracts.SessionView {
	x.t.Helper()
	v, err := x.h.GetSession(as(owner), contracts.GetRequest{ID: id})
	if err != nil {
		x.t.Fatal(err)
	}
	return v
}

func (x *harness) msg(id string) contracts.MessageView {
	x.t.Helper()
	v, err := x.h.GetMessage(as(owner), contracts.GetRequest{ID: id})
	if err != nil {
		x.t.Fatal(err)
	}
	return v
}

func (x *harness) storedRunState(runID string) contracts.RunState {
	var st string
	x.db.QueryRow(`SELECT state FROM runs WHERE run_id=?`, runID).Scan(&st)
	return contracts.RunState(st)
}

func (x *harness) countEvents(kind contracts.EventKind) int {
	var n int
	x.db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind=?`, string(kind)).Scan(&n)
	return n
}

func sesPrincipal(id string) contracts.Principal {
	return contracts.Principal{Kind: contracts.PrincipalSession, ID: id}
}

func wantCode(t *testing.T, err error, code contracts.ErrorCode) {
	t.Helper()
	if !contracts.IsCode(err, code) {
		t.Fatalf("want %s, got %v", code, err)
	}
}
