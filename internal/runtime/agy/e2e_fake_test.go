//go:build !windows

package agy

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

type fakeEnv struct {
	t    *testing.T
	a    *Adapter
	opts Options
	dir  string
	home string // fake agy home (never the real ~/.gemini)
}

func newFakeEnv(t *testing.T, mutate func(*Options)) *fakeEnv {
	t.Helper()
	for _, tool := range []string{"tmux", "stty"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "pa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	home := filepath.Join(dir, "agyhome")
	_ = os.MkdirAll(home, 0o700)
	o := Options{
		NodeID: "node_t", TmuxSocket: filepath.Join(dir, "tmux", "t.sock"), SSHAlias: "nodet",
		AgyBin: exe, AgyHome: home, WorkspaceRoot: filepath.Join(dir, "agyws"),
		HookCommand:    []string{"/usr/bin/env", "PYRE_TEST_ROLE=hook", exe},
		MCPCommand:     []string{"/opt/synthetic/pyre", "mcp"},
		DeliverTimeout: 8 * time.Second, StartupWait: 8 * time.Second, StopWait: 5 * time.Second,
	}
	if mutate != nil {
		mutate(&o)
	}
	a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.srv.KillServer(context.Background()) })
	return &fakeEnv{t: t, a: a, opts: o, dir: dir, home: home}
}

func (f *fakeEnv) restart() *Adapter {
	a, err := New(f.opts)
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

func (f *fakeEnv) launch(runID string, l contracts.ResolvedLaunch) contracts.RunHandle {
	f.t.Helper()
	work := filepath.Join(f.dir, "work")
	_ = os.MkdirAll(work, 0o755)
	env := map[string]string{"PYRE_TEST_ROLE": "fakeagy", "PYRE_SESSION_ID": "ses_t", "FAKE_AGY_HOME": f.home}
	for k, v := range l.Env {
		env[k] = v
	}
	l.RunID, l.SessionID, l.Harness, l.Cwd, l.Env = runID, "ses_t", contracts.HarnessAgy, work, env
	l.StateDir = filepath.Join(f.dir, "runs", runID)
	h, err := f.a.Launch(context.Background(), l)
	if err != nil {
		f.t.Fatal(err)
	}
	return h
}

func (f *fakeEnv) waitObs(a *Adapter, h contracts.RunHandle, d time.Duration, pred func(contracts.Observation) bool) contracts.Observation {
	f.t.Helper()
	end := time.Now().Add(d)
	var obs contracts.Observation
	for time.Now().Before(end) {
		o, err := a.Observe(context.Background(), h, "")
		if err != nil {
			f.t.Fatal(err)
		}
		obs = o
		if pred(o) {
			return o
		}
		time.Sleep(150 * time.Millisecond)
	}
	screen, _ := a.srv.CapturePane(context.Background(), h.TmuxTarget, tmux.CaptureOptions{Join: true})
	f.t.Fatalf("condition not reached; last state %s (%s)\nscreen:\n%s", obs.State, obs.StateReason, screen)
	return obs
}

func (f *fakeEnv) waitState(a *Adapter, h contracts.RunHandle, want contracts.RunState) contracts.Observation {
	f.t.Helper()
	return f.waitObs(a, h, 10*time.Second, func(o contracts.Observation) bool { return o.State == want })
}

func TestFakeLaunchDeliverTranscriptResume(t *testing.T) {
	f := newFakeEnv(t, nil)
	argv := filepath.Join(f.dir, "argv.txt")
	h := f.launch("run_a", contracts.ResolvedLaunch{Env: map[string]string{"FAKE_ARGV_FILE": argv, PermissionsEnv: "bypass"},
		AgentGuide: "SYNTHETIC GUIDE LINE", Model: "fake-model-x", Effort: "high"})
	f.waitState(f.a, h, contracts.RunWaitingInput)

	b, _ := os.ReadFile(argv)
	args := strings.Split(string(b), "\n")
	for _, want := range []string{"--add-dir", "--dangerously-skip-permissions", "--model", "fake-model-x", "--effort", "high"} {
		if !contains(args, want) {
			t.Fatalf("argv %q lacks %s", args, want)
		}
	}
	ws := filepath.Join(f.opts.WorkspaceRoot, "ses_t")
	if args[1] != ws {
		t.Fatalf("--add-dir %q, want %s", args[1], ws)
	}
	rules, _ := os.ReadFile(filepath.Join(ws, ".agents", "rules", "pyre.md"))
	if !strings.Contains(string(rules), "SYNTHETIC GUIDE LINE") || !strings.Contains(string(rules), filepath.Join(f.dir, "work")) {
		t.Fatalf("rules: %s", rules)
	}
	var mcp struct {
		MCPServers map[string]struct {
			Command string
			Args    []string
		} `json:"mcpServers"`
	}
	mb, _ := os.ReadFile(filepath.Join(ws, ".agents", "mcp_config.json"))
	if json.Unmarshal(mb, &mcp) != nil || mcp.MCPServers["pyre"].Command != "/opt/synthetic/pyre" || mcp.MCPServers["pyre"].Args[0] != "mcp" {
		t.Fatalf("mcp_config: %s", mb)
	}

	r, err := f.a.Deliver(context.Background(), h, contracts.DeliveryInput{MessageID: "msg_A1", AttemptID: "att_1", Mode: contracts.ModeQueue,
		Text: "first line\nsecond line"})
	if err != nil || r.State != contracts.DeliveryAccepted || r.Evidence != contracts.EvidenceAgyUserInput {
		t.Fatalf("deliver: %+v %v", r, err)
	}
	obs := f.waitState(f.a, h, contracts.RunWaitingInput)
	if obs.NativeRef == nil || obs.NativeRef.Kind != NativeKind || !safeConversationID(obs.NativeRef.ID) {
		t.Fatalf("native ref %+v", obs.NativeRef)
	}
	// The engine's dated header is the marker (no second one is added).
	r, err = f.a.Deliver(context.Background(), h, contracts.DeliveryInput{MessageID: "msg_A3", AttemptID: "att_h", Mode: contracts.ModeQueue,
		Text: "[pyre-msg:msg_A3 · sent 3:05 PM PT (just now)] from session:synthetic-peer\n\nplease rebase"})
	if err != nil || r.State != contracts.DeliveryAccepted || r.Evidence != contracts.EvidenceAgyUserInput {
		t.Fatalf("dated header deliver: %+v %v", r, err)
	}
	f.waitState(f.a, h, contracts.RunWaitingInput)
	// Idempotent: evidence present, nothing re-sent.
	r, _ = f.a.Deliver(context.Background(), h, contracts.DeliveryInput{MessageID: "msg_A1", AttemptID: "att_2", Mode: contracts.ModeQueue, Text: "x"})
	if r.State != contracts.DeliveryAccepted || !strings.Contains(r.Detail, "already accepted") {
		t.Fatalf("redeliver: %+v", r)
	}
	tr, _ := f.a.ReadTranscript(context.Background(), h, contracts.ReadSessionRequest{Limit: 50})
	var kinds []string
	for _, e := range tr.Entries {
		kinds = append(kinds, e.Kind)
	}
	if len(tr.Entries) != 6 || tr.Entries[0].Kind != "pyre_message" || tr.Entries[0].MessageID != "msg_A1" ||
		!strings.Contains(tr.Entries[0].Text, "second line") || tr.Entries[2].Kind != "assistant" || tr.Entries[3].MessageID != "msg_A3" {
		t.Fatalf("transcript entries %v: %+v", kinds, tr.Entries)
	}
	if format, files := f.a.TranscriptFiles(h); format != TranscriptFormat || len(files) != 1 {
		t.Fatalf("transcript files %s %v", format, files)
	}

	// Stop with /exit, then resume the conversation natively.
	rc, _ := f.a.Stop(context.Background(), h)
	if rc.Outcome != contracts.OutcomeSucceeded || !strings.Contains(rc.Detail, "/exit") {
		t.Fatalf("stop: %+v", rc)
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Fatalf("session folder kept after stop: %v", err)
	}
	prev := contracts.Run{RunID: "run_a", NativeRef: obs.NativeRef}
	kind, ref, err := f.a.ResumeSpec(context.Background(), prev, true)
	if err != nil || kind != contracts.ContinuationNativeResume || ref.ID != obs.NativeRef.ID {
		t.Fatalf("resume spec %s %+v %v", kind, ref, err)
	}
	h2 := f.launch("run_b", contracts.ResolvedLaunch{Env: map[string]string{"FAKE_ARGV_FILE": argv}, Resume: ref})
	if h2.NativeRef == nil || h2.NativeRef.ID != ref.ID {
		t.Fatalf("resumed handle ref %+v", h2.NativeRef)
	}
	b, _ = os.ReadFile(argv)
	if !strings.Contains(string(b), "--conversation\n"+ref.ID) || strings.Contains(string(b), "--dangerously-skip-permissions") {
		t.Fatalf("resume argv %q", b)
	}
	f.waitState(f.a, h2, contracts.RunWaitingInput)
	r, err = f.a.Deliver(context.Background(), h2, contracts.DeliveryInput{MessageID: "msg_A2", AttemptID: "att_3", Mode: contracts.ModeQueue, Text: "after resume"})
	if err != nil || r.State != contracts.DeliveryAccepted {
		t.Fatalf("deliver after resume: %+v %v", r, err)
	}
	// An unknown conversation hands off.
	if kind, _, _ := f.a.ResumeSpec(context.Background(), contracts.Run{NativeRef: &contracts.NativeRef{Kind: NativeKind, ID: "00000000-0000-4000-8000-000000000000"}}, true); kind != contracts.ContinuationHandoff {
		t.Fatalf("unknown conversation: %s", kind)
	}
}

func TestFakeBusyQueuesAndInterrupt(t *testing.T) {
	f := newFakeEnv(t, nil)
	h := f.launch("run_c", contracts.ResolvedLaunch{InitialPrompt: "work FAKE_SLOW"})
	f.waitState(f.a, h, contracts.RunRunning)
	r, err := f.a.Deliver(context.Background(), h, contracts.DeliveryInput{MessageID: "msg_C1", AttemptID: "att_1", Mode: contracts.ModeQueue, Text: "later"})
	if err != nil || r.State != contracts.DeliveryQueued {
		t.Fatalf("busy deliver: %+v %v", r, err)
	}
	if ok, why := f.a.InputReady(context.Background(), h, contracts.ModeQueue); ok || why == "" {
		t.Fatalf("input ready while busy: %v %q", ok, why)
	}
	if _, err := f.a.Deliver(context.Background(), h, contracts.DeliveryInput{MessageID: "msg_C2", Mode: contracts.ModeSteer, Text: "x"}); !contracts.IsCode(err, contracts.ErrUnsupported) {
		t.Fatalf("steer: %v", err)
	}
	rc, _ := f.a.Interrupt(context.Background(), h)
	if !rc.Accepted {
		t.Fatalf("interrupt: %+v", rc)
	}
	// No Stop hook after an interrupt: the screen ends the turn.
	obs := f.waitState(f.a, h, contracts.RunWaitingInput)
	if !strings.Contains(obs.StateReason, "interrupted") {
		t.Fatalf("reason %q", obs.StateReason)
	}
	r, err = f.a.Deliver(context.Background(), h, contracts.DeliveryInput{MessageID: "msg_C1", AttemptID: "att_2", Mode: contracts.ModeQueue, Text: "later"})
	if err != nil || r.State != contracts.DeliveryAccepted {
		t.Fatalf("deliver after interrupt: %+v %v", r, err)
	}
}

// An interrupt with text in the input box: no Stop hook and no "? for
// shortcuts" (the draft hides it), but no "esc to cancel" either, so the
// run ends its turn instead of staying "running" forever. While the turn
// runs, a draft keeps "esc to cancel" and the run stays running.
func TestFakeInterruptWithDraftEndsTurn(t *testing.T) {
	f := newFakeEnv(t, nil)
	h := f.launch("run_g", contracts.ResolvedLaunch{InitialPrompt: "work FAKE_SLOW"})
	f.waitState(f.a, h, contracts.RunRunning)
	_ = f.a.srv.Paste(context.Background(), h.TmuxTarget, "d", []byte("half-typed draft"))
	time.Sleep(stateGrace + 500*time.Millisecond)
	if o, _ := f.a.Observe(context.Background(), h, ""); o.State != contracts.RunRunning {
		t.Fatalf("busy with a draft: %s (%s)", o.State, o.StateReason)
	}
	rc, _ := f.a.Interrupt(context.Background(), h)
	if rc.Outcome != contracts.OutcomeSucceeded {
		t.Fatalf("interrupt: %+v", rc)
	}
	obs := f.waitState(f.a, h, contracts.RunWaitingInput)
	if !strings.Contains(obs.StateReason, "draft") {
		t.Fatalf("reason %q", obs.StateReason)
	}
	// The draft is not Pyre's: mail waits instead of pasting over it.
	if r, _ := f.a.Deliver(context.Background(), h, contracts.DeliveryInput{MessageID: "msg_G1", AttemptID: "att_1", Mode: contracts.ModeQueue, Text: "x"}); r.State != contracts.DeliveryQueued {
		t.Fatalf("deliver over a human draft: %+v", r)
	}
}

func TestFakeTrustDialogAndPermissionPrompt(t *testing.T) {
	work := ""
	f := newFakeEnv(t, func(o *Options) {
		o.TrustWorkspace = func(cwd string) bool { return cwd == work }
	})
	work = filepath.Join(f.dir, "work")
	autos := make(chan string, 4)
	f.a.o.OnAutomation = func(n contracts.NodeAutomation) { autos <- n.Action }
	h := f.launch("run_d", contracts.ResolvedLaunch{Env: map[string]string{"FAKE_TRUST": "1"}, InitialPrompt: "do it FAKE_PERM"})
	obs := f.waitState(f.a, h, contracts.RunWaitingApproval)
	if !strings.Contains(obs.StateReason, "needs human") || !strings.Contains(obs.StateReason, "permission") {
		t.Fatalf("reason %q", obs.StateReason)
	}
	r, _ := f.a.Deliver(context.Background(), h, contracts.DeliveryInput{MessageID: "msg_D1", AttemptID: "att_1", Mode: contracts.ModeQueue, Text: "x"})
	if r.State != contracts.DeliveryQueued {
		t.Fatalf("deliver under a dialog: %+v", r)
	}
	if _, err := f.a.Answer(context.Background(), h, contracts.Approval{}, "allow", ""); !contracts.IsCode(err, contracts.ErrUnsupported) {
		t.Fatalf("answer: %v", err)
	}
	_ = f.a.srv.SendKeys(context.Background(), h.TmuxTarget, tmux.KeyEnter) // the human answers in the terminal
	f.waitState(f.a, h, contracts.RunWaitingInput)
	select {
	case a := <-autos:
		if a != contracts.AutomationTrustAccepted {
			t.Fatalf("automation %s", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("trust acceptance not reported")
	}
}

func TestFakeUntrustedWorkspaceNeedsHuman(t *testing.T) {
	f := newFakeEnv(t, func(o *Options) { o.StartupWait = 2 * time.Second })
	h := f.launch("run_e", contracts.ResolvedLaunch{Env: map[string]string{"FAKE_TRUST": "1"}})
	obs := f.waitState(f.a, h, contracts.RunWaitingApproval)
	if !strings.Contains(obs.StateReason, "trust") {
		t.Fatalf("reason %q", obs.StateReason)
	}
}

func TestFakeRecoverAfterRestartAndReconcile(t *testing.T) {
	f := newFakeEnv(t, nil)
	h := f.launch("run_f", contracts.ResolvedLaunch{InitialPrompt: "hello"})
	f.waitObs(f.a, h, 10*time.Second, func(o contracts.Observation) bool { return o.State == contracts.RunWaitingInput && o.NativeRef != nil })
	// A paste that never got Enter (pyred died): reconcile clears it.
	_ = f.a.journal(h, journalRec{MessageID: "msg_F1", AttemptID: "att_1", State: jAttempting, Tail: draftTail("[pyre-msg:msg_F1] stuck draft")})
	_ = f.a.srv.Paste(context.Background(), h.TmuxTarget, "t", []byte("[pyre-msg:msg_F1] stuck draft"))
	time.Sleep(400 * time.Millisecond)
	a2 := f.restart()
	h2, err := a2.Recover(context.Background(), h)
	if err != nil || h2.NativeRef == nil || h2.NativeRef.Kind != NativeKind {
		t.Fatalf("recover: %+v %v", h2, err)
	}
	r, err := a2.Reconcile(context.Background(), h2, "msg_F1")
	if err != nil || r.State != contracts.DeliveryRejected || !strings.Contains(r.Detail, "cleared") {
		t.Fatalf("reconcile: %+v %v", r, err)
	}
	// Another message's draft with the same ending is never cleared as this
	// one's (a reissue): without its own marker the outcome stays unknown.
	_ = a2.journal(h2, journalRec{MessageID: "msg_F2", AttemptID: "att_2", State: jIndeterminate, Tail: draftTail("same ending text")})
	_ = a2.srv.Paste(context.Background(), h2.TmuxTarget, "t2", []byte("[pyre-msg:msg_F3] same ending text"))
	time.Sleep(400 * time.Millisecond)
	if r, _ := a2.Reconcile(context.Background(), h2, "msg_F2"); r.State != contracts.DeliveryIndeterminate {
		t.Fatalf("reconcile by tail only: %+v", r)
	}
	a2.clearInput(context.Background(), h2.TmuxTarget)
	if r, _ := a2.Reconcile(context.Background(), h2, "msg_never"); r.State != contracts.DeliveryRejected {
		t.Fatalf("reconcile unknown: %+v", r)
	}
	// After a stop, Observe reports exited.
	if rc, _ := a2.Stop(context.Background(), h2); rc.Outcome != contracts.OutcomeSucceeded {
		t.Fatalf("stop: %+v", rc)
	}
	if o, _ := a2.Observe(context.Background(), h2, ""); o.State != contracts.RunExited {
		t.Fatalf("after stop: %s", o.State)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
