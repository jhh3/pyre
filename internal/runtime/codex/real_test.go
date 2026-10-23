package codex

// Real Codex integration test. Skipped unless PYRE_REAL_HARNESS=1. Uses the
// existing default login (CODEX_HOME unset unless PYRE_REAL_CODEX_HOME), an
// isolated tmux server, a scratch cwd (PYRE_REAL_CODEX_CWD) and a handful of
// tiny synthetic prompts. It never edits ~/.codex/config.toml.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// appServerProcs counts app-server process trees on sock (the npm launcher
// chain node -> native codex counts once): matches whose parent is not a match.
func appServerProcs(sock string) int {
	out, _ := exec.Command("pgrep", "-f", "app-server --listen unix://"+sock).Output()
	pids := map[string]bool{}
	for _, p := range strings.Fields(string(out)) {
		cmd, _ := exec.Command("ps", "-o", "command=", "-p", p).Output()
		if !strings.Contains(string(cmd), "tmux") { // skip tmux clients/servers naming the command
			pids[p] = true
		}
	}
	roots := 0
	for p := range pids {
		pp, _ := exec.Command("ps", "-o", "ppid=", "-p", p).Output()
		if !pids[strings.TrimSpace(string(pp))] {
			roots++
		}
	}
	return roots
}

func TestRealCodex(t *testing.T) {
	if os.Getenv("PYRE_REAL_HARNESS") != "1" {
		t.Skip("set PYRE_REAL_HARNESS=1 to run against the real codex")
	}
	cwd := os.Getenv("PYRE_REAL_CODEX_CWD")
	if cwd == "" {
		t.Fatal("PYRE_REAL_CODEX_CWD required (scratch dir)")
	}
	home := os.Getenv("PYRE_REAL_CODEX_HOME")
	codex := os.Getenv("PYRE_REAL_CODEX")
	if codex == "" {
		codex, _ = exec.LookPath("codex")
	}
	dir, _ := os.MkdirTemp("", "pq")
	defer os.RemoveAll(dir)
	o := Options{NodeID: "node_real", TmuxSocket: filepath.Join(dir, "tmux", "t.sock"), StateRoot: filepath.Join(dir, "st"),
		CodexBin: codex, ApprovalPolicy: "untrusted", Sandbox: "read-only",
		AppServerArgs: []string{"-c", `model_reasoning_effort="low"`}, DeliverTimeout: 30 * time.Second}
	a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	defer a.srv.KillServer(ctx)
	pr, _ := a.Probe(ctx)
	t.Logf("probe: version=%s available=%v reason=%q", pr.Version, pr.Available, pr.Reason)
	wait := func(ad *Adapter, h contracts.RunHandle, d time.Duration, label string, pred func(contracts.Observation) bool) contracts.Observation {
		t.Helper()
		end := time.Now().Add(d)
		var obs contracts.Observation
		for time.Now().Before(end) {
			obs, _ = ad.Observe(ctx, h, "")
			if pred(obs) {
				t.Logf("%s: state=%s reason=%q approvals=%d subagents=%d activity=%q", label, obs.State, obs.StateReason, len(obs.Approvals), len(obs.Subagents), obs.Activity)
				return obs
			}
			time.Sleep(300 * time.Millisecond)
		}
		t.Fatalf("%s: timeout; last state=%s reason=%q", label, obs.State, obs.StateReason)
		return obs
	}
	idleP := func(o contracts.Observation) bool { return o.State == contracts.RunWaitingInput }
	l := contracts.ResolvedLaunch{RunID: "run_rc1", SessionID: "ses_rc", Harness: contracts.HarnessCodex, Cwd: cwd,
		StateDir: filepath.Join(dir, "runs", "run_rc1"), ProfileDir: home, Unset: []string{"OPENAI_API_KEY", "CODEX_API_KEY"}}
	h, err := a.Launch(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(o.StateRoot, "codex", homeKey(home), "codex.sock")
	t.Logf("launched thread %s; app-server processes on our socket: %d", h.NativeRef.ID, appServerProcs(sock))
	wait(a, h, 30*time.Second, "after launch", idleP)

	r, err := a.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_rc1", AttemptID: "dlv_rc1", Text: "Reply with the single word ok."})
	t.Logf("deliver: state=%s evidence=%s ref=%s err=%v", r.State, r.Evidence, r.EvidenceRef, err)
	if r.State != contracts.DeliveryAccepted || r.Evidence != contracts.EvidenceCodexUserMessage {
		t.Fatal("expected userMessage clientId evidence")
	}
	wait(a, h, 120*time.Second, "after msg_rc1", idleP)

	// Daemon restart: fresh adapter, Recover, no second app-server.
	a2, _ := New(o)
	h, err = a2.Recover(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	if n := appServerProcs(sock); n != 1 {
		t.Fatalf("app-server processes after recover: %d", n)
	}
	if r, _ := a2.Reconcile(ctx, h, "msg_rc1"); r.State != contracts.DeliveryAccepted {
		t.Fatalf("reconcile after restart: %+v", r)
	}
	t.Logf("recover: one app-server; msg_rc1 reconciled from thread/turns/list")

	// Native attach to the TUI window (codex resume --remote).
	d, err := a2.Attach(ctx, h, contracts.AttachCommand{RunID: h.RunID})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("attach: mechanism=%s command=%q", d.Mechanism, d.Command)
	outer, _ := tmux.NewServer("", filepath.Join(dir, "outer.sock"))
	_ = outer.NewSession(ctx, tmux.SessionSpec{Name: "human", Argv: d.Command, Width: 160, Height: 45})
	attached := false
	for i := 0; i < 50 && !attached; i++ {
		cs, _ := a2.srv.ListClients(ctx, h.RunID)
		attached = tmux.WritableClients(cs) == 1
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("attach client registered: %v", attached)
	if !attached {
		t.Fatal("attach argv did not attach")
	}

	// Approval through Pyre (shared with the attached TUI).
	target := filepath.Join(cwd, "pyre-wp07-codex.txt")
	_ = os.Remove(target)
	r, _ = a2.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_rc_appr", AttemptID: "dlv_a",
		Text: "Run exactly this shell command: `touch pyre-wp07-codex.txt`. Then reply with the single word done."})
	t.Logf("deliver approval prompt: %s", r.State)
	obs := wait(a2, h, 180*time.Second, "approval pending", func(o contracts.Observation) bool {
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
	ids := []string{}
	for _, op := range ap.Options {
		ids = append(ids, op.ID)
	}
	t.Logf("approval: request=%s title=%q body=%q options=%v", ap.RequestID, ap.Title, ap.Body, ids)
	rc, err := a2.Answer(ctx, h, ap, "accept", "")
	t.Logf("answer: outcome=%s detail=%q err=%v", rc.Outcome, rc.Detail, err)
	wait(a2, h, 180*time.Second, "after approval", func(o contracts.Observation) bool {
		for _, x := range o.Approvals {
			if x.RequestID == ap.RequestID && x.Resolution != nil {
				return idleP(o)
			}
		}
		return false
	})
	if _, err := os.Stat(target); err != nil {
		t.Logf("note: approved command output file not found (%v); sandbox may block writes in read-only mode", err)
	}
	_ = os.Remove(target)
	_ = outer.KillServer(ctx)

	// Native subagent.
	r, _ = a2.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_rc_sub", AttemptID: "dlv_s",
		Text: "Use your sub-agent / spawn_agent tool to delegate this tiny task to one sub-agent: 'reply with the single word ok'. Wait for it, then reply with what it said. Do not run shell commands."})
	t.Logf("deliver subagent prompt: %s", r.State)
	obs = wait(a2, h, 300*time.Second, "subagent", func(o contracts.Observation) bool { return idleP(o) && len(o.Subagents) > 0 })
	t.Logf("subagents observed: %d (first state=%s)", len(obs.Subagents), obs.Subagents[0].State)

	// Rate limits through the same app-server (keys only in the log).
	raw, err := a2.ReadRateLimits(ctx, home)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	keys := []string{}
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("account/rateLimits/read: err=%v keys=%v app-server processes=%d", err, keys, appServerProcs(sock))
	ai, err := a2.ReadAccount(ctx, home)
	t.Logf("account/read: type=%s plan=%s logged_in=%v err=%v", ai.Type, ai.PlanType, ai.LoggedIn, err)

	// Stop (last ref: app-server stops), then resume the same thread id in a
	// fresh app-server.
	rc, _ = a2.Stop(ctx, h)
	t0 := time.Now()
	for appServerProcs(sock) > 0 && time.Since(t0) < 15*time.Second {
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("stop: %s %q; app-server processes=%d (%.1fs after Stop returned)", rc.Outcome, rc.Detail, appServerProcs(sock), time.Since(t0).Seconds())
	if appServerProcs(sock) != 0 {
		t.Fatal("app-server outlived Stop")
	}
	kind, ref, _ := a2.ResumeSpec(ctx, contracts.Run{NativeRef: h.NativeRef}, true)
	l2 := l
	l2.RunID, l2.StateDir, l2.Resume = "run_rc2", filepath.Join(dir, "runs", "run_rc2"), ref
	h2, err := a2.Launch(ctx, l2)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("resume kind=%s: new run thread %s (same=%v)", kind, h2.NativeRef.ID, h2.NativeRef.ID == h.NativeRef.ID)
	if h2.NativeRef.ID != h.NativeRef.ID {
		t.Fatal("resume changed the thread id")
	}
	r, _ = a2.Deliver(ctx, h2, contracts.DeliveryInput{MessageID: "msg_rc2", AttemptID: "dlv_r2", Text: "Reply with the single word ok."})
	t.Logf("deliver after resume: state=%s evidence=%s", r.State, r.Evidence)
	if r.State != contracts.DeliveryAccepted {
		t.Fatal("deliver after resume")
	}
	wait(a2, h2, 120*time.Second, "after resume turn", idleP)
	resp, _ := a2.ReadTranscript(ctx, h2, contracts.ReadSessionRequest{Limit: 500})
	n := 0
	for _, e := range resp.Entries {
		if e.Kind == "pyre_message" {
			n++
		}
	}
	t.Logf("transcript entries=%d pyre_message=%d", len(resp.Entries), n)
	rc, _ = a2.Stop(ctx, h2)
	t.Logf("stop resumed run: %s %q; app-server processes=%d", rc.Outcome, rc.Detail, appServerProcs(sock))
}

// TestRealCodexBackgroundWake: a command the agent leaves running when its
// turn ends wakes the thread with a new turn when it exits (bgwake.go).
// Same gating and isolation as TestRealCodex (PYRE_REAL_HARNESS=1,
// PYRE_REAL_CODEX_CWD scratch dir, the default login, no TUI).
func TestRealCodexBackgroundWake(t *testing.T) {
	if os.Getenv("PYRE_REAL_HARNESS") != "1" {
		t.Skip("set PYRE_REAL_HARNESS=1 to run against the real codex")
	}
	cwd := os.Getenv("PYRE_REAL_CODEX_CWD")
	if cwd == "" {
		t.Fatal("PYRE_REAL_CODEX_CWD required (scratch dir)")
	}
	codex := os.Getenv("PYRE_REAL_CODEX")
	if codex == "" {
		codex, _ = exec.LookPath("codex")
	}
	dir, _ := os.MkdirTemp("", "pw")
	defer os.RemoveAll(dir)
	rec := &automations{}
	o := Options{NodeID: "node_real", TmuxSocket: filepath.Join(dir, "tmux", "t.sock"), StateRoot: filepath.Join(dir, "st"),
		CodexBin: codex, ApprovalPolicy: "never", Sandbox: "read-only", DisableTUI: true, OnAutomation: rec.add,
		AppServerArgs: []string{"-c", `model_reasoning_effort="low"`}, DeliverTimeout: 30 * time.Second}
	a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	defer a.srv.KillServer(ctx)
	if pr, _ := a.Probe(ctx); !pr.Available {
		t.Fatalf("codex unavailable: %s", pr.Reason)
	} else {
		t.Logf("codex %s", pr.Version)
	}
	h, err := a.Launch(ctx, contracts.ResolvedLaunch{RunID: "run_rbw", SessionID: "ses_rbw", Harness: contracts.HarnessCodex, Cwd: cwd,
		StateDir: filepath.Join(dir, "runs", "run_rbw"), Unset: []string{"OPENAI_API_KEY", "CODEX_API_KEY"}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop(ctx, h)
	t0 := time.Now()
	waitState := func(label string, d time.Duration, want contracts.RunState) {
		t.Helper()
		end := time.Now().Add(d)
		for time.Now().Before(end) {
			if obs, _ := a.Observe(ctx, h, ""); obs.State == want {
				t.Logf("%6.1fs %s: %s", time.Since(t0).Seconds(), label, want)
				return
			}
			time.Sleep(300 * time.Millisecond)
		}
		t.Fatalf("%s: never %s", label, want)
	}
	waitState("launched", 30*time.Second, contracts.RunWaitingInput)
	r, err := a.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_rbw", AttemptID: "dlv_rbw",
		Text: "Run this exact shell command once: `sleep 30; echo pyre-bg-done`. It yields back to you while still running: do not wait, poll or call any other tool; end your turn immediately by replying with the single word started."})
	if err != nil || r.State != contracts.DeliveryAccepted {
		t.Fatalf("deliver: %+v %v", r, err)
	}
	waitState("first turn ended", 120*time.Second, contracts.RunWaitingInput)
	var notice string
	end := time.Now().Add(150 * time.Second)
	for notice == "" && time.Now().Before(end) {
		ns, _ := wakeEntries(t, a, h)
		if len(ns) > 0 {
			notice = ns[0].Text
			break
		}
		time.Sleep(time.Second)
	}
	if notice == "" {
		t.Fatal("no wake turn after the background command finished")
	}
	t.Logf("%6.1fs wake notice:\n%s", time.Since(t0).Seconds(), notice)
	if !strings.Contains(notice, "sleep 30; echo pyre-bg-done") || !strings.Contains(notice, "exit 0") || !strings.Contains(notice, "pyre-bg-done") {
		t.Fatalf("unexpected notice")
	}
	waitState("wake turn ended", 120*time.Second, contracts.RunWaitingInput)
	_, all := wakeEntries(t, a, h)
	for _, e := range all {
		t.Logf("transcript %-12s %s", e.Kind, oneLine(e.Text, 200))
	}
	time.Sleep(5 * time.Second)
	if ns, _ := wakeEntries(t, a, h); len(ns) != 1 || rec.wakes() != 1 {
		t.Fatalf("want exactly one wake (got %d notices, %d automation reports)", len(ns), rec.wakes())
	}
}
