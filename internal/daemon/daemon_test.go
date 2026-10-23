package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon/daemontest"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

func TestRestartRecoversRunsWithoutRelaunch(t *testing.T) {
	dir := t.TempDir()
	ad := &countingAdapter{RuntimeAdapter: mock.NewAdapter()}
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{ad}})
	cwd := t.TempDir()
	r1 := n.start(startCmd("ses_a", "run_a1", contracts.HarnessMock, cwd))
	n.start(startCmd("ses_b", "run_b1", contracts.HarnessMock, cwd))
	if r1.NodeID != "node_test" || r1.Adapter != "mock" || r1.AdapterData["pyre.account"] != "ambient" {
		t.Fatalf("run = %+v", r1)
	}
	if got := n.run("run_a1"); got.Phase != phaseLive || got.Handle == nil {
		t.Fatalf("after launch = %+v", got)
	}
	// pyred stops; run_b1's process disappears meanwhile.
	n.stop()
	ad.RuntimeAdapter.(*mock.Adapter).Vanish("run_b1")

	n = n.restart()
	a := n.run("run_a1")
	if a.Phase != phaseLive || a.Run.State.Terminal() {
		t.Fatalf("recovered run = %+v", a.Run)
	}
	b := n.run("run_b1")
	if b.Phase != phaseEnded || b.Run.State != contracts.RunExited || !strings.Contains(b.Run.StateReason, "not found") {
		t.Fatalf("vanished run = %+v / %s", b.Run, b.Phase)
	}
	// run_b1 was a live session whose process died underneath pyred: it is
	// never relaunched in place, but pyred resumes the session natively in a
	// new run (crashresume.go). The live run_a1 is only re-attached.
	var b2 runRecord
	waitFor(t, "crash resume of ses_b", func() bool {
		b2 = n.latestRun("ses_b")
		return b2.Run.RunID != "run_b1" && b2.Phase == phaseLive
	})
	if b2.Run.PreviousRunID != "run_b1" || b2.Run.ContinuationKind != contracts.ContinuationNativeResume {
		t.Fatalf("ses_b continuation = %+v", b2.Run)
	}
	n.d.WaitLaunches()
	if ad.launches.Load() != 3 {
		t.Fatalf("launches = %d, want 3 (run_a1 re-attached, not relaunched; ses_b resumed once)", ad.launches.Load())
	}
	// A re-sent start_run for the live run returns it; never a second process.
	res := n.do(command(contracts.CmdStartRun, startCmd("ses_a", "run_a1", contracts.HarnessMock, cwd)))
	if res.Error != nil || res.Outcome != contracts.OutcomeSucceeded {
		t.Fatalf("resent start = %+v", res)
	}
	n.d.WaitLaunches()
	if ad.launches.Load() != 3 {
		t.Fatalf("launches after resend = %d", ad.launches.Load())
	}
	hello, err := n.d.Hello(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var liveIDs []string
	for _, r := range hello.LiveRuns {
		liveIDs = append(liveIDs, r.RunID)
	}
	if len(liveIDs) != 2 || !slices.Contains(liveIDs, "run_a1") || !slices.Contains(liveIDs, b2.Run.RunID) || hello.NodeID != "node_test" {
		t.Fatalf("hello live runs = %+v", hello.LiveRuns)
	}
	// Observation keeps working on the recovered run.
	n.d.PollNow(context.Background(), "run_a1")
	if s := n.run("run_a1").Run.State; s != contracts.RunWaitingInput {
		t.Fatalf("observed state = %s", s)
	}
}

func TestDuplicateStartRunIsOneRun(t *testing.T) {
	ad := &countingAdapter{RuntimeAdapter: mock.NewAdapter()}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{ad}})
	cwd := t.TempDir()
	sc := startCmd("ses_a", "run_1", contracts.HarnessMock, cwd)
	cmd := command(contracts.CmdStartRun, sc)
	first := n.do(cmd)
	if first.Outcome != contracts.OutcomeDelivered {
		t.Fatalf("first = %+v", first)
	}
	n.do(cmd)                                // transport duplicate (same command ID)
	n.do(command(contracts.CmdStartRun, sc)) // re-issued command (new ID), same run
	again := n.do(command(contracts.CmdStartRun, sc))
	n.d.WaitLaunches()
	if ad.launches.Load() != 1 {
		t.Fatalf("launches = %d, want 1", ad.launches.Load())
	}
	var run contracts.Run
	json.Unmarshal(again.Result, &run)
	if run.RunID != "run_1" {
		t.Fatalf("duplicate result = %s", again.Result)
	}
	// A different run for the same live session is refused.
	other := n.do(command(contracts.CmdStartRun, startCmd("ses_a", "run_2", contracts.HarnessMock, cwd)))
	if other.Error == nil || other.Error.Code != contracts.ErrConflict {
		t.Fatalf("second live run = %+v", other)
	}
	// Final command result reported durably after launch.
	var finals int
	for _, r := range n.rep.Reports(contracts.ReportCommandResult) {
		var cr contracts.CommandResult
		json.Unmarshal(r.Payload, &cr)
		if cr.CommandID == cmd.CommandID && cr.Outcome == contracts.OutcomeSucceeded {
			finals++
		}
	}
	if finals != 1 {
		t.Fatalf("final command results = %d", finals)
	}
}

func TestOrphanedLaunchIsNeverRelaunched(t *testing.T) {
	dir := t.TempDir()
	ad := &countingAdapter{RuntimeAdapter: mock.NewAdapter()}
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{ad}})
	// Simulate a crash after the launching record committed and after the
	// adapter was invoked (launch marker present), before Launch returned.
	rec := runRecord{Session: contracts.Session{SessionID: "ses_o", Harness: contracts.HarnessMock}, Phase: phaseLaunching,
		Run: contracts.Run{RunID: "run_o", SessionID: "ses_o", Adapter: "mock", Harness: contracts.HarnessMock, State: contracts.RunStarting}}
	if err := sqlitex.Tx(context.Background(), n.db, func(tx *sql.Tx) error { return putRunTx(context.Background(), tx, rec, time.Now()) }); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(n.d.runDir("run_o"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeLaunchMarker(n.d.runDir("run_o"), time.Now()); err != nil {
		t.Fatal(err)
	}
	n = n.restart()
	got := n.run("run_o")
	if got.Phase != phaseOrphaned || got.Run.State != contracts.RunUnknown || got.Run.AdapterData[AdapterDataNoHandle] != "true" {
		t.Fatalf("orphan = %+v %s", got.Run, got.Phase)
	}
	n.do(command(contracts.CmdStartRun, startCmd("ses_o", "run_o", contracts.HarnessMock, t.TempDir())))
	n.d.WaitLaunches()
	if ad.launches.Load() != 0 {
		t.Fatal("orphaned run was relaunched")
	}
	// The session cannot get a second current run while the orphan is unresolved.
	res := n.do(command(contracts.CmdStartRun, startCmd("ses_o", "run_o2", contracts.HarnessMock, t.TempDir())))
	if res.Error == nil || res.Error.Code != contracts.ErrConflict {
		t.Fatalf("second run beside orphan = %+v", res)
	}
}

// W-resilience: a launch interrupted before the adapter was invoked (no
// marker, no process, no tmux session) is provably never started: it fails
// with a relaunch hint, is never relaunched by Pyre, and an explicit resume
// relaunches it fresh with the original prompt.
func TestNeverStartedLaunchFailsAndRelaunches(t *testing.T) {
	old := runProcessFound
	runProcessFound = func(string) (bool, bool) { return false, true }
	t.Cleanup(func() { runProcessFound = old })
	dir := t.TempDir()
	ad := &recordingAdapter{countingAdapter: countingAdapter{RuntimeAdapter: mock.NewAdapter()}}
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{ad}})
	cwd := t.TempDir()
	sc := startCmd("ses_n", "run_n", contracts.HarnessMock, cwd)
	sc.InitialPrompt = "synthetic task prompt"
	sc.Session.Lineage.ParentSessionID = "ses_parent"
	// The launching record and spec as startRun commits them; then pyred dies
	// before the launch goroutine reaches the adapter.
	rec := runRecord{Session: sc.Session, Phase: phaseLaunching,
		Run: contracts.Run{RunID: "run_n", SessionID: "ses_n", Adapter: "mock", Harness: contracts.HarnessMock, State: contracts.RunStarting, Cwd: cwd,
			Capabilities: mock.AdapterCaps}}
	if err := sqlitex.Tx(context.Background(), n.db, func(tx *sql.Tx) error {
		if err := putRunTx(context.Background(), tx, rec, time.Now()); err != nil {
			return err
		}
		return putLaunchSpecTx(context.Background(), tx, "run_n", sc, nil)
	}); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(n.d.runDir("run_n"), 0o700) // empty run dir, like server
	n = n.restart()
	got := n.run("run_n")
	if got.Phase != phaseEnded || got.Run.State != contracts.RunFailed || got.Run.StateReason != NeverStartedReason ||
		got.Run.AdapterData[AdapterDataNeverStarted] != "true" || got.Run.EndedAt == nil {
		t.Fatalf("never started = %+v %s", got.Run, got.Phase)
	}
	var reported bool
	for _, r := range n.rep.Reports(contracts.ReportRun) {
		var run contracts.Run
		_ = json.Unmarshal(r.Payload, &run)
		if run.RunID == "run_n" && run.State == contracts.RunFailed {
			reported = true
		}
	}
	if !reported {
		t.Fatal("failed run not reported (child->parent report needs it)")
	}
	if ad.launches.Load() != 0 {
		t.Fatal("never-started run relaunched automatically")
	}
	// A second restart leaves it alone.
	n = n.restart()
	if r := n.run("run_n"); r.Phase != phaseEnded {
		t.Fatalf("after second restart: %s", r.Phase)
	}
	// Explicit relaunch (hub resume) with an empty prompt: same spec.
	prev := got.Run
	rc := contracts.ResumeCommand{StartRunCommand: contracts.StartRunCommand{Session: sc.Session, RunID: "run_n2", Cwd: cwd,
		AccountPolicy: contracts.AccountPolicy{Mode: contracts.AccountEligible}, PreviousRun: &prev}}
	res := n.do(command(contracts.CmdResume, rc))
	if res.Error != nil {
		t.Fatalf("relaunch: %+v", res.Error)
	}
	n.d.WaitLaunches()
	if ad.launches.Load() != 1 || ad.last.InitialPrompt != "synthetic task prompt" || ad.last.Resume != nil {
		t.Fatalf("relaunch launches=%d last=%+v", ad.launches.Load(), ad.last)
	}
	r2 := n.run("run_n2")
	if r2.Run.PreviousRunID != "run_n" || r2.Run.ContinuationKind != contracts.ContinuationFresh || r2.Phase != phaseLive {
		t.Fatalf("relaunched run = %+v %s", r2.Run, r2.Phase)
	}
	if _, ok := n.d.launchSpecOf(context.Background(), "run_n"); ok {
		t.Fatal("spec of the relaunched run kept")
	}
	if _, ok := n.d.launchSpecOf(context.Background(), "run_n2"); ok {
		t.Fatal("spec kept after the handle was recorded")
	}
}

// W-resilience: an interrupted launch whose process exists is adopted.
func TestInterruptedLaunchWithLiveProcessIsAdopted(t *testing.T) {
	dir := t.TempDir()
	ad := &countingAdapter{RuntimeAdapter: mock.NewAdapter()}
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{ad}})
	n.start(startCmd("ses_l", "run_l", contracts.HarnessMock, t.TempDir()))
	// Pretend the handle never got recorded.
	rec := n.run("run_l")
	rec.Phase, rec.Handle = phaseLaunching, nil
	n.stop()
	db, err := sqlitex.Open(filepath.Join(dir, "pyred.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.Tx(context.Background(), db, func(tx *sql.Tx) error { return putRunTx(context.Background(), tx, rec, time.Now()) }); err != nil {
		t.Fatal(err)
	}
	db.Close()
	cfg := n.cfg
	cfg.DB, cfg.Reporter = nil, nil
	n = startNode(t, dir, cfg)
	got := n.run("run_l")
	if got.Phase != phaseLive || got.Handle == nil || got.Run.State.Terminal() {
		t.Fatalf("adopted = %+v %s", got.Run, got.Phase)
	}
	if ad.launches.Load() != 1 {
		t.Fatalf("launches = %d", ad.launches.Load())
	}
}

// W-resilience: shutdown refuses new launches and waits (bounded) for
// in-flight ones to record their handle.
func TestDrainWaitsForInflightLaunch(t *testing.T) {
	gate := make(chan struct{})
	ad := &blockingAdapter{RuntimeAdapter: mock.NewAdapter(), gate: gate}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{ad}, ShutdownGrace: 2 * time.Second})
	if res := n.do(command(contracts.CmdStartRun, startCmd("ses_d", "run_d", contracts.HarnessMock, t.TempDir()))); res.Error != nil {
		t.Fatal(res.Error)
	}
	if left := n.d.Drain(50 * time.Millisecond); left != 1 {
		t.Fatalf("in flight = %d, want 1", left)
	}
	res := n.do(command(contracts.CmdStartRun, startCmd("ses_d2", "run_d2", contracts.HarnessMock, t.TempDir())))
	if res.Error == nil || res.Error.Code != contracts.ErrUnavailable {
		t.Fatalf("launch during drain = %+v", res)
	}
	go func() { time.Sleep(100 * time.Millisecond); close(gate) }()
	start := time.Now()
	if left := n.d.Drain(2 * time.Second); left != 0 {
		t.Fatalf("still in flight = %d", left)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatal("drain did not return when the launch finished")
	}
	if r := n.run("run_d"); r.Phase != phaseLive || r.Handle == nil {
		t.Fatalf("drained launch = %+v %s", r.Run, r.Phase)
	}
}

// W-resilience: a launch stuck past the grace period does not block Close.
func TestCloseGivesUpOnStuckLaunch(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	ad := &blockingAdapter{RuntimeAdapter: mock.NewAdapter(), gate: gate}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{ad}, ShutdownGrace: 100 * time.Millisecond})
	n.do(command(contracts.CmdStartRun, startCmd("ses_s", "run_s", contracts.HarnessMock, t.TempDir())))
	start := time.Now()
	n.d.Close()
	n.d = nil
	if time.Since(start) > 2*time.Second {
		t.Fatal("Close blocked on a stuck launch")
	}
}

type blockingAdapter struct {
	contracts.RuntimeAdapter
	gate chan struct{}
}

func (b *blockingAdapter) Launch(ctx context.Context, l contracts.ResolvedLaunch) (contracts.RunHandle, error) {
	<-b.gate
	return b.RuntimeAdapter.Launch(ctx, l)
}

type recordingAdapter struct {
	countingAdapter
	last contracts.ResolvedLaunch
}

func (r *recordingAdapter) Launch(ctx context.Context, l contracts.ResolvedLaunch) (contracts.RunHandle, error) {
	r.last = l
	return r.countingAdapter.Launch(ctx, l)
}

func TestDeliverHumanLeaseBlocksPaste(t *testing.T) {
	w := daemontest.NewWorld()
	fake := daemontest.New(w, contracts.HarnessClaude) // paste-style input, no structured path
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{fake}})
	n.start(startCmd("ses_c", "run_c", contracts.HarnessClaude, t.TempDir()))
	n.d.PollNow(context.Background(), "run_c")

	lease := contracts.InputLease{LeaseID: "lease_1", RunID: "run_c", Holder: contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_me"},
		AcquiredAt: time.Now(), RenewedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	n.do(command(contracts.CmdLeaseChanged, lease))
	if n.run("run_c").Run.Lease == nil {
		t.Fatal("lease not recorded on run")
	}
	msg := contracts.DeliverCommand{SessionID: "ses_c", Message: contracts.MessageEnvelope{SchemaVersion: 1, MessageID: "msg_1", OperationID: "op_1",
		Sender: contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_peer"}, Recipient: contracts.Address{Kind: contracts.AddrSession, ID: "ses_c"},
		Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: "please review"}}}
	res := n.do(command(contracts.CmdDeliver, msg))
	var rc contracts.DeliveryReceipt
	json.Unmarshal(res.Result, &rc)
	if rc.State != contracts.DeliveryNodeReceived {
		t.Fatalf("deliver ack = %+v", rc)
	}
	n.d.engine.ProcessSession(context.Background(), "ses_c", true)
	m, _ := n.d.inbox.Get(context.Background(), "msg_1")
	if m.State != contracts.DeliveryQueued || !strings.Contains(m.Reason, "lease") || fake.Hits() != 0 {
		t.Fatalf("under lease: %+v hits=%d", m, fake.Hits())
	}
	// Duplicate deliver command: stored once.
	n.do(command(contracts.CmdDeliver, msg))
	// Lease released -> delivered.
	now := time.Now()
	lease.ReleasedAt = &now
	n.do(command(contracts.CmdLeaseChanged, lease))
	n.d.engine.ProcessSession(context.Background(), "ses_c", true)
	m, _ = n.d.inbox.Get(context.Background(), "msg_1")
	if m.State != contracts.DeliveryAccepted || fake.Hits() != 1 {
		t.Fatalf("after release: %+v hits=%d", m, fake.Hits())
	}
	p, _ := w.Proc("run_c")
	if !strings.HasPrefix(p.Delivered[0].Text, "[pyre-msg:msg_1] from session:ses_peer:\nplease review") {
		t.Fatalf("rendered = %q", p.Delivered[0].Text)
	}
	// Cancel after an attempt is refused.
	cr := n.do(command(contracts.CmdCancelDelivery, map[string]string{"message_id": "msg_1"}))
	if cr.Error == nil || cr.Error.Code != contracts.ErrConflict {
		t.Fatalf("cancel after attempt = %+v", cr)
	}
}

func TestCrashBetweenClaimAndReceiptAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	w := daemontest.NewWorld()
	fa := daemontest.New(w, contracts.HarnessClaude)
	fa.ReconcileUnknown = true // the runtime took the input but leaves no evidence
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{fa}})
	n.start(startCmd("ses_x", "run_x", contracts.HarnessClaude, t.TempDir()))
	n.rep.FailOn = func(kind contracts.ReportKind, p any) error {
		if r, ok := p.(contracts.DeliveryReceipt); ok && r.State == contracts.DeliveryAccepted {
			return errors.New("simulated crash")
		}
		return nil
	}
	n.do(command(contracts.CmdDeliver, contracts.DeliverCommand{SessionID: "ses_x", Message: contracts.MessageEnvelope{SchemaVersion: 1,
		MessageID: "msg_x", OperationID: "op_x", Sender: contracts.Principal{Kind: contracts.PrincipalOwner, ID: "o"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: "ses_x"}, Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: "rm the thing"}}}))
	n.d.engine.ProcessSession(context.Background(), "ses_x", true)
	if m, _ := n.d.inbox.Get(context.Background(), "msg_x"); m.State != contracts.DeliveryAttempting {
		t.Fatalf("pre-restart = %+v", m)
	}
	n = n.restart()
	m, _ := n.d.inbox.Get(context.Background(), "msg_x")
	if m.State != contracts.DeliveryIndeterminate || m.BoundRunID != "run_x" {
		t.Fatalf("post-restart = %+v", m)
	}
	n.d.engine.ProcessSession(context.Background(), "ses_x", true)
	if fa.Hits() != 1 {
		t.Fatalf("indeterminate message replayed: %d deliveries", fa.Hits())
	}
	rs := n.rep.Receipts("msg_x")
	if len(rs) == 0 || rs[len(rs)-1].State != contracts.DeliveryIndeterminate {
		t.Fatalf("receipts = %+v", rs)
	}
}

func TestAdapterCrashWindowWithoutEvidenceIsIndeterminate(t *testing.T) {
	ma := mock.NewAdapter()
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{ma}})
	n.start(startCmd("ses_m", "run_m", contracts.HarnessMock, t.TempDir()))
	ma.CrashAfterSubmit("run_m")
	ma.LoseEvidence("run_m")
	n.do(command(contracts.CmdDeliver, contracts.DeliverCommand{SessionID: "ses_m", Message: contracts.MessageEnvelope{SchemaVersion: 1,
		MessageID: "msg_m", OperationID: "op_m", Sender: contracts.Principal{Kind: contracts.PrincipalOwner, ID: "o"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: "ses_m"}, Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: "deploy"}}}))
	n.d.engine.ProcessSession(context.Background(), "ses_m", true)
	m, _ := n.d.inbox.Get(context.Background(), "msg_m")
	if m.State != contracts.DeliveryIndeterminate {
		t.Fatalf("state = %+v", m)
	}
	n.d.engine.ProcessSession(context.Background(), "ses_m", true)
	resp, _ := ma.ReadTranscript(context.Background(), contracts.RunHandle{RunID: "run_m"}, contracts.ReadSessionRequest{})
	count := 0
	for _, e := range resp.Entries {
		if e.MessageID == "msg_m" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("message submitted %d times", count)
	}
}

func TestBoundedPollingAndConcurrency(t *testing.T) {
	w := daemontest.NewWorld()
	ad := &countingAdapter{RuntimeAdapter: daemontest.New(w, contracts.HarnessClaude), observeIn: 20 * time.Millisecond}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{ad},
		Poll: PollConfig{Interval: 20 * time.Millisecond, IdleMax: 160 * time.Millisecond, MaxConcurrent: 2}})
	for i := 0; i < 6; i++ {
		id := string(rune('a' + i))
		n.start(startCmd("ses_"+id, "run_"+id, contracts.HarnessClaude, t.TempDir()))
	}
	time.Sleep(1200 * time.Millisecond)
	obs := ad.observes.Load()
	// Unbounded fixed-rate polling would be ~6 runs x 60 = 360 calls.
	if obs > 120 || obs < 6 {
		t.Fatalf("observe calls = %d (idle backoff not bounding polls)", obs)
	}
	if m := ad.maxInflt.Load(); m > 2 {
		t.Fatalf("max concurrent observes = %d > 2", m)
	}
	diag := n.d.Diagnostics(context.Background())
	if diag.Resources.PyreRSSMB <= 0 || diag.Resources.ActiveRuns != 6 || diag.Limits["poll_max_concurrent"] != 2 {
		t.Fatalf("diagnostics = %+v", diag)
	}
	// A state change resets the interval and is reported once.
	before := len(n.rep.Reports(contracts.ReportRun))
	w.Mutate("run_a", func(p *daemontest.Proc) { p.State = contracts.RunRunning; p.Activity = "editing main.go" })
	waitFor(t, "state change observed", func() bool { return n.run("run_a").Run.State == contracts.RunRunning })
	time.Sleep(200 * time.Millisecond)
	if after := len(n.rep.Reports(contracts.ReportRun)); after != before+1 {
		t.Fatalf("run reports %d -> %d, want exactly one more", before, after)
	}
	if len(n.rep.Reports(contracts.ReportSessionActivity)) != 1 {
		t.Fatalf("activity reports = %d", len(n.rep.Reports(contracts.ReportSessionActivity)))
	}
}

func TestApprovalsAnswerAndLimitFallback(t *testing.T) {
	ma := mock.NewAdapter()
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{ma}})
	n.start(startCmd("ses_p", "run_p", contracts.HarnessMock, t.TempDir()))
	ma.RequestApproval("run_p", contracts.Approval{Kind: contracts.ApprovalPermission, Title: "run rm -rf build", Options: []contracts.ApprovalOption{{ID: "allow", Label: "Allow"}}})
	n.d.PollNow(context.Background(), "run_p")
	r := n.run("run_p").Run
	if r.State != contracts.RunWaitingApproval || len(r.PendingApprovalIDs) != 1 {
		t.Fatalf("run = %+v", r)
	}
	var ap contracts.Approval
	reps := n.rep.Reports(contracts.ReportApproval)
	json.Unmarshal(reps[len(reps)-1].Payload, &ap)
	if ap.ApprovalID != r.PendingApprovalIDs[0] || ap.State != contracts.ApprovalPending || ap.RunID != "run_p" {
		t.Fatalf("approval report = %+v", ap)
	}
	// Stable ID across polls.
	n.d.PollNow(context.Background(), "run_p")
	if n.run("run_p").Run.PendingApprovalIDs[0] != ap.ApprovalID {
		t.Fatal("approval id changed between polls")
	}
	res := n.do(command(contracts.CmdAnswer, contracts.AnswerCommand{Approval: ap, OptionID: "allow"}))
	if res.Error != nil {
		t.Fatalf("answer = %+v", res.Error)
	}
	dup := n.do(command(contracts.CmdAnswer, contracts.AnswerCommand{Approval: ap, OptionID: "allow"}))
	if dup.Error == nil || dup.Error.Code != contracts.ErrConflict {
		t.Fatalf("duplicate answer = %+v", dup)
	}
	// Wrong run binding is refused.
	wrong := ap
	wrong.RunID = "run_other"
	if w := n.do(command(contracts.CmdAnswer, contracts.AnswerCommand{Approval: wrong, OptionID: "allow"})); w.Error == nil {
		t.Fatal("answer for another run accepted")
	}
	// Limit event -> fallback plan via the (ambient) account resolver.
	ma.InjectLimit("run_p", "usage_limit")
	n.d.PollNow(context.Background(), "run_p")
	n.d.WaitLaunches() // the fallback is planned outside the poll
	fb := n.rep.Reports(contracts.ReportFallback)
	if len(fb) != 1 {
		t.Fatalf("fallback reports = %d", len(fb))
	}
	var plan contracts.FallbackPlan
	json.Unmarshal(fb[0].Payload, &plan)
	if plan.Outcome != contracts.FallbackIntervention || !plan.UncertainWork {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestExplicitAccountUnsupportedWithAmbientResolver(t *testing.T) {
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{mock.NewAdapter()}})
	sc := startCmd("ses_e", "run_e", contracts.HarnessMock, t.TempDir())
	sc.AccountPolicy = contracts.AccountPolicy{Mode: contracts.AccountExplicit, AccountID: "acct_x"}
	res := n.do(command(contracts.CmdStartRun, sc))
	if res.Outcome != contracts.OutcomeUnsupported {
		t.Fatalf("explicit account = %+v", res)
	}
	sc = startCmd("ses_e", "run_e", contracts.HarnessMock, "/does/not/exist")
	if res := n.do(command(contracts.CmdStartRun, sc)); res.Error == nil || res.Error.Code != contracts.ErrInvalid {
		t.Fatalf("bad cwd = %+v", res)
	}
	sc = startCmd("ses_e", "run_e", contracts.HarnessCodex, t.TempDir())
	if res := n.do(command(contracts.CmdStartRun, sc)); res.Outcome != contracts.OutcomeUnsupported {
		t.Fatalf("missing harness = %+v", res)
	}
}

func TestStopAndResumeLineage(t *testing.T) {
	ma := mock.NewAdapter()
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{ma}})
	cwd := t.TempDir()
	r1 := n.start(startCmd("ses_r", "run_r1", contracts.HarnessMock, cwd))
	res := n.do(command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_r", RunID: "run_r1"}))
	if res.Error != nil {
		t.Fatalf("stop = %+v", res.Error)
	}
	if got := n.run("run_r1"); got.Phase != phaseEnded || got.Run.State != contracts.RunExited {
		t.Fatalf("stopped = %+v %s", got.Run, got.Phase)
	}
	prev := n.run("run_r1").Run
	rc := contracts.ResumeCommand{StartRunCommand: startCmd("ses_r", "run_r2", contracts.HarnessMock, cwd)}
	rc.PreviousRun = &prev
	n.do(command(contracts.CmdResume, rc))
	n.d.WaitLaunches()
	r2 := n.run("run_r2").Run
	if r2.ContinuationKind != contracts.ContinuationNativeResume || r2.PreviousRunID != "run_r1" || r2.NativeRef == nil || r2.NativeRef.ID != r1.NativeRef.ID {
		t.Fatalf("resumed = %+v (prev native %+v)", r2, r1.NativeRef)
	}
	// A resume whose launch failed (e.g. the account was still cooling down)
	// never ran: it has no native reference. Resuming after it continues the
	// last run that did run, natively, while lineage records the failed one.
	n.do(command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_r", RunID: "run_r2"}))
	failed := contracts.Run{RunID: "run_r3", SessionID: "ses_r", NodeID: prev.NodeID, Harness: contracts.HarnessMock,
		State: contracts.RunFailed, StateReason: "launch failed: no eligible subscription (cooldown)", PreviousRunID: "run_r2"}
	rc4 := contracts.ResumeCommand{StartRunCommand: startCmd("ses_r", "run_r4", contracts.HarnessMock, cwd)}
	rc4.PreviousRun = &failed
	n.do(command(contracts.CmdResume, rc4))
	n.d.WaitLaunches()
	r4 := n.run("run_r4").Run
	if r4.ContinuationKind != contracts.ContinuationNativeResume || r4.PreviousRunID != "run_r3" || r4.NativeRef == nil || r4.NativeRef.ID != r1.NativeRef.ID {
		t.Fatalf("resume after a failed launch = %+v (want native resume of %+v)", r4, r1.NativeRef)
	}
	// Cross-harness resume is refused.
	bad := contracts.ResumeCommand{StartRunCommand: startCmd("ses_q", "run_q", contracts.HarnessMock, cwd)}
	p2 := prev
	p2.Harness = contracts.HarnessClaude
	bad.PreviousRun = &p2
	if res := n.do(command(contracts.CmdResume, bad)); res.Error == nil || res.Error.Code != contracts.ErrInvalid {
		t.Fatalf("cross-harness resume = %+v", res)
	}
}

func TestWorktreeCheckoutLeavesDirtyRepoAlone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git missing")
	}
	repo := filepath.Join(t.TempDir(), "proj")
	os.MkdirAll(repo, 0o755)
	gitT := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.email=t@example.invalid", "-c", "user.name=t"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	gitT("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o644)
	gitT("add", "a.txt")
	gitT("commit", "-q", "-m", "init")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("dirty local edit\n"), 0o644)

	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{mock.NewAdapter()}})
	sc := startCmd("ses_w", "run_w", contracts.HarnessMock, repo)
	sc.Checkout = &contracts.CheckoutRequest{Mode: contracts.CheckoutWorktree, RepoID: "proj"}
	n.start(sc)
	run := n.run("run_w").Run
	if run.State == contracts.RunFailed || run.Checkout == nil || run.Checkout.Mode != contracts.CheckoutWorktree {
		t.Fatalf("worktree run = %+v", run)
	}
	if !strings.HasPrefix(run.Checkout.Branch, "pyre/") || run.Cwd == repo {
		t.Fatalf("binding = %+v cwd=%s", run.Checkout, run.Cwd)
	}
	if b, _ := os.ReadFile(filepath.Join(run.Cwd, "a.txt")); string(b) != "one\n" {
		t.Fatalf("worktree content = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(b) != "dirty local edit\n" {
		t.Fatalf("source checkout modified: %q", b)
	}
}

func TestHeartbeatAndInventoryActions(t *testing.T) {
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{mock.NewAdapter()}, HeartbeatInterval: 20 * time.Millisecond})
	waitFor(t, "status heartbeat", func() bool { return len(n.rep.Statuses()) > 0 })
	st := n.rep.Statuses()[0]
	if st.Resources.CPUCount == 0 || st.Versions["pyred"] == "" {
		t.Fatalf("status = %+v", st)
	}
	for _, a := range []string{contracts.ActionInventory, contracts.ActionDiagnostics} {
		res := n.do(command(contracts.CmdAction, contracts.NodeActionRequest{NodeID: "node_test", Action: a}))
		if res.Error != nil || len(res.Result) == 0 {
			t.Fatalf("%s = %+v", a, res)
		}
	}
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	os.Mkdir(filepath.Join(dir, ".git"), 0o755)
	params, _ := json.Marshal(contracts.DirListParams{Path: dir})
	res := n.do(command(contracts.CmdAction, contracts.NodeActionRequest{Action: contracts.ActionDirList, Params: params}))
	var dl contracts.DirListResult
	json.Unmarshal(res.Result, &dl)
	if !dl.IsGit || len(dl.Dirs) != 1 || dl.Dirs[0] != "sub" {
		t.Fatalf("dir.list = %+v", dl)
	}
	if res := n.do(command(contracts.CmdAction, contracts.NodeActionRequest{Action: "env.apply"})); res.Outcome != contracts.OutcomeUnsupported {
		t.Fatalf("unregistered action = %+v", res)
	}
	// Registered async actions report their result durably.
	n.d.RegisterAction("memory.status", func(ctx context.Context, p json.RawMessage) (any, error) {
		return contracts.MemoryStatus{Backend: "unavailable"}, nil
	})
	cmd := command(contracts.CmdAction, contracts.NodeActionRequest{Action: "memory.status"})
	if res := n.do(cmd); res.Outcome != contracts.OutcomeDelivered {
		t.Fatalf("async action = %+v", res)
	}
	n.d.WaitLaunches()
	found := false
	for _, r := range n.rep.Reports(contracts.ReportCommandResult) {
		var cr contracts.CommandResult
		json.Unmarshal(r.Payload, &cr)
		found = found || (cr.CommandID == cmd.CommandID && cr.Outcome == contracts.OutcomeSucceeded)
	}
	if !found {
		t.Fatal("async action result not reported")
	}
}

type fakeBinder struct {
	AmbientAccounts
	mu       sync.Mutex
	released []string
	checked  []string
}

func (b *fakeBinder) Bind(ctx context.Context, r AccountBindRequest) (AccountBinding, error) {
	return AccountBinding{ProfileDir: "/profiles/acct_1", Env: map[string]string{"CLAUDE_CONFIG_DIR": "/profiles/acct_1"},
		Unset: []string{"ANTHROPIC_API_KEY"}, Assignment: &contracts.AccountAssignment{AssignmentID: "asg_1", AccountID: "acct_1"}}, nil
}
func (b *fakeBinder) CheckAssignment(ctx context.Context, runID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.checked = append(b.checked, runID)
	return nil
}
func (b *fakeBinder) Release(runID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.released = append(b.released, runID)
}

func TestAccountBindingUnsetAndRelease(t *testing.T) {
	w := daemontest.NewWorld()
	fa := daemontest.New(w, contracts.HarnessClaude)
	b := &fakeBinder{}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{fa}, Accounts: b})
	run := n.start(startCmd("ses_acc", "run_acc", contracts.HarnessClaude, t.TempDir()))
	if run.AccountID != "acct_1" || run.AssignmentID != "asg_1" || run.AdapterData["pyre.account"] != "" {
		t.Fatalf("run account = %+v", run)
	}
	p, _ := w.Proc("run_acc")
	if p.Launch.ProfileDir != "/profiles/acct_1" || p.Launch.Env["CLAUDE_CONFIG_DIR"] != "/profiles/acct_1" || len(p.Launch.Unset) != 1 {
		t.Fatalf("launch = %+v", p.Launch)
	}
	if len(n.rep.Reports(contracts.ReportAssignment)) != 1 {
		t.Fatal("assignment not reported")
	}
	n.do(command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_acc"}))
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.released) != 1 || b.released[0] != "run_acc" || len(b.checked) == 0 {
		t.Fatalf("released=%v checked=%v", b.released, b.checked)
	}
}

type fakeEnv struct {
	mu  sync.Mutex
	got []EnvRequest
}

func (f *fakeEnv) Resolve(ctx context.Context, r EnvRequest) (EnvResult, error) {
	f.mu.Lock()
	f.got = append(f.got, r)
	f.mu.Unlock()
	return EnvResult{GenerationID: "env_gen1", Args: []string{"--settings", "/env/gen1/settings.json"}, Env: map[string]string{"PYRE_ENV_GENERATION": "env_gen1"},
		Executable: "/tools/claude/2.1.289/bin/claude", RuntimeVersion: "2.1.289", ConnectionGrants: []string{"conn_gh"}}, nil
}

func TestEnvironmentGenerationAndPinnedExecutable(t *testing.T) {
	w := daemontest.NewWorld()
	fa := daemontest.New(w, contracts.HarnessClaude)
	env := &fakeEnv{}
	var prep LaunchPrepRequest
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{fa}, Accounts: &fakeBinder{}, Environments: env,
		LaunchPrep: func(ctx context.Context, r LaunchPrepRequest) (LaunchPrepResult, error) {
			prep = r
			return LaunchPrepResult{Env: map[string]string{"GH_TOKEN": "x"}, Manifest: json.RawMessage(`{"env":["GH_TOKEN"]}`)}, nil
		}})
	sc := startCmd("ses_env", "run_env", contracts.HarnessClaude, t.TempDir())
	sc.Args = []string{"--add-dir", "/work/extra"}
	n.start(sc)
	run := n.run("run_env").Run
	if run.EnvGenerationID != "env_gen1" || run.Runtime.Executable != "/tools/claude/2.1.289/bin/claude" || run.Runtime.Version != "2.1.289" {
		t.Fatalf("run = %+v", run)
	}
	if run.AdapterData["pyre.connections"] != `{"env":["GH_TOKEN"]}` {
		t.Fatalf("connection manifest = %q", run.AdapterData["pyre.connections"])
	}
	p, _ := w.Proc("run_env")
	l := p.Launch
	if l.EnvGeneration != "env_gen1" || l.Executable != run.Runtime.Executable || len(l.Args) != 3 || l.Args[0] != "--settings" || l.Args[2] != "--add-dir=/work/extra" {
		t.Fatalf("launch = %+v", l)
	}
	if l.Env["PYRE_ENV_GENERATION"] != "env_gen1" || l.Env["GH_TOKEN"] != "x" {
		t.Fatal("env not merged into launch")
	}
	if len(env.got) != 1 || env.got[0].ConfigHome != "/profiles/acct_1" || prep.ConnectionGrants[0] != "conn_gh" {
		t.Fatalf("env request = %+v prep = %+v", env.got, prep)
	}
	act, _ := n.d.ActiveRuns(context.Background())
	if len(act) != 1 || act[0].GenerationID != "env_gen1" || act[0].Executable != run.Runtime.Executable {
		t.Fatalf("active runs = %+v", act)
	}
}

func TestRawInterruptRespectsInputLease(t *testing.T) {
	w := daemontest.NewWorld()
	fa := daemontest.New(w, contracts.HarnessClaude) // input.paste: interrupt is a raw Escape
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{fa}})
	n.start(startCmd("ses_i", "run_i", contracts.HarnessClaude, t.TempDir()))
	other := contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_phone"}
	n.do(command(contracts.CmdLeaseChanged, contracts.InputLease{LeaseID: "lease_x", RunID: "run_i", Holder: other, ExpiresAt: time.Now().Add(time.Hour)}))
	res := n.do(command(contracts.CmdInterrupt, contracts.RunCommand{SessionID: "ses_i"})) // actor cli_test
	if res.Error == nil || res.Error.Code != contracts.ErrConflict {
		t.Fatalf("interrupt under another's lease = %+v", res)
	}
	if p, _ := w.Proc("run_i"); p.Interrupts != 0 {
		t.Fatal("raw interrupt sent despite lease")
	}
	cmd := command(contracts.CmdInterrupt, contracts.RunCommand{SessionID: "ses_i"})
	cmd.Actor = other
	if res := n.do(cmd); res.Error != nil {
		t.Fatalf("lease holder interrupt = %+v", res.Error)
	}
	if p, _ := w.Proc("run_i"); p.Interrupts != 1 {
		t.Fatal("lease holder interrupt not sent")
	}
}

// WP18-6: a message queued behind a busy run goes out promptly once the run
// is idle again, even when the observer never saw the busy period (the
// runtime looked idle at every poll) and the engine's per-message backoff
// has grown.
func TestQueuedFollowUpDeliveredPromptlyAfterIdle(t *testing.T) {
	w := daemontest.NewWorld()
	fake := daemontest.New(w, contracts.HarnessClaude)
	var busyUntil, firstIdle, secondAccepted atomic.Int64
	fake.DeliverFunc = func(p *daemontest.Proc, in contracts.DeliveryInput) (contracts.DeliveryResult, error, bool) {
		now := time.Now().UnixNano()
		if now < busyUntil.Load() {
			return contracts.DeliveryResult{State: contracts.DeliveryQueued, Evidence: contracts.EvidenceNone, Detail: "fake runtime busy"}, nil, true
		}
		// Accept and stay busy for one 5 s turn; observations keep
		// reporting the prompt, so no state change is ever seen.
		p.Delivered = append(p.Delivered, in)
		end := time.Now().Add(5 * time.Second).UnixNano()
		if firstIdle.Load() == 0 {
			firstIdle.Store(end)
		} else {
			secondAccepted.Store(now)
		}
		busyUntil.Store(end)
		return contracts.DeliveryResult{State: contracts.DeliveryAccepted, Evidence: contracts.EvidenceMock, EvidenceRef: "fake:" + in.AttemptID}, nil, true
	}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{fake},
		Poll: PollConfig{Interval: 50 * time.Millisecond, IdleMax: 30 * time.Second}})
	n.start(startCmd("ses_f", "run_f", contracts.HarnessClaude, t.TempDir()))
	time.Sleep(500 * time.Millisecond) // the observer backs off while nothing changes
	deliver := func(id string) {
		msg := contracts.DeliverCommand{SessionID: "ses_f", Message: contracts.MessageEnvelope{SchemaVersion: 1, MessageID: id, OperationID: "op_" + id,
			Sender: contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_me"}, Recipient: contracts.Address{Kind: contracts.AddrSession, ID: "ses_f"},
			Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: "synthetic " + id}}}
		n.do(command(contracts.CmdDeliver, msg))
	}
	deliver("msg_f1")
	deliver("msg_f2")
	state := func(id string) contracts.RuntimeDeliveryState {
		m, _ := n.d.inbox.Get(context.Background(), id)
		return m.State
	}
	deadline := time.Now().Add(15 * time.Second)
	for state("msg_f2") != contracts.DeliveryAccepted {
		if time.Now().After(deadline) {
			t.Fatalf("follow-up never delivered (state %s)", state("msg_f2"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if state("msg_f1") != contracts.DeliveryAccepted {
		t.Fatalf("first message state %s", state("msg_f1"))
	}
	lag := time.Duration(secondAccepted.Load() - firstIdle.Load())
	t.Logf("follow-up accepted %v after the run went idle", lag.Round(time.Millisecond))
	if lag > time.Second {
		t.Fatalf("follow-up delivered %v after the run went idle; want within ~1-2 poll intervals", lag)
	}
}

// A recovered run of a provider-less harness (shell) has no account, so
// recovery must not check (and complain about) an assignment.
func TestRecoveredShellRunSkipsAccountCheck(t *testing.T) {
	dir := t.TempDir()
	w := daemontest.NewWorld()
	fa := daemontest.New(w, contracts.HarnessShell)
	b := &fakeBinder{}
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{fa}, Accounts: b})
	n.start(startCmd("ses_sh", "run_sh", contracts.HarnessShell, t.TempDir()))
	n = n.restart()
	n.d.mu.Lock()
	errs := append([]string(nil), n.d.errs...)
	n.d.mu.Unlock()
	b.mu.Lock()
	checked := append([]string(nil), b.checked...)
	b.mu.Unlock()
	if len(checked) != 0 || len(errs) != 0 || n.run("run_sh").Run.AdapterData["pyre.account_check"] != "" {
		t.Fatalf("shell run recovery checked accounts: checked=%v errs=%v", checked, errs)
	}
}

// SendRunSnapshot reports live and ended runs with their sessions so a hub
// that lost reports can reconcile them.
func TestSendRunSnapshot(t *testing.T) {
	dir := t.TempDir()
	w := daemontest.NewWorld()
	fa := daemontest.New(w, contracts.HarnessShell)
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{fa}})
	n.start(startCmd("ses_snap-live", "run_snap-live", contracts.HarnessShell, t.TempDir()))
	n.start(startCmd("ses_snap-ended", "run_snap-ended", contracts.HarnessShell, t.TempDir()))
	w.Kill("run_snap-ended")
	waitFor(t, "ended run observed", func() bool {
		n.d.PollNow(context.Background(), "run_snap-ended")
		return n.run("run_snap-ended").Run.State.Terminal()
	})
	sent, err := n.d.SendRunSnapshot(context.Background(), "test")
	if err != nil || sent != 2 {
		t.Fatalf("SendRunSnapshot = %d, %v", sent, err)
	}
	reps := n.rep.Reports(contracts.ReportRunSnapshot)
	if len(reps) != 1 {
		t.Fatalf("snapshot reports = %d", len(reps))
	}
	var snap contracts.RunSnapshot
	if err := json.Unmarshal(reps[0].Payload, &snap); err != nil {
		t.Fatal(err)
	}
	got := map[string]contracts.SessionRun{}
	for _, r := range snap.Records {
		got[r.Run.RunID] = r
	}
	if r := got["run_snap-ended"]; !r.Run.State.Terminal() || r.Session.Title != "test ses_snap-ended" || r.Run.NodeID != "node_test" {
		t.Fatalf("ended record = %+v", r)
	}
	if r := got["run_snap-live"]; r.Run.State.Terminal() || r.Session.SessionID != "ses_snap-live" {
		t.Fatalf("live record = %+v", r)
	}
}

// handoffOnly is an adapter whose ResumeSpec can only reconstruct (it
// returns a handoff without an error, as the Claude and Codex adapters do
// when there is no native thread on the account).
type handoffOnly struct{ *mock.Adapter }

func (handoffOnly) ResumeSpec(context.Context, contracts.Run, bool) (contracts.ContinuationKind, *contracts.NativeRef, error) {
	return contracts.ContinuationHandoff, nil, nil
}

func TestResumeHandoffNeedsAllowHandoff(t *testing.T) {
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{handoffOnly{mock.NewAdapter()}}})
	cwd := t.TempDir()
	n.start(startCmd("ses_h", "run_h1", contracts.HarnessMock, cwd))
	n.do(command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_h", RunID: "run_h1"}))
	prev := n.run("run_h1").Run

	rc := contracts.ResumeCommand{StartRunCommand: startCmd("ses_h", "run_h2", contracts.HarnessMock, cwd)}
	rc.PreviousRun = &prev
	if res := n.do(command(contracts.CmdResume, rc)); res.Error == nil || res.Error.Code != contracts.ErrUnsupported {
		t.Fatalf("handoff without AllowHandoff = %+v, want unsupported", res)
	}

	rc = contracts.ResumeCommand{StartRunCommand: startCmd("ses_h", "run_h3", contracts.HarnessMock, cwd)}
	rc.PreviousRun = &prev
	rc.AllowHandoff = true
	n.do(command(contracts.CmdResume, rc))
	n.d.WaitLaunches()
	if r := n.run("run_h3").Run; r.ContinuationKind != contracts.ContinuationHandoff || r.PreviousRunID != "run_h1" {
		t.Fatalf("allowed handoff = %+v", r)
	}
}

// TestSessionForNative: a harness-native session ID (what Hive records for
// a captured transcript) maps back to the Pyre session, its latest run and
// project; an unknown native ID is not attributed.
func TestSessionForNative(t *testing.T) {
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{mock.NewAdapter()}})
	sc := startCmd("ses_nat", "run_nat1", contracts.HarnessMock, t.TempDir())
	pid := "prj_synth-native"
	sc.Session.ProjectID = &pid
	r1 := n.start(sc)
	if r1.NativeRef == nil || r1.NativeRef.ID == "" {
		t.Fatalf("mock run has no native ref: %+v", r1)
	}
	sid, rid, proj, ok := n.d.SessionForNative(context.Background(), r1.NativeRef.ID)
	if !ok || sid != "ses_nat" || rid != "run_nat1" || proj == nil || *proj != pid {
		t.Fatalf("SessionForNative = %q %q %v %v", sid, rid, proj, ok)
	}
	if _, _, _, ok := n.d.SessionForNative(context.Background(), "11111111-not-a-pyre-session"); ok {
		t.Fatal("unknown native session attributed")
	}
	if _, _, _, ok := n.d.SessionForNative(context.Background(), ""); ok {
		t.Fatal("empty native id attributed")
	}
}

// TestOnRunEndedBeforeAccountRelease: the run-end hook sees the ended run,
// its native reference and bound profile, and runs before the assignment is
// released (so an integration can hold the profile past a rota check-in).
// ActiveRuns exposes the same fields for live runs.
func TestOnRunEndedBeforeAccountRelease(t *testing.T) {
	w := daemontest.NewWorld()
	fa := daemontest.New(w, contracts.HarnessClaude)
	b := &fakeBinder{}
	var mu sync.Mutex
	var ended []RunEnded
	var releasedAtHook int
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{fa}, Accounts: b,
		OnRunEnded: func(ev RunEnded) {
			b.mu.Lock()
			releasedAtHook = len(b.released)
			b.mu.Unlock()
			mu.Lock()
			ended = append(ended, ev)
			mu.Unlock()
		}})
	run := n.start(startCmd("ses_end", "run_end", contracts.HarnessClaude, t.TempDir()))
	act, err := n.d.ActiveRuns(context.Background())
	if err != nil || len(act) != 1 || act[0].ProfileDir != "/profiles/acct_1" || (run.NativeRef != nil && act[0].NativeID != run.NativeRef.ID) || act[0].State == "" {
		t.Fatalf("active runs = %+v %v", act, err)
	}
	n.do(command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_end"}))
	mu.Lock()
	defer mu.Unlock()
	if len(ended) != 1 || ended[0].Run.RunID != "run_end" || ended[0].ProfileDir != "/profiles/acct_1" || ended[0].Session.SessionID != "ses_end" {
		t.Fatalf("run ended events = %+v", ended)
	}
	if releasedAtHook != 0 {
		t.Fatal("account released before the run-end hook ran")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.released) != 1 {
		t.Fatalf("released = %v", b.released)
	}
}

func TestUnavailableActionSaysWhy(t *testing.T) {
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{mock.NewAdapter()},
		ActionsUnavailable: "pyred runs with --no-integrations"})
	res := n.do(command(contracts.CmdAction, contracts.NodeActionRequest{Action: "update.list"}))
	if res.Outcome != contracts.OutcomeUnsupported || res.Error == nil ||
		!strings.Contains(res.Error.Message, `"update.list" is not available on this node: pyred runs with --no-integrations`) {
		t.Fatalf("result = %+v err=%+v", res, res.Error)
	}
}

// dir.list (folder pickers): dot-folders only when asked (never .git);
// the node's home and worktree root ride along.
func TestDirListHiddenHomeAndWorktreeRoot(t *testing.T) {
	root := t.TempDir()
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{mock.NewAdapter()}, WorktreeRoot: root})
	dir := t.TempDir()
	for _, d := range []string{"sub", ".config", ".git"} {
		os.Mkdir(filepath.Join(dir, d), 0o755)
	}
	list := func(hidden bool) contracts.DirListResult {
		params, _ := json.Marshal(contracts.DirListParams{Path: dir, Hidden: hidden})
		res := n.do(command(contracts.CmdAction, contracts.NodeActionRequest{Action: contracts.ActionDirList, Params: params}))
		var dl contracts.DirListResult
		json.Unmarshal(res.Result, &dl)
		return dl
	}
	plain := list(false)
	if strings.Join(plain.Dirs, ",") != "sub" || plain.Hidden || !plain.IsGit {
		t.Fatalf("plain dir.list = %+v", plain)
	}
	all := list(true)
	if strings.Join(all.Dirs, ",") != ".config,sub" || !all.Hidden {
		t.Fatalf("hidden dir.list = %+v", all)
	}
	home, _ := os.UserHomeDir()
	if all.Home != home || all.WorktreeRoot != root {
		t.Fatalf("home/worktree root = %q %q", all.Home, all.WorktreeRoot)
	}
}
