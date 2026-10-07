package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon/daemontest"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/supervisor"
)

// shortDir: unix socket paths are limited (~104 bytes on macOS).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "pyre-d-")
	if err != nil {
		d = t.TempDir()
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// node is a test daemon over a state dir that survives restarts.
type node struct {
	t   *testing.T
	dir string
	db  *sql.DB
	rep *daemontest.Reporter
	d   *Daemon
	cfg Config
}

func startNode(t *testing.T, dir string, cfg Config) *node {
	t.Helper()
	db, err := sqlitex.Open(filepath.Join(dir, "pyred.db"))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := daemontest.NewReporter(db)
	if err != nil {
		t.Fatal(err)
	}
	cfg.StateDir, cfg.DB, cfg.Reporter = dir, db, rep
	if cfg.NodeID == "" {
		cfg.NodeID = "node_test"
	}
	if cfg.Poll.Interval == 0 {
		cfg.Poll.Interval = time.Hour // tests drive polling with PollNow unless they opt in
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = time.Hour
	}
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	n := &node{t: t, dir: dir, db: db, rep: rep, d: d, cfg: cfg}
	t.Cleanup(n.stop)
	return n
}

func (n *node) stop() {
	if n.d != nil {
		n.d.Close()
		n.d = nil
	}
	if n.db != nil {
		n.db.Close()
		n.db = nil
	}
}

// restart simulates a pyred restart (runs keep running in the adapter).
func (n *node) restart() *node {
	n.t.Helper()
	n.stop()
	cfg := n.cfg
	cfg.DB, cfg.Reporter = nil, nil
	return startNode(n.t, n.dir, cfg)
}

var cmdSeq atomic.Int64

func command(kind contracts.CommandKind, payload any) contracts.NodeCommand {
	b, _ := json.Marshal(payload)
	return contracts.NodeCommand{CommandID: contracts.NewID(contracts.PrefixCommand), Seq: cmdSeq.Add(1), Kind: kind,
		Actor: contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_test"}, IssuedAt: time.Now(), Payload: b}
}

func (n *node) do(cmd contracts.NodeCommand) contracts.CommandResult {
	n.t.Helper()
	res, err := n.d.HandleCommand(context.Background(), cmd)
	if err != nil {
		n.t.Fatalf("HandleCommand(%s): %v", cmd.Kind, err)
	}
	return res
}

func startCmd(session, run string, h contracts.Harness, cwd string) contracts.StartRunCommand {
	return contracts.StartRunCommand{Session: contracts.Session{SessionID: session, Harness: h, Title: "test " + session, CreatedAt: time.Now()},
		RunID: run, Cwd: cwd, AccountPolicy: contracts.AccountPolicy{Mode: contracts.AccountEligible}}
}

// start launches a run and waits for the async launch.
func (n *node) start(sc contracts.StartRunCommand) contracts.Run {
	n.t.Helper()
	res := n.do(command(contracts.CmdStartRun, sc))
	if res.Error != nil {
		n.t.Fatalf("start_run: %v", res.Error)
	}
	n.d.WaitLaunches()
	if _, live := n.d.getLive(sc.RunID); live {
		waitFor(n.t, "first observation of "+sc.RunID, func() bool {
			n.d.PollNow(context.Background(), sc.RunID)
			return n.run(sc.RunID).Run.State != contracts.RunStarting
		})
	}
	rec, ok, err := n.d.st.run(context.Background(), sc.RunID)
	if err != nil || !ok {
		n.t.Fatalf("run record: %v %v", ok, err)
	}
	return rec.Run
}

func (n *node) run(id string) runRecord {
	n.t.Helper()
	if lr, ok := n.d.getLive(id); ok {
		r, _ := lr.snapshot()
		return r
	}
	r, ok, err := n.d.st.run(context.Background(), id)
	if err != nil || !ok {
		n.t.Fatalf("no run %s: %v", id, err)
	}
	return r
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// countingAdapter counts Launch/Observe calls and concurrent Observes.
type countingAdapter struct {
	contracts.RuntimeAdapter
	launches  atomic.Int64
	observes  atomic.Int64
	inflight  atomic.Int64
	maxInflt  atomic.Int64
	observeIn time.Duration
}

func (c *countingAdapter) Launch(ctx context.Context, l contracts.ResolvedLaunch) (contracts.RunHandle, error) {
	c.launches.Add(1)
	return c.RuntimeAdapter.Launch(ctx, l)
}

func (c *countingAdapter) Observe(ctx context.Context, h contracts.RunHandle, cur string) (contracts.Observation, error) {
	c.observes.Add(1)
	n := c.inflight.Add(1)
	defer c.inflight.Add(-1)
	for {
		m := c.maxInflt.Load()
		if n <= m || c.maxInflt.CompareAndSwap(m, n) {
			break
		}
	}
	if c.observeIn > 0 {
		time.Sleep(c.observeIn)
	}
	return c.RuntimeAdapter.Observe(ctx, h, cur)
}

// tmuxShell is a minimal real-tmux plain-shell adapter standing in for
// internal/runtime/shell (WP07): persistent attach/detach, stop, screen
// read; no agent input capabilities.
type tmuxShell struct{}

func (s *tmuxShell) Name() string               { return "shell-tmux-test" }
func (s *tmuxShell) Harness() contracts.Harness { return contracts.HarnessShell }
func (s *tmuxShell) caps() []string {
	return []string{contracts.CapAttachNative, contracts.CapAttachReadOnly, contracts.CapStop, contracts.CapScreen}
}
func (s *tmuxShell) Probe(context.Context) (contracts.ProbeResult, error) {
	return contracts.ProbeResult{Harness: contracts.HarnessShell, Adapter: s.Name(), Version: "test", Executable: "/bin/sh", Available: true, Capabilities: s.caps()}, nil
}
func (s *tmuxShell) tm(h contracts.RunHandle) supervisor.Tmux {
	return supervisor.Tmux{Socket: h.TmuxSocket, OwnedRoot: filepath.Dir(filepath.Dir(h.StateDir))}
}
func (s *tmuxShell) Launch(ctx context.Context, l contracts.ResolvedLaunch) (contracts.RunHandle, error) {
	h := contracts.RunHandle{RunID: l.RunID, SessionID: l.SessionID, Adapter: s.Name(), Harness: contracts.HarnessShell,
		TmuxSocket: filepath.Join(l.StateDir, "tmux.sock"), TmuxTarget: "pyre", StateDir: l.StateDir}
	err := s.tm(h).NewSession(ctx, supervisor.SessionSpec{Name: "pyre", Cwd: l.Cwd, Env: l.Env, Argv: []string{"/bin/sh"}, RemainOnExit: true})
	return h, err
}
func (s *tmuxShell) Recover(ctx context.Context, h contracts.RunHandle) (contracts.RunHandle, error) {
	ok, err := s.tm(h).HasSession(ctx, h.TmuxTarget)
	if err != nil {
		return h, err
	}
	if !ok {
		return h, contracts.Errorf(contracts.ErrNotFound, "tmux session gone")
	}
	return h, nil
}
func (s *tmuxShell) Observe(ctx context.Context, h contracts.RunHandle, cur string) (contracts.Observation, error) {
	if _, err := s.Recover(ctx, h); err != nil {
		return contracts.Observation{}, err
	}
	p, err := s.tm(h).Pane(ctx, h.TmuxTarget)
	if err != nil {
		return contracts.Observation{}, err
	}
	if p.Dead {
		code := p.ExitStatus
		return contracts.Observation{State: contracts.RunExited, ExitCode: &code, ObservedAt: time.Now()}, nil
	}
	return contracts.Observation{State: contracts.RunWaitingInput, StateReason: "shell (no turn concept)", ObservedAt: time.Now()}, nil
}
func (s *tmuxShell) Deliver(context.Context, contracts.RunHandle, contracts.DeliveryInput) (contracts.DeliveryResult, error) {
	return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrUnsupported, "plain shell has no structured input")
}
func (s *tmuxShell) Reconcile(context.Context, contracts.RunHandle, string) (contracts.DeliveryResult, error) {
	return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrUnsupported, "plain shell has no input evidence")
}
func (s *tmuxShell) Attach(ctx context.Context, h contracts.RunHandle, req contracts.AttachCommand) (contracts.AttachDescriptor, error) {
	return contracts.AttachDescriptor{RunID: h.RunID, Mechanism: contracts.AttachLocalTmux, Command: s.tm(h).AttachArgv(h.TmuxTarget, req.ReadOnly), ReadOnly: req.ReadOnly}, nil
}
func (s *tmuxShell) Interrupt(context.Context, contracts.RunHandle) (contracts.CommandReceipt, error) {
	return contracts.CommandReceipt{}, contracts.Errorf(contracts.ErrUnsupported, "interrupt is an agent action")
}
func (s *tmuxShell) Stop(ctx context.Context, h contracts.RunHandle) (contracts.CommandReceipt, error) {
	if err := s.tm(h).Kill(ctx); err != nil {
		return contracts.CommandReceipt{}, err
	}
	return contracts.CommandReceipt{Accepted: true, Outcome: contracts.OutcomeSucceeded, ObservedAt: time.Now()}, nil
}
func (s *tmuxShell) Answer(context.Context, contracts.RunHandle, contracts.Approval, string, string) (contracts.CommandReceipt, error) {
	return contracts.CommandReceipt{}, contracts.Errorf(contracts.ErrUnsupported, "no approvals in a shell")
}
func (s *tmuxShell) ReadTranscript(ctx context.Context, h contracts.RunHandle, req contracts.ReadSessionRequest) (contracts.ReadSessionResponse, error) {
	scr, err := s.tm(h).Capture(ctx, h.TmuxTarget, 0)
	return contracts.ReadSessionResponse{Screen: scr, Freshness: contracts.Freshness{ObservedAt: time.Now(), Source: "tmux"}}, err
}
func (s *tmuxShell) ResumeSpec(context.Context, contracts.Run, bool) (contracts.ContinuationKind, *contracts.NativeRef, error) {
	return "", nil, contracts.Errorf(contracts.ErrUnsupported, "shell cannot resume")
}

var _ contracts.RuntimeAdapter = (*tmuxShell)(nil)
