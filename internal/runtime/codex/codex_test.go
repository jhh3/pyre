package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/accounts"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/fileperm"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// WP13 consumes ReadRateLimits through this interface.
var _ accounts.CodexRateLimitReader = (*Adapter)(nil)

type env struct {
	t    *testing.T
	a    *Adapter
	o    Options
	dir  string
	exe  string
	home string
}

func newEnv(t *testing.T, mutate func(*Options)) *env {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	exe, _ := os.Executable()
	dir, err := os.MkdirTemp("", "px")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	o := Options{NodeID: "node_x", TmuxSocket: filepath.Join(dir, "tmux", "t.sock"), StateRoot: filepath.Join(dir, "state"),
		CodexBin: exe, AppServerEnv: map[string]string{"PYRE_TEST_ROLE": "fakeappserver"}, DisableTUI: true,
		DeliverTimeout: 5 * time.Second, InterruptTimeout: 2 * time.Second, SSHAlias: "nodex"}
	if mutate != nil {
		mutate(&o)
	}
	a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.srv.KillServer(context.Background()) })
	home := filepath.Join(dir, "home-a")
	_ = os.MkdirAll(home, 0o700)
	return &env{t: t, a: a, o: o, dir: dir, exe: exe, home: home}
}

func (e *env) restart() *Adapter {
	a, err := New(e.o)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *env) launch(a *Adapter, runID string, home string, resume *contracts.NativeRef, extra map[string]string) contracts.RunHandle {
	e.t.Helper()
	work := filepath.Join(e.dir, "work")
	_ = os.MkdirAll(work, 0o755)
	h, err := a.Launch(context.Background(), contracts.ResolvedLaunch{RunID: runID, SessionID: "ses_x", Harness: contracts.HarnessCodex,
		Cwd: work, StateDir: filepath.Join(e.dir, "runs", runID), ProfileDir: home, Resume: resume, Env: extra,
		Unset: []string{"OPENAI_API_KEY", "CODEX_API_KEY"}})
	if err != nil {
		e.t.Fatal(err)
	}
	return h
}

func (e *env) wait(a *Adapter, h contracts.RunHandle, d time.Duration, pred func(contracts.Observation) bool) contracts.Observation {
	e.t.Helper()
	var obs contracts.Observation
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		obs, _ = a.Observe(context.Background(), h, "")
		if pred(obs) {
			return obs
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatalf("condition not met; last %+v", obs)
	return obs
}

func idle(o contracts.Observation) bool { return o.State == contracts.RunWaitingInput }

func dlv(t *testing.T, a *Adapter, h contracts.RunHandle, id, text string, mode contracts.MessageMode) contracts.DeliveryResult {
	t.Helper()
	r, err := a.Deliver(context.Background(), h, contracts.DeliveryInput{MessageID: id, AttemptID: "dlv_" + id, Mode: mode, Text: text})
	if err != nil {
		t.Fatalf("deliver %s: %v", id, err)
	}
	return r
}

func starts(t *testing.T, e *env, home string) int {
	b, _ := os.ReadFile(filepath.Join(e.o.StateRoot, "codex", homeKey(home), "fake-starts.log"))
	return strings.Count(string(b), "\n")
}

func TestCodexLaunchDeliverReceipts(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	h := e.launch(e.a, "run_c1", e.home, nil, nil)
	if h.NativeRef == nil || h.NativeRef.Kind != "codex_thread" {
		t.Fatalf("%+v", h)
	}
	obs0 := e.wait(e.a, h, 5*time.Second, idle)
	r := dlv(t, e.a, h, "msg_1", "reply ok", contracts.ModeQueue)
	if r.State != contracts.DeliveryAccepted || r.Evidence != contracts.EvidenceCodexUserMessage || r.EvidenceRef == "" {
		t.Fatalf("%+v", r)
	}
	obs := e.wait(e.a, h, 5*time.Second, idle)
	o2, _ := e.a.Observe(context.Background(), h, obs0.NextCursor)
	if len(o2.Receipts) != 1 || o2.Receipts[0].MessageID != "msg_1" || o2.Receipts[0].AttemptID != "dlv_msg_1" {
		t.Fatalf("receipts %+v", o2.Receipts)
	}
	if o3, _ := e.a.Observe(context.Background(), h, o2.NextCursor); len(o3.Receipts) != 0 {
		t.Fatal("repeated receipts")
	}
	_ = obs
	// Same id again: found, not re-sent (Codex itself does not dedupe).
	r = dlv(t, e.a, h, "msg_1", "reply ok", contracts.ModeQueue)
	if r.State != contracts.DeliveryAccepted || !strings.Contains(r.Detail, "not re-sent") {
		t.Fatalf("%+v", r)
	}
	if r, _ := e.a.Reconcile(context.Background(), h, "msg_1"); r.State != contracts.DeliveryAccepted {
		t.Fatalf("%+v", r)
	}
	resp, err := e.a.ReadTranscript(context.Background(), h, contracts.ReadSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, en := range resp.Entries {
		if en.MessageID == "msg_1" {
			n++
		}
	}
	if n != 1 || len(resp.Entries) < 2 {
		t.Fatalf("transcript %+v", resp.Entries)
	}
	if !resp.Entries[0].At.Equal(time.Unix(1791100000, 0)) {
		t.Fatalf("entry time is not the turn start: %v", resp.Entries[0].At)
	}
	if _, err := e.a.Deliver(context.Background(), h, contracts.DeliveryInput{MessageID: "msg_i", Mode: contracts.ModeInfo}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatal(err)
	}
}

func TestCodexBusyQueueSteerInterrupt(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	h := e.launch(e.a, "run_c2", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_long", "FAKE_SLEEP_MS=20000", contracts.ModeQueue)
	e.wait(e.a, h, 5*time.Second, func(o contracts.Observation) bool { return o.State == contracts.RunRunning })
	if r := dlv(t, e.a, h, "msg_q", "later", contracts.ModeQueue); r.State != contracts.DeliveryQueued {
		t.Fatalf("busy queue: %+v", r)
	}
	r := dlv(t, e.a, h, "msg_s", "steer now", contracts.ModeSteer)
	if r.State != contracts.DeliveryAccepted || r.Evidence != contracts.EvidenceCodexUserMessage {
		t.Fatalf("steer: %+v", r)
	}
	rc, err := e.a.Interrupt(context.Background(), h)
	if err != nil || rc.Outcome != contracts.OutcomeSucceeded {
		t.Fatalf("interrupt %+v %v", rc, err)
	}
	e.wait(e.a, h, 5*time.Second, idle)
	if rc, _ := e.a.Interrupt(context.Background(), h); rc.Outcome != contracts.OutcomeSucceeded || !strings.Contains(rc.Detail, "nothing") {
		t.Fatalf("idle interrupt %+v", rc)
	}
	// Now idle: the queued message can be delivered.
	if r := dlv(t, e.a, h, "msg_q", "later", contracts.ModeQueue); r.State != contracts.DeliveryAccepted {
		t.Fatalf("%+v", r)
	}
}

func TestCodexApprovals(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	h := e.launch(e.a, "run_c3", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	ctx := context.Background()
	dlv(t, e.a, h, "msg_a1", "FAKE_APPROVAL", contracts.ModeQueue)
	obs := e.wait(e.a, h, 5*time.Second, func(o contracts.Observation) bool {
		return o.State == contracts.RunWaitingApproval && len(o.Approvals) == 1
	})
	ap := obs.Approvals[0]
	ids := []string{}
	for _, op := range ap.Options {
		ids = append(ids, op.ID)
	}
	if ap.Body != "touch approved.txt" || strings.Join(ids, ",") != "accept,acceptWithExecpolicyAmendment,cancel" || !strings.HasSuffix(ap.RequestID, ":0:exec-appr") {
		t.Fatalf("approval %+v ids %v", ap, ids)
	}
	if r := dlv(t, e.a, h, "msg_blocked", "x", contracts.ModeQueue); r.State != contracts.DeliveryQueued {
		t.Fatalf("delivery during approval: %+v", r)
	}
	rc, err := e.a.Answer(ctx, h, ap, "allow", "")
	if err != nil || rc.Outcome != contracts.OutcomeSucceeded || !strings.Contains(rc.Detail, "effect=effective") {
		t.Fatalf("answer %+v %v", rc, err)
	}
	if _, err := e.a.Answer(ctx, h, ap, "accept", ""); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("duplicate answer: %v", err)
	}
	obs = e.wait(e.a, h, 5*time.Second, idle)
	if res := obs.Approvals[0].Resolution; res == nil || res.Source != "pyre" || res.Effect != "effective" || res.OptionID != "accept" {
		t.Fatalf("resolution %+v", obs.Approvals[0])
	}
	// Another client (the native TUI) answers first: superseded.
	dlv(t, e.a, h, "msg_a2", "FAKE_APPROVAL", contracts.ModeQueue)
	obs = e.wait(e.a, h, 5*time.Second, func(o contracts.Observation) bool {
		return len(o.Approvals) == 2 && o.Approvals[1].State == contracts.ApprovalPending
	})
	ap2 := obs.Approvals[1]
	rr, _ := e.a.run(h)
	other, err := dialRPC(ctx, rr.hs.sock, nil, func(id json.RawMessage, method string, _ json.RawMessage) {})
	if err != nil {
		t.Fatal(err)
	}
	defer other.close()
	_ = other.call(ctx, "initialize", initializeParams{ClientInfo: clientInfo{Name: "tui", Version: "0"}}, nil)
	parts := strings.Split(ap2.RequestID, ":")
	_ = other.respond(ctx, json.RawMessage(parts[1]), map[string]any{"decision": "cancel"})
	obs = e.wait(e.a, h, 5*time.Second, func(o contracts.Observation) bool { return o.Approvals[1].State == contracts.ApprovalResolved })
	if res := obs.Approvals[1].Resolution; res.Source != "native" {
		t.Fatalf("%+v", res)
	}
	if _, err := e.a.Answer(ctx, h, ap2, "accept", ""); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("late answer: %v", err)
	}
}

func TestCodexCrashWindowReconcile(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	h := e.launch(e.a, "run_c4", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	ctx := context.Background()
	// The adapter journals, sends turn/start, and dies before the response.
	if err := e.a.journal(h, journalRec{MessageID: "msg_crash", AttemptID: "dlv_c", State: jAttempting, Mode: "turn/start"}); err != nil {
		t.Fatal(err)
	}
	rr, _ := e.a.run(h)
	raw, err := dialRPC(ctx, rr.hs.sock, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = raw.call(ctx, "initialize", initializeParams{ClientInfo: clientInfo{Name: "x", Version: "0"}}, nil)
	_ = raw.write(ctx, rpcMessage{ID: json.RawMessage("99"), Method: "turn/start",
		Params: json.RawMessage(`{"threadId":"` + h.NativeRef.ID + `","clientUserMessageId":"msg_crash","input":[{"type":"text","text":"[pyre-msg:msg_crash] hi"}]}`)})
	raw.close()
	a2 := e.restart()
	h2, err := a2.Recover(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	var r contracts.DeliveryResult
	for i := 0; i < 40; i++ {
		if r, _ = a2.Reconcile(ctx, h2, "msg_crash"); r.State == contracts.DeliveryAccepted {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if r.State != contracts.DeliveryAccepted || r.Evidence != contracts.EvidenceCodexUserMessage {
		t.Fatalf("reconcile %+v", r)
	}
	// Retrying a message whose outcome is unknown is refused (no auto-replay).
	_ = a2.journal(h2, journalRec{MessageID: "msg_unknown", AttemptID: "dlv_u", State: jIndeterminate})
	if r := dlv(t, a2, h2, "msg_unknown", "x", contracts.ModeQueue); r.State != contracts.DeliveryIndeterminate {
		t.Fatalf("unknown outcome retry: %+v", r)
	}
	if r, _ := a2.Reconcile(ctx, h2, "msg_unknown"); r.State != contracts.DeliveryIndeterminate {
		t.Fatalf("%+v", r)
	}
	if r, _ := a2.Reconcile(ctx, h2, "msg_never"); r.State != contracts.DeliveryRejected {
		t.Fatalf("never attempted %+v", r)
	}
	// Accepted by protocol ack only (item not echoed): still accepted.
	r = dlv(t, a2, h2, "msg_noitem", "FAKE_NO_ITEM", contracts.ModeQueue)
	if r.State != contracts.DeliveryAccepted || r.Evidence != contracts.EvidenceProtocolAck {
		t.Fatalf("%+v", r)
	}
	if starts(t, e, e.home) != 1 {
		t.Fatalf("app-server restarted: %d starts", starts(t, e, e.home))
	}
}

func TestCodexSubagentLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	h := e.launch(e.a, "run_c5", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_sa", "FAKE_SUBAGENT", contracts.ModeQueue)
	obs := e.wait(e.a, h, 5*time.Second, func(o contracts.Observation) bool {
		return idle(o) && len(o.Subagents) == 1 && o.Subagents[0].State == "stopped"
	})
	if obs.Subagents[0].Kind != "worker" {
		t.Fatalf("%+v", obs.Subagents)
	}
	dlv(t, e.a, h, "msg_lim", "FAKE_LIMIT", contracts.ModeQueue)
	obs = e.wait(e.a, h, 5*time.Second, func(o contracts.Observation) bool { return o.LimitEvent != nil })
	if obs.LimitEvent.Kind != "usage_limit" {
		t.Fatalf("%+v", obs.LimitEvent)
	}
}

func TestCodexSharedServerStopResumeRateLimits(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	ctx := context.Background()
	h1 := e.launch(e.a, "run_s1", e.home, nil, nil)
	h2 := e.launch(e.a, "run_s2", e.home, nil, nil)
	if starts(t, e, e.home) != 1 {
		t.Fatalf("one app-server per home expected, got %d", starts(t, e, e.home))
	}
	dlv(t, e.a, h1, "msg_s1", "hi", contracts.ModeQueue)
	e.wait(e.a, h1, 5*time.Second, idle)
	rc, _ := e.a.Stop(ctx, h1)
	if rc.Outcome != contracts.OutcomeSucceeded || !strings.Contains(rc.Detail, "kept") {
		t.Fatalf("stop 1 %+v", rc)
	}
	if obs, _ := e.a.Observe(ctx, h1, ""); obs.State != contracts.RunExited {
		t.Fatalf("%+v", obs)
	}
	if _, err := e.a.Recover(ctx, h1); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("recover stopped: %v", err)
	}
	kind, ref, _ := e.a.ResumeSpec(ctx, contracts.Run{NativeRef: h1.NativeRef}, true)
	if kind != contracts.ContinuationNativeResume || ref.ID != h1.NativeRef.ID {
		t.Fatal(kind)
	}
	if k, _, _ := e.a.ResumeSpec(ctx, contracts.Run{NativeRef: h1.NativeRef}, false); k != contracts.ContinuationHandoff {
		t.Fatal(k)
	}
	h3 := e.launch(e.a, "run_s3", e.home, ref, nil)
	if h3.NativeRef.ID != h1.NativeRef.ID {
		t.Fatalf("resume changed id %s", h3.NativeRef.ID)
	}
	if r := dlv(t, e.a, h3, "msg_s3", "after resume", contracts.ModeQueue); r.State != contracts.DeliveryAccepted {
		t.Fatalf("%+v", r)
	}
	// Rate limits through the same app-server (no second process).
	raw, err := e.a.ReadRateLimits(ctx, e.home)
	if err != nil || !strings.Contains(string(raw), `"usedPercent":12`) {
		t.Fatalf("rate limits %s %v", raw, err)
	}
	if starts(t, e, e.home) != 1 {
		t.Fatal("rate-limit read started another app-server")
	}
	_, _ = e.a.Stop(ctx, h2)
	rc, _ = e.a.Stop(ctx, h3)
	if !strings.Contains(rc.Detail, "app-server stopped") {
		t.Fatalf("last stop %+v", rc)
	}
	if ok, _ := e.a.srv.HasSession(ctx, "codex-as-"+homeKey(e.home)); ok {
		t.Fatal("app-server still running")
	}
	// A probe on an idle home starts the server only for the read.
	other := filepath.Join(e.dir, "home-b")
	_ = os.MkdirAll(other, 0o700)
	raw, err = e.a.ReadRateLimits(ctx, other)
	if err != nil || !strings.Contains(string(raw), "rateLimits") {
		t.Fatalf("%s %v", raw, err)
	}
	if ok, _ := e.a.srv.HasSession(ctx, "codex-as-"+homeKey(other)); ok {
		t.Fatal("probe left the app-server running")
	}
	ai, err := e.a.ReadAccount(ctx, other)
	if err != nil || !ai.LoggedIn || ai.PlanType != "pro" {
		t.Fatalf("%+v %v", ai, err)
	}
	b, _ := json.Marshal(ai)
	if strings.Contains(string(b), "@") {
		t.Fatal("account info leaked an email")
	}
	if _, err := os.Stat(filepath.Join(other, "fake-refreshed")); err == nil {
		t.Fatal("account/read without refreshToken must not refresh")
	}
	// RefreshLogin: a managed-auth refresh through the home's app-server only.
	if err := e.a.RefreshLogin(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, "fake-refreshed")); err != nil {
		t.Fatal("RefreshLogin did not ask the app-server for a token refresh")
	}
	if ok, _ := e.a.srv.HasSession(ctx, "codex-as-"+homeKey(other)); ok {
		t.Fatal("refresh left the app-server running")
	}
}

func TestCodexRecoverAfterDaemonRestart(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	ctx := context.Background()
	h := e.launch(e.a, "run_r1", e.home, nil, nil)
	dlv(t, e.a, h, "msg_r1", "one", contracts.ModeQueue)
	e.wait(e.a, h, 5*time.Second, idle)
	a2 := e.restart()
	h2, err := a2.Recover(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	e.wait(a2, h2, 5*time.Second, idle)
	if r := dlv(t, a2, h2, "msg_r1", "one", contracts.ModeQueue); r.State != contracts.DeliveryAccepted || !strings.Contains(r.Detail, "not re-sent") {
		t.Fatalf("evidence must be rebuilt after restart: %+v", r)
	}
	if r := dlv(t, a2, h2, "msg_r2", "two", contracts.ModeQueue); r.State != contracts.DeliveryAccepted {
		t.Fatalf("%+v", r)
	}
	if starts(t, e, e.home) != 1 {
		t.Fatal("recover launched a second app-server")
	}
}

// TestCodexThreadIdentityConfig: the shared app-server cannot carry a run's
// Pyre identity in its environment, so thread/start carries it as per-thread
// config (shell env + `pyre mcp`), persisted 0600 and re-sent on resume
// after a pyred restart.
func TestCodexThreadIdentityConfig(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	ctx := context.Background()
	env := map[string]string{"PYRE_SESSION_ID": "ses_x", "PYRE_RUN_ID": "run_c1", "PYRE_SOCKET": "/synthetic/pyred.sock",
		"PYRE_SESSION_TOKEN": "synthetic-session-token-0000", "PYRE_CLI": "/synthetic/bin/pyre", "UNRELATED": "nope"}
	h := e.launch(e.a, "run_c1", e.home, nil, env)
	logPath := filepath.Join(e.o.StateRoot, "codex", homeKey(e.home), "fake-config.log")
	readLog := func() []string {
		b, _ := os.ReadFile(logPath)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	lines := readLog()
	method, raw, _ := strings.Cut(lines[0], " ")
	var cfg map[string]any
	if method != "thread/start" || json.Unmarshal([]byte(raw), &cfg) != nil {
		t.Fatalf("thread/start config: %v", lines)
	}
	if cfg["shell_environment_policy.set.PYRE_SESSION_TOKEN"] != "synthetic-session-token-0000" ||
		cfg["shell_environment_policy.set.PYRE_SOCKET"] != "/synthetic/pyred.sock" ||
		cfg["shell_environment_policy.set.PYRE_CLI"] != "/synthetic/bin/pyre" {
		t.Fatalf("shell env overrides: %v", cfg)
	}
	if _, ok := cfg["shell_environment_policy.set.UNRELATED"]; ok {
		t.Fatal("non-Pyre variable forwarded")
	}
	mcp, _ := cfg["mcp_servers.pyre"].(map[string]any)
	menv, _ := mcp["env"].(map[string]any)
	if mcp["command"] != "/synthetic/bin/pyre" || fmt.Sprint(mcp["args"]) != "[mcp]" || menv["PYRE_SESSION_ID"] != "ses_x" || menv["PYRE_SESSION_TOKEN"] == nil {
		t.Fatalf("mcp server override: %v", mcp)
	}
	st, err := os.Stat(filepath.Join(h.StateDir, threadConfigFile))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("thread config file: %v %v", st, err)
	}
	// pyred restart: the new adapter re-subscribes with the same config.
	a2 := e.restart()
	if _, err := a2.Recover(ctx, h); err != nil {
		t.Fatal(err)
	}
	lines = readLog()
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "thread/resume ") || !strings.Contains(last, `"mcp_servers.pyre"`) || !strings.Contains(last, "synthetic-session-token-0000") {
		t.Fatalf("resume after restart lacks the identity config: %v", lines)
	}
	if _, err := a2.Stop(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.StateDir, threadConfigFile)); !os.IsNotExist(err) {
		t.Fatalf("thread config (session token) kept after stop: %v", err)
	}
	// No Pyre session: no overrides (e.g. the account probe path).
	if threadConfig(map[string]string{"PATH": "/bin"}) != nil {
		t.Fatal("config without a Pyre session")
	}
}

func TestCodexTUIAttach(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *Options) { o.DisableTUI = false })
	ctx := context.Background()
	h := e.launch(e.a, "run_t1", e.home, nil, map[string]string{"PYRE_TEST_ROLE": "faketui"})
	waitScreen := func(want string) {
		t.Helper()
		for i := 0; i < 100; i++ {
			s, _ := e.a.srv.CapturePane(ctx, h.RunID, tmux.CaptureOptions{})
			if strings.Contains(s, want) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		s, _ := e.a.srv.CapturePane(ctx, h.RunID, tmux.CaptureOptions{})
		t.Fatalf("screen lacks %q:\n%s", want, s)
	}
	waitScreen("FAKE CODEX TUI attached to " + h.NativeRef.ID)
	// The TUI shares the thread: it sees our turn.
	dlv(t, e.a, h, "msg_t", "hello", contracts.ModeQueue)
	waitScreen("notify turn/completed")
	d, err := e.a.Attach(ctx, h, contracts.AttachCommand{RunID: h.RunID, ClientNodeID: "node_y"})
	if err != nil || d.Mechanism != contracts.AttachSSHTmux || d.Command[2] != "nodex" {
		t.Fatalf("%+v %v", d, err)
	}
	// Human quits the TUI: the thread lives on; Attach respawns the TUI.
	_ = e.a.srv.SendKeys(ctx, h.RunID, tmux.KeyCtrlD)
	for i := 0; i < 60; i++ {
		if p, _ := e.a.srv.Pane(ctx, h.RunID); p.Dead {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// (The first observation may report the unseen turn as working.)
	e.wait(e.a, h, 2*time.Second, idle) // the thread is not affected by the TUI exit
	if _, err := e.a.Attach(ctx, h, contracts.AttachCommand{RunID: h.RunID}); err != nil {
		t.Fatal(err)
	}
	if p, _ := e.a.srv.Pane(ctx, h.RunID); p.Dead {
		t.Fatal("TUI not respawned")
	}
	waitScreen("FAKE CODEX TUI attached")
}

// TestCodexTUITrust: before the native TUI starts, a trusted cwd (owner's
// --trust-dir) is recorded in the app-server's config (the remote TUI asks
// "Trust this folder?" from there); untrusted cwds are not.
func TestCodexTUITrust(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *Options) {
		o.DisableTUI = false
		o.TrustWorkspace = func(cwd string) bool { return strings.HasSuffix(cwd, "/work") }
	})
	h := e.launch(e.a, "run_tr", e.home, nil, map[string]string{"PYRE_TEST_ROLE": "faketui"})
	b, _ := os.ReadFile(filepath.Join(e.o.StateRoot, "codex", homeKey(e.home), "fake-config.log"))
	var write string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "config/value/write ") {
			write = l
		}
	}
	work := filepath.Join(e.dir, "work")
	if !strings.Contains(write, `"keyPath":"projects"`) || !strings.Contains(write, `"mergeStrategy":"upsert"`) ||
		!strings.Contains(write, `"`+work+`":{"trust_level":"trusted"}`) {
		t.Fatalf("trust not written before the TUI: %q", write)
	}
	_ = h
	e2 := newEnv(t, func(o *Options) {
		o.DisableTUI = false
		o.TrustWorkspace = func(string) bool { return false }
	})
	e2.launch(e2.a, "run_tr2", e2.home, nil, map[string]string{"PYRE_TEST_ROLE": "faketui"})
	b, _ = os.ReadFile(filepath.Join(e2.o.StateRoot, "codex", homeKey(e2.home), "fake-config.log"))
	if strings.Contains(string(b), `"keyPath":"projects"`) {
		t.Fatal("untrusted cwd written as trusted")
	}
	// The TUI's own turns get the run's permissions (it does not inherit
	// the thread's): sandbox_mode is written for the profile home.
	if !strings.Contains(string(b), `"keyPath":"sandbox_mode"`) {
		t.Fatalf("run permissions not written for the TUI: %q", string(b))
	}
}

func TestCodexUnits(t *testing.T) {
	a := &Adapter{o: Options{ApprovalPolicy: "on-request", Sandbox: "workspace-write"}}
	m, ap, sb, err := a.parseArgs([]string{"-m", "gpt-x", "--sandbox=read-only", "-a", "never"})
	if err != nil || m != "gpt-x" || ap != "never" || sb != "read-only" {
		t.Fatal(m, ap, sb, err)
	}
	if _, _, _, err := a.parseArgs([]string{"--dangerously-bypass-approvals-and-sandbox"}); err == nil {
		t.Fatal("disallowed arg accepted")
	}
	if le := limitFromError(turnError{Message: "x", CodexErrorInfo: json.RawMessage(`"unauthorized"`)}, time.Now()); le == nil || le.Kind != "auth_invalid" {
		t.Fatal(le)
	}
	if limitFromError(turnError{Message: "context window", CodexErrorInfo: json.RawMessage(`"contextWindowExceeded"`)}, time.Now()) != nil {
		t.Fatal("false positive")
	}
	ts := newThreadState("run_u", "ses_u", "thr")
	pa := newPending(ts, "as1", json.RawMessage("5"), "item/tool/requestUserInput",
		approvalParams{ItemID: "i", Questions: []struct {
			ID       string `json:"id"`
			Header   string `json:"header"`
			Question string `json:"question"`
			Options  []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options,omitempty"`
		}{{ID: "q1", Header: "Color", Question: "Pick"}}}, time.Now())
	if pa.approval.Kind != contracts.ApprovalQuestion || pa.approval.RequestID != "as1:5:i" {
		t.Fatalf("%+v", pa.approval)
	}
	if got := string(questionAnswer(pa.params, "", "blue")); got != `{"answers":{"q1":{"answers":["blue"]}}}` {
		t.Fatal(got)
	}
	if homeKey("") != "default" || homeKey("/a") == homeKey("/b") {
		t.Fatal("home key")
	}
}

// HomeBusy reports whether the Pyre app-server for a CODEX_HOME is running
// (accounts' rota check-in waits for it).
func TestCodexHomeBusy(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	ctx := context.Background()
	if busy, err := e.a.HomeBusy(ctx, e.home); err != nil || busy {
		t.Fatalf("before launch: %v %v", busy, err)
	}
	h := e.launch(e.a, "run_hb", e.home, nil, nil)
	if busy, err := e.a.HomeBusy(ctx, e.home); err != nil || !busy {
		t.Fatalf("while running: %v %v", busy, err)
	}
	other := filepath.Join(e.dir, "home-idle")
	if busy, err := e.a.HomeBusy(ctx, other); err != nil || busy {
		t.Fatalf("other home: %v %v", busy, err)
	}
	if _, err := e.a.Stop(ctx, h); err != nil {
		t.Fatal(err)
	}
	if busy, err := e.a.HomeBusy(ctx, e.home); err != nil || busy {
		t.Fatalf("after last stop: %v %v", busy, err)
	}
}

// Codex runs had no change stamp, so a backed-off observer noticed a turn
// end or an approval request after a quiet stretch only on its next doubled
// poll. The stamp moves with every app-server notification for the run's
// thread, so the daemon wakes the observer at once.
func TestCodexChangeStampMovesWithNotifications(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	h := e.launch(e.a, "run_cs", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	s0 := e.a.ChangeStamp(h)
	if s0 == "" {
		t.Fatal("no change stamp for a live codex run")
	}
	if r := dlv(t, e.a, h, "msg_cs", "reply ok", contracts.ModeQueue); r.State != contracts.DeliveryAccepted {
		t.Fatalf("%+v", r)
	}
	e.wait(e.a, h, 5*time.Second, idle)
	s1 := e.a.ChangeStamp(h)
	if s1 == s0 {
		t.Fatalf("stamp did not move across a turn: %q", s1)
	}
	if s2 := e.a.ChangeStamp(h); s2 != s1 {
		t.Fatalf("stamp moved without notifications: %q -> %q", s1, s2)
	}
	if s := e.a.ChangeStamp(contracts.RunHandle{RunID: "run_unknown"}); s != "" {
		t.Fatalf("unknown run stamp %q", s)
	}
}

// TestCodexTUITrustPromptAnswered: when the TUI still shows Codex's
// "Trust this folder?" (an ambient run on the owner's own CODEX_HOME, which
// Pyre never writes itself, or an older Codex), the adapter answers "Trust
// and continue" only for a cwd under --trust-dir.
func TestCodexTUITrustPromptAnswered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	screenOf := func(e *env, h contracts.RunHandle, want string) (string, bool) {
		var s string
		for i := 0; i < 100; i++ {
			s, _ = e.a.srv.CapturePane(ctx, h.RunID, tmux.CaptureOptions{})
			if strings.Contains(s, want) {
				return s, true
			}
			time.Sleep(50 * time.Millisecond)
		}
		return s, false
	}
	env := map[string]string{"PYRE_TEST_ROLE": "faketui", "PYRE_FAKE_TRUST_PROMPT": "1"}
	e := newEnv(t, func(o *Options) {
		o.DisableTUI = false
		o.TrustWorkspace = func(cwd string) bool { return strings.HasSuffix(cwd, "/work") }
	})
	h := e.launch(e.a, "run_tp", "", nil, env) // ambient: no profile config write
	if s, ok := screenOf(e, h, "FAKE CODEX TUI attached"); !ok || !strings.Contains(s, "FAKE TRUST ANSWERED") {
		t.Fatalf("trust prompt not answered:\n%s", s)
	}
	if b, _ := os.ReadFile(filepath.Join(e.o.StateRoot, "codex", homeKey(""), "fake-config.log")); strings.Contains(string(b), "config/value/write") {
		t.Fatal("ambient home config written by Pyre")
	}

	e2 := newEnv(t, func(o *Options) {
		o.DisableTUI = false
		o.TrustWorkspace = func(string) bool { return false }
	})
	h2 := e2.launch(e2.a, "run_tp2", "", nil, env)
	if s, ok := screenOf(e2, h2, "Trust and continue"); !ok {
		t.Fatalf("no prompt:\n%s", s)
	}
	time.Sleep(time.Second)
	if s, _ := e2.a.srv.CapturePane(ctx, h2.RunID, tmux.CaptureOptions{}); !isCodexTrustScreen(s) || strings.Contains(s, "FAKE TRUST ANSWERED") {
		t.Fatalf("untrusted cwd answered:\n%s", s)
	}
}

func TestClassifyTrustScreen(t *testing.T) {
	real := "  Folder access\n  /tmp/x\n  Trust this folder? Codex can read, edit, and run files here.\n› 1. Trust and continue\n  2. Back to Agent Command Center\n  enter continue · esc back\n"
	if classifyTrustScreen(real) != trustSelected {
		t.Fatal("selected")
	}
	if classifyTrustScreen(strings.Replace(strings.Replace(real, "› 1.", "  1.", 1), "  2.", "› 2.", 1)) != trustOther {
		t.Fatal("other")
	}
	if classifyTrustScreen(">_ OpenAI Codex (v0.160.1)\n› Ask Codex to do anything\n") != trustNone {
		t.Fatal("conversation")
	}
}

// TestCodexReadKeepsSettledPages: sessions.read lists only the turn pages
// after the last page whose turns all finished, and answers exactly what a
// listing from the first turn answers.
func TestCodexReadKeepsSettledPages(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.a.turnsPage = 2
	h := e.launch(e.a, "run_pages", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	for i := 0; i < 7; i++ {
		dlv(t, e.a, h, fmt.Sprintf("msg_p%d", i), fmt.Sprintf("synthetic turn %d", i), contracts.ModeQueue)
		e.wait(e.a, h, 5*time.Second, idle)
	}
	rr, err := e.a.run(h)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	logPath := filepath.Join(e.home, "fake-turns-list.log")
	calls := func() []string {
		b, _ := os.ReadFile(logPath)
		_ = os.Remove(logPath)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	// reference: a listing from the first turn, nothing kept.
	ref := func(req contracts.ReadSessionRequest) []contracts.TranscriptEntry {
		rr.readMu.Lock()
		rr.turns = nil
		rr.readMu.Unlock()
		resp, err := e.a.ReadTranscript(ctx, h, req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Entries
	}
	read := func(req contracts.ReadSessionRequest) []contracts.TranscriptEntry {
		resp, err := e.a.ReadTranscript(ctx, h, req)
		if err != nil || resp.Freshness.Stale {
			t.Fatalf("read: %v %+v", err, resp.Freshness)
		}
		return resp.Entries
	}
	reqs := []contracts.ReadSessionRequest{{Limit: 500}, {Tail: true, Limit: 3}, {Cursor: "4", Limit: 3}, {Cursor: "2", Tail: true, Limit: 500}}
	for _, req := range reqs {
		want := ref(req)
		if len(want) == 0 {
			t.Fatalf("%+v: no entries", req)
		}
		calls()
		read(req) // keeps the settled pages
		calls()
		if got := read(req); !reflect.DeepEqual(got, want) {
			t.Fatalf("%+v: kept pages answer %+v, want %+v", req, got, want)
		}
		// 7 turns in pages of 2: the first three pages are full and
		// finished; only the last one is listed again.
		if c := calls(); len(c) != 1 || !strings.HasPrefix(c[0], "c6 ") {
			t.Fatalf("%+v: listed %v, want only the page after the kept ones", req, c)
		}
	}
	// A new turn is picked up.
	dlv(t, e.a, h, "msg_p7", "synthetic turn 7", contracts.ModeQueue)
	e.wait(e.a, h, 5*time.Second, idle)
	got := read(contracts.ReadSessionRequest{Limit: 500})
	if want := ref(contracts.ReadSessionRequest{Limit: 500}); !reflect.DeepEqual(got, want) || got[len(got)-1].Seq <= 7 {
		t.Fatalf("after a new turn: %+v, want %+v", got, want)
	}
	// A kept cursor the app-server no longer knows: listed from the start.
	read(contracts.ReadSessionRequest{Limit: 500})
	rr.readMu.Lock()
	rr.turns.cursor = "c99"
	rr.readMu.Unlock()
	if got2 := read(contracts.ReadSessionRequest{Limit: 500}); !reflect.DeepEqual(got2, got) {
		t.Fatalf("after an unknown cursor: %+v, want %+v", got2, got)
	}
}

// Starting an app-server makes its socket directory and <StateRoot>/codex
// private to the user through fileperm.UserOnlyDir, also when they already
// exist with looser permissions (Windows installs whose socket dir inherited
// user+SYSTEM, which codex refuses; 0700 on Unix).
func TestCodexSocketDirUserOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	root := filepath.Join(e.o.StateRoot, "codex")
	sockDir := filepath.Join(root, homeKey(e.home))
	if err := os.MkdirAll(sockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(root, 0o755)
	_ = os.Chmod(sockDir, 0o755)
	var mu sync.Mutex
	var calls []string
	e.a.userOnlyDir = func(dir string) error {
		mu.Lock()
		calls = append(calls, dir)
		mu.Unlock()
		return fileperm.UserOnlyDir(dir)
	}
	h := e.launch(e.a, "run_ud", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	if want := []string{root, sockDir}; !reflect.DeepEqual(got, want) {
		t.Fatalf("userOnlyDir calls %q, want %q", got, want)
	}
	for _, d := range []string{root, sockDir} {
		if st, err := os.Stat(d); err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v, want 0700", d, st.Mode(), err)
		}
	}

	// A start that cannot secure the directory fails instead of launching
	// an app-server that would refuse its socket.
	e2 := newEnv(t, nil)
	e2.a.userOnlyDir = func(string) error { return errors.New("acl denied") }
	_, err := e2.a.Launch(context.Background(), contracts.ResolvedLaunch{RunID: "run_ud2", SessionID: "ses_x", Harness: contracts.HarnessCodex,
		Cwd: e2.dir, StateDir: filepath.Join(e2.dir, "runs", "run_ud2"), ProfileDir: e2.home})
	if err == nil || !strings.Contains(err.Error(), "acl denied") {
		t.Fatalf("launch err %v, want acl denied", err)
	}
}
