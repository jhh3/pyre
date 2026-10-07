package claude

// Real Claude Code integration test. Skipped unless PYRE_REAL_HARNESS=1.
// Uses the existing default login, an isolated tmux server, per-run hooks via
// --settings, an already-trusted scratch dir (PYRE_REAL_CWD) and a handful of
// tiny synthetic prompts. It never edits ~/.claude/settings.json.
//
//   PYRE_REAL_HARNESS=1 PYRE_REAL_CWD=/path/to/trusted/scratch go test -run TestRealClaude -v ./internal/runtime/claude/

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

func realOptions(t *testing.T, dir string) Options {
	exe, _ := os.Executable()
	claude := os.Getenv("PYRE_REAL_CLAUDE")
	if claude == "" {
		p, err := exec.LookPath("claude")
		if err != nil {
			home, _ := os.UserHomeDir()
			p = filepath.Join(home, ".local", "bin", "claude")
		}
		claude = p
	}
	return Options{
		NodeID: "node_real", TmuxSocket: filepath.Join(dir, "tmux", "t.sock"), SSHAlias: os.Getenv("PYRE_REAL_SSH_ALIAS"),
		ClaudeBin: claude, HookCommand: []string{"/usr/bin/env", "PYRE_TEST_ROLE=hook", exe},
		PermissionMode: "default", ApprovalWait: 90 * time.Second, DeliverTimeout: 30 * time.Second,
		StartupWait: 30 * time.Second, StopWait: 15 * time.Second,
	}
}

var realArgs = []string{"--model", "sonnet", "--effort", "low"}

// realLaunchMain (PYRE_TEST_ROLE=reallaunch) launches a run from a separate
// process, prints the handle, then waits to be SIGKILLed (simulated daemon
// crash).
func realLaunchMain() int {
	var l contracts.ResolvedLaunch
	_ = json.Unmarshal([]byte(os.Getenv("PYRE_REAL_LAUNCH")), &l)
	l.Env = map[string]string{"PYRE_SESSION_ID": l.SessionID}
	a, err := New(realOptions(nil, os.Getenv("PYRE_REAL_DIR")))
	if err != nil {
		fmt.Println("ERR", err)
		return 1
	}
	h, err := a.Launch(context.Background(), l)
	if err != nil {
		fmt.Println("ERR", err)
		return 1
	}
	b, _ := json.Marshal(h)
	fmt.Println("HANDLE " + string(b))
	time.Sleep(10 * time.Minute)
	return 0
}

func claudeProcs(sessionID string) int {
	out, _ := exec.Command("pgrep", "-f", "--", sessionID).Output()
	n := 0
	for _, l := range strings.Fields(string(out)) {
		cmd, _ := exec.Command("ps", "-o", "command=", "-p", l).Output()
		if strings.Contains(string(cmd), "--settings") && !strings.Contains(string(cmd), "tmux") {
			n++
		}
	}
	return n
}

func TestRealClaude(t *testing.T) {
	if os.Getenv("PYRE_REAL_HARNESS") != "1" {
		t.Skip("set PYRE_REAL_HARNESS=1 to run against the real claude")
	}
	cwd := os.Getenv("PYRE_REAL_CWD")
	if cwd == "" {
		t.Fatal("PYRE_REAL_CWD must name an already-trusted scratch directory")
	}
	dir, err := os.MkdirTemp("", "pr")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	o := realOptions(t, dir)
	a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	defer a.srv.KillServer(ctx)
	pr, _ := a.Probe(ctx)
	t.Logf("probe: version=%s available=%v reason=%q", pr.Version, pr.Available, pr.Reason)
	logObs := func(label string, obs contracts.Observation) {
		t.Logf("%s: state=%s reason=%q subagents=%d approvals=%d activity=%q", label, obs.State, obs.StateReason, len(obs.Subagents), len(obs.Approvals), obs.Activity)
	}
	wait := func(h contracts.RunHandle, d time.Duration, label string, pred func(contracts.Observation) bool) contracts.Observation {
		t.Helper()
		end := time.Now().Add(d)
		var obs contracts.Observation
		for time.Now().Before(end) {
			obs, _ = a.Observe(ctx, h, "")
			if pred(obs) {
				logObs(label, obs)
				return obs
			}
			time.Sleep(500 * time.Millisecond)
		}
		screen, _ := a.srv.CapturePane(ctx, h.TmuxTarget, tmux.CaptureOptions{})
		t.Fatalf("%s: timeout; state=%s reason=%q\n%s", label, obs.State, obs.StateReason, screen)
		return obs
	}
	idle := func(o contracts.Observation) bool { return o.State == contracts.RunWaitingInput }

	// 1. Launch from a separate process, then SIGKILL it (daemon crash).
	runID := "run_real1"
	l := contracts.ResolvedLaunch{RunID: runID, SessionID: "ses_real", Harness: contracts.HarnessClaude, Cwd: cwd,
		StateDir: filepath.Join(dir, "runs", runID), Args: realArgs}
	lb, _ := json.Marshal(l)
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), "PYRE_TEST_ROLE=reallaunch", "PYRE_REAL_DIR="+dir, "PYRE_REAL_LAUNCH="+string(lb))
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var h contracts.RunHandle
	buf := make([]byte, 64<<10)
	n := 0
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		m, err := stdout.Read(buf[n:])
		n += m
		if s := string(buf[:n]); strings.Contains(s, "HANDLE ") && strings.Contains(s[strings.Index(s, "HANDLE "):], "\n") {
			line := s[strings.Index(s, "HANDLE ")+7:]
			line = line[:strings.Index(line, "\n")]
			if err := json.Unmarshal([]byte(line), &h); err != nil {
				t.Fatal(err)
			}
			break
		}
		if strings.Contains(string(buf[:n]), "ERR") || err != nil {
			t.Fatalf("launcher: %s %v", buf[:n], err)
		}
	}
	_ = cmd.Process.Signal(syscall.SIGKILL)
	_ = cmd.Wait()
	t.Logf("launcher pid %d SIGKILLed; claude session id %s", cmd.Process.Pid, h.NativeRef.ID)
	h, err = a.Recover(ctx, h)
	if err != nil {
		t.Fatalf("recover after controller kill: %v", err)
	}
	obs := wait(h, 60*time.Second, "after recover", idle)
	if obs.NativeRef == nil || obs.NativeRef.ID != h.NativeRef.ID {
		t.Fatalf("native ref %+v", obs.NativeRef)
	}
	if c := claudeProcs(h.NativeRef.ID); c != 1 {
		t.Fatalf("expected exactly one claude process for the session, got %d", c)
	}
	t.Logf("claude processes for session after controller kill: 1")

	// 2. Deliver with acceptance evidence.
	r, err := a.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_real1", AttemptID: "dlv_real1", Mode: contracts.ModeQueue, Text: "Reply with the single word ok."})
	t.Logf("deliver msg_real1: state=%s evidence=%s detail=%q err=%v", r.State, r.Evidence, r.Detail, err)
	if err != nil || r.State != contracts.DeliveryAccepted || r.Evidence != contracts.EvidenceClaudePromptHook {
		t.Fatal("expected accepted with prompt-hook evidence")
	}
	wait(h, 90*time.Second, "after msg_real1", idle)
	if r, _ := a.Reconcile(ctx, h, "msg_real1"); r.State != contracts.DeliveryAccepted {
		t.Fatalf("reconcile %+v", r)
	}

	// 3. Native attach: the attach argv in a pty (outer isolated tmux) shows
	// up in list-clients and makes Pyre queue instead of paste.
	d, err := a.Attach(ctx, h, contracts.AttachCommand{RunID: h.RunID})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("attach descriptor: mechanism=%s command=%q", d.Mechanism, d.Command)
	outer, _ := tmux.NewServer("", filepath.Join(dir, "outer.sock"))
	if err := outer.NewSession(ctx, tmux.SessionSpec{Name: "human", Argv: d.Command, Width: 160, Height: 45}); err != nil {
		t.Fatal(err)
	}
	attached := false
	for i := 0; i < 40 && !attached; i++ {
		cs, _ := a.srv.ListClients(ctx, h.TmuxTarget)
		attached = tmux.WritableClients(cs) == 1
		time.Sleep(100 * time.Millisecond)
	}
	if !attached {
		t.Fatal("attach argv did not attach")
	}
	r, _ = a.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_real_lease", AttemptID: "dlv_l", Text: "Reply with the single word ok."})
	t.Logf("deliver while attached: state=%s detail=%q", r.State, r.Detail)
	if r.State != contracts.DeliveryQueued {
		t.Fatal("expected queued while a human is attached")
	}
	_ = outer.KillServer(ctx) // closes the human's terminal
	time.Sleep(time.Second)
	if c := claudeProcs(h.NativeRef.ID); c != 1 {
		t.Fatalf("claude processes after detach: %d", c)
	}

	// 4. Native subagent.
	r, _ = a.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_real_sub", AttemptID: "dlv_s",
		Text: "Use the Agent tool exactly once with a general-purpose subagent whose task is: reply with the single word ok. Then reply with the single word done."})
	t.Logf("deliver subagent prompt: state=%s evidence=%s", r.State, r.Evidence)
	obs = wait(h, 180*time.Second, "subagent", func(o contracts.Observation) bool {
		return o.State == contracts.RunWaitingInput && len(o.Subagents) > 0 && o.Subagents[0].State == "stopped"
	})
	t.Logf("subagent observed: id=%s kind=%s", obs.Subagents[0].ID, obs.Subagents[0].Kind)

	// 5. Approval answered through Pyre (broker), harmless command in scratch.
	target := filepath.Join(cwd, "pyre-wp07-approval.txt")
	_ = os.Remove(target)
	r, _ = a.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_real_appr", AttemptID: "dlv_a",
		Text: "Use the Bash tool to run exactly: touch pyre-wp07-approval.txt  Then reply with the single word done."})
	t.Logf("deliver approval prompt: state=%s", r.State)
	obs = wait(h, 120*time.Second, "approval pending", func(o contracts.Observation) bool {
		for _, ap := range o.Approvals {
			if ap.State == contracts.ApprovalPending {
				return true
			}
		}
		return false
	})
	var ap contracts.Approval
	for _, x := range obs.Approvals {
		if x.State == contracts.ApprovalPending {
			ap = x
		}
	}
	t.Logf("approval: id=%s request=%s title=%q body=%q", ap.ApprovalID, ap.RequestID, ap.Title, ap.Body)
	if r2, _ := a.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_real_blocked", AttemptID: "dlv_b", Text: "x"}); r2.State != contracts.DeliveryQueued {
		t.Fatalf("delivery during approval must queue: %+v", r2)
	}
	rc, err := a.Answer(ctx, h, ap, "allow", "")
	t.Logf("answer: accepted=%v outcome=%s detail=%q err=%v", rc.Accepted, rc.Outcome, rc.Detail, err)
	obs = wait(h, 120*time.Second, "approval effective", func(o contracts.Observation) bool {
		for _, x := range o.Approvals {
			if x.RequestID == ap.RequestID && x.Resolution != nil && x.Resolution.Effect == "effective" {
				return o.State == contracts.RunWaitingInput
			}
		}
		return false
	})
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("approved command did not run: %v", err)
	}
	_ = os.Remove(target)

	// 6. Stop and native resume with the same id.
	rc, _ = a.Stop(ctx, h)
	t.Logf("stop: outcome=%s detail=%q", rc.Outcome, rc.Detail)
	obs, _ = a.Observe(ctx, h, "")
	logObs("after stop", obs)
	if obs.State != contracts.RunExited {
		t.Fatal("not exited")
	}
	kind, ref, _ := a.ResumeSpec(ctx, contracts.Run{NativeRef: obs.NativeRef}, true)
	if kind != contracts.ContinuationNativeResume {
		t.Fatalf("resume spec %v", kind)
	}
	l2 := contracts.ResolvedLaunch{RunID: "run_real2", SessionID: "ses_real", Harness: contracts.HarnessClaude, Cwd: cwd,
		StateDir: filepath.Join(dir, "runs", "run_real2"), Args: realArgs, Resume: ref}
	h2, err := a.Launch(ctx, l2)
	if err != nil {
		t.Fatal(err)
	}
	obs = wait(h2, 60*time.Second, "resumed", func(o contracts.Observation) bool { return idle(o) && strings.Contains(o.StateReason, "resume") })
	if obs.NativeRef.ID != h.NativeRef.ID || !strings.Contains(obs.StateReason, "resume") {
		t.Fatalf("resume did not keep the id: %+v", obs.NativeRef)
	}
	t.Logf("resumed with the same claude session id %s", obs.NativeRef.ID)
	r, _ = a.Deliver(ctx, h2, contracts.DeliveryInput{MessageID: "msg_real2", AttemptID: "dlv_r2", Text: "Reply with the single word ok."})
	t.Logf("deliver after resume: state=%s evidence=%s", r.State, r.Evidence)
	if r.State != contracts.DeliveryAccepted {
		t.Fatal("deliver after resume")
	}
	wait(h2, 90*time.Second, "after msg_real2", idle)
	resp, _ := a.ReadTranscript(ctx, h2, contracts.ReadSessionRequest{Limit: 500})
	pyreMsgs := 0
	for _, e := range resp.Entries {
		if e.Kind == "pyre_message" {
			pyreMsgs++
		}
	}
	t.Logf("transcript entries=%d pyre_message entries=%d (history kept across resume)", len(resp.Entries), pyreMsgs)
	if pyreMsgs < 4 {
		t.Fatalf("expected the resumed transcript to contain earlier pyre messages")
	}
	rc, _ = a.Stop(ctx, h2)
	t.Logf("stop resumed run: outcome=%s detail=%q", rc.Outcome, rc.Detail)
}

// TestRealClaudeTrustDialog checks detection of the workspace-trust dialog in
// an untrusted scratch dir without accepting it (no model turn).
func TestRealClaudeTrustDialog(t *testing.T) {
	if os.Getenv("PYRE_REAL_HARNESS") != "1" {
		t.Skip("set PYRE_REAL_HARNESS=1")
	}
	dir, _ := os.MkdirTemp("", "pt")
	defer os.RemoveAll(dir)
	work := filepath.Join(dir, "untrusted")
	_ = os.MkdirAll(work, 0o755)
	o := realOptions(t, dir)
	o.StartupWait = 8 * time.Second
	a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	defer a.srv.KillServer(ctx)
	h, err := a.Launch(ctx, contracts.ResolvedLaunch{RunID: "run_trust", SessionID: "ses_t", Cwd: work, StateDir: filepath.Join(dir, "run"), Args: realArgs})
	if err != nil {
		t.Fatal(err)
	}
	obs, _ := a.Observe(ctx, h, "")
	t.Logf("untrusted dir: state=%s reason=%q", obs.State, obs.StateReason)
	if !strings.Contains(obs.StateReason, "needs human: workspace trust") {
		t.Fatal("trust dialog not detected")
	}
	r, _ := a.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_t", Text: "x"})
	t.Logf("deliver into trust dialog: state=%s detail=%q", r.State, r.Detail)
	if r.State != contracts.DeliveryQueued {
		t.Fatal("must not type into the trust dialog")
	}
	_ = a.srv.TerminateSession(ctx, h.TmuxTarget, 5*time.Second)
}
