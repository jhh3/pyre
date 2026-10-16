package claude

import (
	"context"
	"errors"
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
	exe  string
}

func newFakeEnv(t *testing.T, mutate func(*Options)) *fakeEnv {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	if _, err := exec.LookPath("stty"); err != nil {
		t.Skip("stty not installed")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "pc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	o := Options{
		NodeID: "node_t", TmuxSocket: filepath.Join(dir, "tmux", "t.sock"), SSHAlias: "nodet",
		ClaudeBin: exe, HookCommand: []string{"/usr/bin/env", "PYRE_TEST_ROLE=hook", exe},
		ProfileAsConfigDir: true, ApprovalWait: 20 * time.Second, DeliverTimeout: 8 * time.Second,
		StartupWait: 8 * time.Second, StopWait: 5 * time.Second,
	}
	if mutate != nil {
		mutate(&o)
	}
	a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.srv.KillServer(context.Background()) })
	return &fakeEnv{t: t, a: a, opts: o, dir: dir, exe: exe}
}

// restart simulates a daemon restart: a fresh adapter with no memory.
func (f *fakeEnv) restart() *Adapter {
	a, err := New(f.opts)
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

func (f *fakeEnv) launch(runID string, env map[string]string, resume *contracts.NativeRef) contracts.RunHandle {
	f.t.Helper()
	return f.launchIn(runID, filepath.Join(f.dir, "cfg"), env, resume, "")
}

// launchIn launches under a given profile (CLAUDE_CONFIG_DIR) with an
// optional initial prompt.
func (f *fakeEnv) launchIn(runID, profile string, env map[string]string, resume *contracts.NativeRef, prompt string) contracts.RunHandle {
	f.t.Helper()
	work := filepath.Join(f.dir, "work")
	_ = os.MkdirAll(work, 0o755)
	e := map[string]string{"PYRE_TEST_ROLE": "fakeclaude", "PYRE_SESSION_ID": "ses_t"}
	for k, v := range env {
		e[k] = v
	}
	h, err := f.a.Launch(context.Background(), contracts.ResolvedLaunch{
		RunID: runID, SessionID: "ses_t", Harness: contracts.HarnessClaude, Cwd: work, Env: e,
		StateDir: filepath.Join(f.dir, "runs", runID), ProfileDir: profile, Resume: resume, InitialPrompt: prompt,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return h
}

func (f *fakeEnv) waitState(a *Adapter, h contracts.RunHandle, want contracts.RunState, d time.Duration) contracts.Observation {
	f.t.Helper()
	var obs contracts.Observation
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		obs, _ = a.Observe(context.Background(), h, "")
		if obs.State == want {
			return obs
		}
		time.Sleep(100 * time.Millisecond)
	}
	screen, _ := a.srv.CapturePane(context.Background(), h.TmuxTarget, tmux.CaptureOptions{})
	f.t.Fatalf("state %s (%s), want %s\nscreen:\n%s", obs.State, obs.StateReason, want, screen)
	return obs
}

func (f *fakeEnv) waitObs(a *Adapter, h contracts.RunHandle, d time.Duration, pred func(contracts.Observation) bool) contracts.Observation {
	f.t.Helper()
	var obs contracts.Observation
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		obs, _ = a.Observe(context.Background(), h, "")
		if pred(obs) {
			return obs
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.t.Fatalf("condition not met; last observation %+v", obs)
	return obs
}

func deliver(t *testing.T, a *Adapter, h contracts.RunHandle, id, text string, mode contracts.MessageMode) contracts.DeliveryResult {
	t.Helper()
	r, err := a.Deliver(context.Background(), h, contracts.DeliveryInput{MessageID: id, AttemptID: "dlv_" + id, Mode: mode, Text: text})
	if err != nil {
		t.Fatalf("deliver %s: %v", id, err)
	}
	return r
}

func countUserRecords(t *testing.T, a *Adapter, h contracts.RunHandle, id string) int {
	resp, err := a.ReadTranscript(context.Background(), h, contracts.ReadSessionRequest{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range resp.Entries {
		if e.MessageID == id {
			n++
		}
	}
	return n
}

func TestFakeLaunchDeliverTranscript(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	h := f.launch("run_basic", nil, nil)
	obs := f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	if obs.NativeRef == nil || obs.NativeRef.Kind != "claude_session" || obs.NativeRef.ID != h.NativeRef.ID {
		t.Fatalf("native ref %+v vs %+v", obs.NativeRef, h.NativeRef)
	}
	r := deliver(t, f.a, h, "msg_b1", "reply ok", contracts.ModeQueue)
	if r.State != contracts.DeliveryAccepted || r.Evidence != contracts.EvidenceClaudePromptHook || r.EvidenceRef == "" {
		t.Fatalf("deliver %+v", r)
	}
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	// Retrying the same message id does not paste again.
	r = deliver(t, f.a, h, "msg_b1", "reply ok", contracts.ModeQueue)
	if r.State != contracts.DeliveryAccepted || !strings.Contains(r.Detail, "already accepted") {
		t.Fatalf("redeliver %+v", r)
	}
	if n := countUserRecords(t, f.a, h, "msg_b1"); n != 1 {
		t.Fatalf("user records for msg_b1: %d", n)
	}
	// Multi-line input keeps its lines.
	r = deliver(t, f.a, h, "msg_b2", "line one\nline two", contracts.ModeQueue)
	if r.State != contracts.DeliveryAccepted {
		t.Fatalf("multi-line %+v", r)
	}
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	resp, _ := f.a.ReadTranscript(context.Background(), h, contracts.ReadSessionRequest{Tail: true, Limit: 3})
	if len(resp.Entries) == 0 || resp.Screen == "" {
		t.Fatalf("transcript %+v", resp)
	}
	found := false
	for _, e := range resp.Entries {
		if e.MessageID == "msg_b2" && strings.Contains(e.Text, "line one\nline two") {
			found = true
		}
	}
	if !found {
		t.Fatalf("multi-line entry missing: %+v", resp.Entries)
	}
	if _, err := f.a.Deliver(context.Background(), h, contracts.DeliveryInput{MessageID: "msg_i", Mode: contracts.ModeInfo}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("info: %v", err)
	}
	if _, err := f.a.Deliver(context.Background(), h, contracts.DeliveryInput{MessageID: "msg_s", Mode: contracts.ModeSteer}); !contracts.IsCode(err, contracts.ErrUnsupported) {
		t.Fatalf("steer without stop hook: %v", err)
	}
}

func TestFakeBusyQueueAndConsumedReceipt(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	h := f.launch("run_busy", nil, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	obs0, _ := f.a.Observe(context.Background(), h, "")
	r := deliver(t, f.a, h, "msg_long", "FAKE_SLEEP_MS=2500 work", contracts.ModeQueue)
	if r.State != contracts.DeliveryAccepted {
		t.Fatalf("%+v", r)
	}
	f.waitState(f.a, h, contracts.RunRunning, 2*time.Second)
	r = deliver(t, f.a, h, "msg_q", "queued while busy", contracts.ModeQueue)
	if r.State != contracts.DeliveryAccepted || r.Evidence != contracts.EvidenceClaudePromptHook {
		t.Fatalf("busy paste: %+v", r)
	}
	// After the turn, the queued input is consumed: a receipt says so.
	obs := f.waitObs(f.a, h, 10*time.Second, func(o contracts.Observation) bool {
		o2, _ := f.a.Observe(context.Background(), h, obs0.NextCursor)
		for _, rc := range o2.Receipts {
			if rc.MessageID == "msg_q" && strings.HasPrefix(rc.Detail, "consumed_at=") && rc.Evidence == contracts.EvidenceClaudeUserRecord {
				return true
			}
		}
		return false
	})
	_ = obs
	o2, _ := f.a.Observe(context.Background(), h, obs0.NextCursor)
	kinds := map[contracts.EvidenceCategory]bool{}
	for _, rc := range o2.Receipts {
		if rc.MessageID == "msg_q" {
			kinds[rc.Evidence] = true
			if rc.AttemptID != "dlv_msg_q" || rc.RunID != h.RunID || rc.NodeID != "node_t" {
				t.Fatalf("receipt ids %+v", rc)
			}
		}
	}
	if !kinds[contracts.EvidenceClaudeQueue] || !kinds[contracts.EvidenceClaudePromptHook] {
		t.Fatalf("receipt kinds %v", kinds)
	}
	// A cursor at the end yields no repeated receipts.
	o3, _ := f.a.Observe(context.Background(), h, o2.NextCursor)
	if len(o3.Receipts) != 0 {
		t.Fatalf("repeated receipts %+v", o3.Receipts)
	}
}

func TestFakeRefuseWhileHumanAttached(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	h := f.launch("run_lease", nil, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	ctx := context.Background()
	d, err := f.a.Attach(ctx, h, contracts.AttachCommand{RunID: h.RunID})
	if err != nil || d.Mechanism != contracts.AttachLocalTmux {
		t.Fatalf("%+v %v", d, err)
	}
	outer, _ := tmux.NewServer("", filepath.Join(f.dir, "o.sock"))
	defer outer.KillServer(ctx)
	if err := outer.NewSession(ctx, tmux.SessionSpec{Name: "human", Argv: d.Command}); err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		cs, _ := f.a.srv.ListClients(ctx, h.TmuxTarget)
		if tmux.WritableClients(cs) == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	r := deliver(t, f.a, h, "msg_l1", "hello", contracts.ModeQueue)
	if r.State != contracts.DeliveryQueued || !strings.Contains(r.Detail, "human attached") {
		t.Fatalf("expected queued while attached: %+v", r)
	}
	if n := len(lastAttempts(h.StateDir)); n != 0 {
		t.Fatalf("refusal must not journal an attempt: %d", n)
	}
	// A read-only viewer does not block delivery.
	_ = outer.KillServer(ctx)
	ro, _ := f.a.Attach(ctx, h, contracts.AttachCommand{RunID: h.RunID, ReadOnly: true})
	outer2, _ := tmux.NewServer("", filepath.Join(f.dir, "o2.sock"))
	defer outer2.KillServer(ctx)
	_ = outer2.NewSession(ctx, tmux.SessionSpec{Name: "viewer", Argv: ro.Command})
	time.Sleep(500 * time.Millisecond)
	r = deliver(t, f.a, h, "msg_l1", "hello", contracts.ModeQueue)
	if r.State != contracts.DeliveryAccepted {
		t.Fatalf("after detach / read-only viewer: %+v", r)
	}
}

func TestFakeCrashBeforeEnterReconcile(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, func(o *Options) {
		o.testHook = func(p string) error {
			if p == "after_paste" {
				return errors.New("simulated crash before Enter")
			}
			return nil
		}
	})
	h := f.launch("run_crash1", nil, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	if _, err := f.a.Deliver(context.Background(), h, contracts.DeliveryInput{MessageID: "msg_c1", AttemptID: "dlv_1", Text: "first\nsecond line"}); err == nil {
		t.Fatal("expected simulated crash")
	}
	f.opts.testHook = nil
	a2 := f.restart()
	r, err := a2.Reconcile(context.Background(), h, "msg_c1")
	if err != nil || r.State != contracts.DeliveryRejected || !strings.Contains(r.Detail, "cleared") {
		t.Fatalf("reconcile %+v %v", r, err)
	}
	si, _ := a2.screen(context.Background(), h.TmuxTarget)
	if !si.InputEmpty() {
		t.Fatalf("draft not cleared: %q", si.InputLines)
	}
	r = deliver(t, a2, h, "msg_c1", "first\nsecond line", contracts.ModeQueue)
	if r.State != contracts.DeliveryAccepted {
		t.Fatalf("redeliver after clear %+v", r)
	}
	f.waitState(a2, h, contracts.RunWaitingInput, 15*time.Second)
	if n := countUserRecords(t, a2, h, "msg_c1"); n != 1 {
		t.Fatalf("records %d", n)
	}
	// A message never attempted reconciles as rejected (safe).
	if r, _ := a2.Reconcile(context.Background(), h, "msg_never"); r.State != contracts.DeliveryRejected {
		t.Fatalf("never attempted: %+v", r)
	}
}

func TestFakeCrashAfterEnterReconcile(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, func(o *Options) {
		o.testHook = func(p string) error {
			if p == "after_enter" {
				return errors.New("simulated crash after Enter")
			}
			return nil
		}
	})
	h := f.launch("run_crash2", nil, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	if _, err := f.a.Deliver(context.Background(), h, contracts.DeliveryInput{MessageID: "msg_c2", AttemptID: "dlv_2", Text: "hi"}); err == nil {
		t.Fatal("expected simulated crash")
	}
	f.opts.testHook = nil
	a2 := f.restart()
	h2, err := a2.Recover(context.Background(), h)
	if err != nil || h2.PID != h.PID {
		t.Fatalf("recover %+v %v", h2, err)
	}
	var r contracts.DeliveryResult
	for i := 0; i < 30; i++ {
		r, _ = a2.Reconcile(context.Background(), h2, "msg_c2")
		if r.State == contracts.DeliveryAccepted {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if r.State != contracts.DeliveryAccepted {
		t.Fatalf("reconcile after enter %+v", r)
	}
	names, _ := a2.srv.ListSessions(context.Background())
	if len(names) != 1 {
		t.Fatalf("duplicate engine? sessions %v", names)
	}
	// Journal claimed but nothing pasted, no draft: indeterminate (never replay).
	_ = a2.journal(h2, journalRec{MessageID: "msg_c3", AttemptID: "dlv_3", State: jAttempting, Mode: "paste"})
	if r, _ := a2.Reconcile(context.Background(), h2, "msg_c3"); r.State != contracts.DeliveryIndeterminate {
		t.Fatalf("attempting without evidence %+v", r)
	}
}

func TestFakeApprovalBrokerAllowDeny(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	h := f.launch("run_appr", nil, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	ctx := context.Background()
	for _, opt := range []string{"allow", "deny"} {
		r := deliver(t, f.a, h, "msg_p_"+opt, "FAKE_PERMISSION please", contracts.ModeQueue)
		if r.State != contracts.DeliveryAccepted {
			t.Fatalf("%+v", r)
		}
		obs := f.waitObs(f.a, h, 15*time.Second, func(o contracts.Observation) bool {
			return o.State == contracts.RunWaitingApproval && len(o.Approvals) > 0 && o.Approvals[len(o.Approvals)-1].State == contracts.ApprovalPending
		})
		ap := obs.Approvals[len(obs.Approvals)-1]
		if ap.RunID != h.RunID || ap.Body != "ls" || len(ap.Options) != 2 || ap.ExpiresAt == nil {
			t.Fatalf("approval %+v", ap)
		}
		// Input is refused while an approval is pending.
		if r := deliver(t, f.a, h, "msg_blocked", "x", contracts.ModeQueue); r.State != contracts.DeliveryQueued {
			t.Fatalf("delivery during approval: %+v", r)
		}
		other := ap
		other.RunID = "run_other"
		if _, err := f.a.Answer(ctx, h, other, opt, ""); !contracts.IsCode(err, contracts.ErrConflict) {
			t.Fatalf("cross-run answer: %v", err)
		}
		rc, err := f.a.Answer(ctx, h, ap, opt, "")
		if err != nil || !rc.Accepted || !strings.Contains(rc.Detail, "effect=submitted") {
			t.Fatalf("answer %+v %v", rc, err)
		}
		if _, err := f.a.Answer(ctx, h, ap, opt, ""); !contracts.IsCode(err, contracts.ErrConflict) {
			t.Fatalf("duplicate answer: %v", err)
		}
		obs = f.waitObs(f.a, h, 15*time.Second, func(o contracts.Observation) bool {
			a := o.Approvals[len(o.Approvals)-1]
			return o.State == contracts.RunWaitingInput && a.Resolution != nil && a.Resolution.Effect == "effective"
		})
		res := obs.Approvals[len(obs.Approvals)-1].Resolution
		if res.Source != "pyre" || res.OptionID != opt {
			t.Fatalf("resolution %+v", res)
		}
	}
}

func TestFakeApprovalAnsweredInTUI(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	h := f.launch("run_tui", nil, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	ctx := context.Background()
	// TUI "Yes" first: PostToolUse arrives while the broker hook still waits.
	deliver(t, f.a, h, "msg_t1", "FAKE_PERMISSION", contracts.ModeQueue)
	obs := f.waitState(f.a, h, contracts.RunWaitingApproval, 15*time.Second)
	ap := obs.Approvals[0]
	_ = f.a.srv.SendKeys(ctx, h.TmuxTarget, tmux.KeyEnter)
	obs = f.waitObs(f.a, h, 15*time.Second, func(o contracts.Observation) bool {
		return o.Approvals[0].State == contracts.ApprovalResolved
	})
	if obs.Approvals[0].Resolution.Source != "native" {
		t.Fatalf("%+v", obs.Approvals[0].Resolution)
	}
	if _, err := f.a.Answer(ctx, h, ap, "deny", ""); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("late answer must conflict: %v", err)
	}
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	// TUI "No"/Esc: Claude kills the hook; the turn ends without Stop.
	deliver(t, f.a, h, "msg_t2", "FAKE_PERMISSION", contracts.ModeQueue)
	f.waitObs(f.a, h, 15*time.Second, func(o contracts.Observation) bool {
		return len(o.Approvals) == 2 && o.Approvals[1].State == contracts.ApprovalPending
	})
	_ = f.a.srv.SendKeys(ctx, h.TmuxTarget, tmux.KeyEscape)
	obs = f.waitObs(f.a, h, 15*time.Second, func(o contracts.Observation) bool {
		return o.State == contracts.RunWaitingInput && o.Approvals[1].State != contracts.ApprovalPending
	})
	if !strings.Contains(obs.StateReason, "turn_duration") {
		t.Fatalf("expected transcript-derived idle state, got %q", obs.StateReason)
	}
}

func TestFakeApprovalBrokerTimeout(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, func(o *Options) { o.ApprovalWait = 1 * time.Second })
	h := f.launch("run_tmo", nil, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	deliver(t, f.a, h, "msg_to", "FAKE_PERMISSION", contracts.ModeQueue)
	obs := f.waitObs(f.a, h, 6*time.Second, func(o contracts.Observation) bool {
		return len(o.Approvals) == 1 && o.Approvals[0].State == contracts.ApprovalExpired
	})
	if obs.State != contracts.RunWaitingApproval || !strings.Contains(obs.StateReason, "needs human") || !strings.Contains(obs.StateReason, "attach") || !strings.Contains(obs.StateReason, "interrupt") {
		t.Fatalf("%+v", obs)
	}
	if _, err := f.a.Answer(context.Background(), h, obs.Approvals[0], "allow", ""); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("answer after expiry: %v", err)
	}
	_ = f.a.srv.SendKeys(context.Background(), h.TmuxTarget, tmux.KeyEnter) // human answers in the TUI
	f.waitObs(f.a, h, 15*time.Second, func(o contracts.Observation) bool {
		return o.State == contracts.RunWaitingInput && o.Approvals[0].State == contracts.ApprovalResolved
	})
}

func TestFakeSubagentLimitBackgroundAsk(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	h := f.launch("run_misc", nil, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	deliver(t, f.a, h, "msg_sa", "FAKE_SUBAGENT", contracts.ModeQueue)
	obs := f.waitObs(f.a, h, 15*time.Second, func(o contracts.Observation) bool {
		return o.State == contracts.RunWaitingInput && len(o.Subagents) == 1 && o.Subagents[0].State == "stopped"
	})
	if obs.Subagents[0].ID != "agent_fake1" || obs.Subagents[0].Kind != "general-purpose" || obs.Subagents[0].EndedAt == nil {
		t.Fatalf("%+v", obs.Subagents)
	}
	deliver(t, f.a, h, "msg_bg", "FAKE_BG", contracts.ModeQueue)
	f.waitState(f.a, h, contracts.RunWaitingBackground, 15*time.Second)
	// waiting_on_background still accepts input.
	deliver(t, f.a, h, "msg_lim", "FAKE_LIMIT", contracts.ModeQueue)
	obs = f.waitObs(f.a, h, 15*time.Second, func(o contracts.Observation) bool { return o.LimitEvent != nil })
	if obs.LimitEvent.Kind != "usage_limit" || obs.LimitEvent.ResetAt == nil {
		t.Fatalf("%+v", obs.LimitEvent)
	}
	deliver(t, f.a, h, "msg_ask", "FAKE_ASK", contracts.ModeQueue)
	obs = f.waitState(f.a, h, contracts.RunWaitingApproval, 15*time.Second)
	if !strings.Contains(obs.StateReason, "AskUserQuestion") {
		t.Fatalf("%q", obs.StateReason)
	}
	if r := deliver(t, f.a, h, "msg_x", "x", contracts.ModeQueue); r.State != contracts.DeliveryQueued {
		t.Fatalf("delivery during question dialog: %+v", r)
	}
}

func TestFakeInterrupt(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	h := f.launch("run_int", nil, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	ctx := context.Background()
	if rc, _ := f.a.Interrupt(ctx, h); rc.Outcome != contracts.OutcomeSucceeded || !strings.Contains(rc.Detail, "nothing") {
		t.Fatalf("idle interrupt %+v", rc)
	}
	deliver(t, f.a, h, "msg_long", "FAKE_SLEEP_MS=20000", contracts.ModeQueue)
	f.waitState(f.a, h, contracts.RunRunning, 3*time.Second)
	rc, err := f.a.Interrupt(ctx, h)
	if err != nil || rc.Outcome != contracts.OutcomeSucceeded {
		t.Fatalf("interrupt %+v %v", rc, err)
	}
	obs, _ := f.a.Observe(ctx, h, "")
	if obs.State != contracts.RunWaitingInput || !strings.Contains(obs.StateReason, "turn_duration") {
		t.Fatalf("%+v", obs)
	}
}

func TestFakeTrustDialog(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, func(o *Options) { o.StartupWait = 2 * time.Second })
	h := f.launch("run_trust1", map[string]string{"FAKE_TRUST_DIALOG": "1"}, nil)
	obs := f.waitObs(f.a, h, 15*time.Second, func(o contracts.Observation) bool {
		return strings.Contains(o.StateReason, "needs human: workspace trust")
	})
	// A blocking startup dialog is "needs human" (waiting on a prompt in the
	// terminal), not idle at the prompt.
	if obs.State != contracts.RunWaitingApproval {
		t.Fatalf("%+v", obs)
	}
	if r := deliver(t, f.a, h, "msg_tr", "x", contracts.ModeQueue); r.State != contracts.DeliveryQueued {
		t.Fatalf("deliver into trust dialog: %+v", r)
	}
	if ready, why := f.a.InputReady(context.Background(), h, contracts.ModeQueue); ready || why == "" {
		t.Fatalf("InputReady with a startup dialog: %v %q", ready, why)
	}
	// Owner-trusted directory: the adapter accepts the dialog.
	g := newFakeEnv(t, func(o *Options) { o.TrustWorkspace = func(string) bool { return true } })
	h2 := g.launch("run_trust2", map[string]string{"FAKE_TRUST_DIALOG": "1"}, nil)
	g.waitObs(g.a, h2, 6*time.Second, func(o contracts.Observation) bool {
		return o.State == contracts.RunWaitingInput && strings.Contains(o.StateReason, "session started")
	})
}

func TestFakeStopHookSteer(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, func(o *Options) { o.StopHookSteer = true })
	h := f.launch("run_steer", nil, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	if !contracts.HasCap(f.a.Capabilities(), contracts.CapInputStopHook) {
		t.Fatal("cap")
	}
	deliver(t, f.a, h, "msg_w", "FAKE_SLEEP_MS=1500", contracts.ModeQueue)
	f.waitState(f.a, h, contracts.RunRunning, 3*time.Second)
	r := deliver(t, f.a, h, "msg_steer", "change course", contracts.ModeSteer)
	if r.State != contracts.DeliveryAccepted || r.Evidence != contracts.EvidenceClaudeStopHook {
		t.Fatalf("steer %+v", r)
	}
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	// Steer while idle is an ordinary paste: accepted on the prompt hook
	// or the transcript's user record, whichever the adapter sees first
	// (on a slow runner the record can win), never through the Stop hook.
	r = deliver(t, f.a, h, "msg_steer2", "now", contracts.ModeSteer)
	if r.State != contracts.DeliveryAccepted ||
		(r.Evidence != contracts.EvidenceClaudePromptHook && r.Evidence != contracts.EvidenceClaudeUserRecord) {
		t.Fatalf("idle steer %+v", r)
	}
}

func TestFakeStopResumeRecover(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	h := f.launch("run_sr1", nil, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	deliver(t, f.a, h, "msg_r1", "remember", contracts.ModeQueue)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	ctx := context.Background()
	rc, err := f.a.Stop(ctx, h)
	if err != nil || rc.Outcome != contracts.OutcomeSucceeded || !strings.Contains(rc.Detail, "/exit") {
		t.Fatalf("stop %+v %v", rc, err)
	}
	obs, _ := f.a.Observe(ctx, h, "")
	if obs.State != contracts.RunExited || obs.ExitCode == nil || *obs.ExitCode != 0 {
		t.Fatalf("after stop %+v", obs)
	}
	if _, err := f.a.Recover(ctx, h); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("recover stopped run: %v", err)
	}
	prev := contracts.Run{RunID: h.RunID, NativeRef: obs.NativeRef}
	kind, ref, err := f.a.ResumeSpec(ctx, prev, true)
	if err != nil || kind != contracts.ContinuationNativeResume || ref.ID != h.NativeRef.ID {
		t.Fatalf("resume spec %v %+v %v", kind, ref, err)
	}
	if kind, ref, _ := f.a.ResumeSpec(ctx, prev, false); kind != contracts.ContinuationHandoff || ref != nil {
		t.Fatalf("cross-account %v %+v", kind, ref)
	}
	h2 := f.launch("run_sr2", nil, ref)
	obs = f.waitState(f.a, h2, contracts.RunWaitingInput, 15*time.Second)
	if obs.NativeRef.ID != h.NativeRef.ID || !strings.Contains(obs.StateReason, "resume") {
		t.Fatalf("resumed %+v", obs)
	}
	// Daemon restart: a fresh adapter recovers without relaunching and can deliver.
	a2 := f.restart()
	h3, err := a2.Recover(ctx, h2)
	if err != nil || h3.PID != h2.PID {
		t.Fatalf("recover %+v %v", h3, err)
	}
	if r := deliver(t, a2, h3, "msg_r2", "after restart", contracts.ModeQueue); r.State != contracts.DeliveryAccepted {
		t.Fatalf("%+v", r)
	}
	names, _ := a2.srv.ListSessions(ctx)
	if len(names) != 1 || names[0] != "run_sr2" {
		t.Fatalf("sessions %v", names)
	}
}

// Archiving a session whose Claude has a background task running (Pyre's
// message monitor) must not leave Claude's exit dialog for the owner.
func TestFakeStopConfirmsBackgroundTasksDialog(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	h := f.launch("run_bgexit", map[string]string{"FAKE_BG_ON_EXIT": "1"}, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	deliver(t, f.a, h, "msg_bg1", "hello", contracts.ModeQueue)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	rc, err := f.a.Stop(context.Background(), h)
	if err != nil || rc.Outcome != contracts.OutcomeSucceeded || !strings.Contains(rc.Detail, "/exit") {
		t.Fatalf("stop %+v %v", rc, err)
	}
}

// waitDialogGone waits until the fake's permission dialog left the screen.
func (f *fakeEnv) waitDialogGone(h contracts.RunHandle) {
	f.t.Helper()
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		if si, err := f.a.screen(context.Background(), h.TmuxTarget); err == nil && si.Clear() && si.Working() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.t.Fatal("dialog did not close")
}

// A TUI "Yes" fires no hook until the tool finishes (C14): while a long
// tool runs, the next observation reports running and the request resolved
// in the terminal, before or after the remote answer window expired.
func TestFakeApprovalTUIYesLongTool(t *testing.T) {
	t.Parallel()
	for _, expire := range []bool{false, true} {
		expire := expire
		t.Run(map[bool]string{false: "pending", true: "expired"}[expire], func(t *testing.T) {
			t.Parallel()
			f := newFakeEnv(t, func(o *Options) {
				if expire {
					o.ApprovalWait = time.Second
				}
			})
			h := f.launch("run_tuilong", nil, nil)
			f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
			deliver(t, f.a, h, "msg_tl", "FAKE_PERMISSION FAKE_TOOL_MS=15000", contracts.ModeQueue)
			want := contracts.ApprovalPending
			if expire {
				want = contracts.ApprovalExpired
			}
			obs := f.waitObs(f.a, h, 15*time.Second, func(o contracts.Observation) bool {
				return len(o.Approvals) == 1 && o.Approvals[0].State == want
			})
			// The real dialog keeps flagging on every observation.
			for i := 0; i < 2; i++ {
				obs, _ = f.a.Observe(context.Background(), h, "")
				if obs.State != contracts.RunWaitingApproval || obs.Approvals[0].State != want {
					t.Fatalf("dialog up: %s %q %s", obs.State, obs.StateReason, obs.Approvals[0].State)
				}
				time.Sleep(100 * time.Millisecond)
			}
			_ = f.a.srv.SendKeys(context.Background(), h.TmuxTarget, tmux.KeyEnter)
			f.waitDialogGone(h)
			obs, _ = f.a.Observe(context.Background(), h, "")
			ap := obs.Approvals[0]
			if obs.State != contracts.RunRunning || ap.State != contracts.ApprovalResolved || ap.Resolution.Source != "native" {
				t.Fatalf("one observation after the TUI answer: %s %q %+v", obs.State, obs.StateReason, ap)
			}
			if _, err := f.a.Answer(context.Background(), h, ap, "deny", ""); err == nil {
				t.Fatal("late answer accepted")
			}
		})
	}
}
