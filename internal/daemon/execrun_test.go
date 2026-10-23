package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon/daemontest"
	"github.com/jhh3/pyre/internal/runtime/claude"
	"github.com/jhh3/pyre/internal/runtime/codex"
)

func postLocal(t *testing.T, srv *httptest.Server, path string, body any, out any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

// pyre run: the node binds the account and records the run without
// starting anything; the CLI claims the binding once with its secret,
// reports the child and its exit, and the run ends exited with that code
// and the Claude session id. No agent guide, MCP server or environment
// generation applies.
func TestHeadlessExecRun(t *testing.T) {
	w := daemontest.NewWorld()
	fa := daemontest.New(w, contracts.HarnessClaude)
	base, err := claude.New(claude.Options{NodeID: "node_test", TmuxSocket: shortSock(t), HookCommand: []string{"true"}, ProfileAsConfigDir: true})
	if err != nil {
		t.Fatal(err)
	}
	env := &fakeEnv{}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{fa}, ExecAdapters: []contracts.RuntimeAdapter{claude.NewExec(base)},
		Accounts: &fakeBinder{}, Environments: env, AgentMCP: []string{"/synthetic/pyre", "mcp"}})
	srv := httptest.NewServer(n.d.LocalHandler())
	defer srv.Close()

	var info ExecInfo
	resp, err := http.Get(srv.URL + PathLocalExec)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("exec info: %v %v", resp, err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&info)
	resp.Body.Close()
	if info.NodeID != "node_test" || len(info.Harnesses) != 1 || info.Harnesses[0] != "claude" {
		t.Fatalf("info = %+v", info)
	}

	secret := "synthetic-claim-secret"
	sc := startCmd("ses_x", "run_x", contracts.HarnessClaude, t.TempDir())
	sc.Session.Tags = []string{"pyre run"}
	sc.Args = []string{ExecArg + "=" + claimHash(secret)}
	if res := n.do(command(contracts.CmdStartRun, sc)); res.Error != nil {
		t.Fatal(res.Error)
	}
	n.d.WaitLaunches()
	r := n.run("run_x").Run
	if r.Adapter != claude.ExecAdapterName || r.AdapterData[adapterDataMode] != modeHeadless || contracts.HasCap(r.Capabilities, contracts.CapInputQueue) {
		t.Fatalf("run = %+v", r)
	}
	if _, ok := w.Proc("run_x"); ok {
		t.Fatal("a process was started for a headless run")
	}
	if len(env.got) != 0 {
		t.Fatal("environment generation resolved for a headless run")
	}

	if code := postLocal(t, srv, PathLocalExecClaim, ExecClaimRequest{RunID: "run_x", Secret: "wrong", ClientPID: os.Getpid()}, nil); code != http.StatusNotFound {
		t.Fatalf("wrong secret: %d", code)
	}
	var b ExecBinding
	if code := postLocal(t, srv, PathLocalExecClaim, ExecClaimRequest{RunID: "run_x", Secret: secret, ClientPID: os.Getpid()}, &b); code != 200 {
		t.Fatalf("claim: %d", code)
	}
	if b.AccountID != "acct_1" || b.Env["CLAUDE_CONFIG_DIR"] != "/profiles/acct_1" || len(b.Unset) != 1 || b.SessionID != "ses_x" {
		t.Fatalf("binding = %+v", b)
	}
	for k := range b.Env {
		if k == "PYRE_SESSION_TOKEN" || k == "PYRE_SESSION_ID" {
			t.Fatalf("pyre session env in a headless binding: %v", b.Env)
		}
	}
	if code := postLocal(t, srv, PathLocalExecClaim, ExecClaimRequest{RunID: "run_x", Secret: secret}, nil); code != http.StatusConflict {
		t.Fatalf("second claim: %d", code)
	}
	if code := postLocal(t, srv, PathLocalExecStarted, ExecProgress{RunID: "run_x", Secret: secret, PID: os.Getpid()}, nil); code != 200 {
		t.Fatalf("started: %d", code)
	}
	n.d.PollNow(context.Background(), "run_x") // the endpoint reports in the background
	if got := n.run("run_x").Run; got.State != contracts.RunRunning {
		t.Fatalf("running state = %s (%s)", got.State, got.StateReason)
	}
	code3, sid := 3, "0b7c1f3e-1111-4222-8333-444455556666"
	if code := postLocal(t, srv, PathLocalExecExited, ExecProgress{RunID: "run_x", Secret: secret, ExitCode: &code3, NativeID: sid}, nil); code != 200 {
		t.Fatalf("exited: %d", code)
	}
	waitFor(t, "exit reported", func() bool {
		n.d.PollNow(context.Background(), "run_x")
		return n.run("run_x").Run.State == contracts.RunExited
	})
	got := n.run("run_x")
	if got.Run.State != contracts.RunExited || got.Run.ExitCode == nil || *got.Run.ExitCode != 3 || got.Run.NativeRef == nil || got.Run.NativeRef.ID != sid {
		t.Fatalf("ended run = %+v", got.Run)
	}
	var reported bool
	for _, rp := range n.rep.Reports(contracts.ReportRun) {
		var run contracts.Run
		_ = json.Unmarshal(rp.Payload, &run)
		reported = reported || run.RunID == "run_x" && run.State == contracts.RunExited && run.ExitCode != nil && *run.ExitCode == 3
	}
	if !reported {
		t.Fatal("exit not reported to the hub")
	}
}

// A headless start for a harness without an exec adapter is refused.
func TestHeadlessExecUnsupported(t *testing.T) {
	w := daemontest.NewWorld()
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{daemontest.New(w, contracts.HarnessClaude)}, Accounts: &fakeBinder{}})
	sc := startCmd("ses_u", "run_u", contracts.HarnessClaude, t.TempDir())
	sc.Args = []string{ExecArg + "=" + claimHash("x")}
	if res := n.do(command(contracts.CmdStartRun, sc)); res.Error == nil || res.Error.Code != contracts.ErrUnsupported {
		t.Fatalf("res = %+v", res)
	}
}

// The CLI claims as soon as the hub recorded the start; the claim waits for
// the start command to reach the node.
func TestHeadlessClaimBeforeStart(t *testing.T) {
	w := daemontest.NewWorld()
	base, err := claude.New(claude.Options{NodeID: "node_test", TmuxSocket: shortSock(t), HookCommand: []string{"true"}, ProfileAsConfigDir: true})
	if err != nil {
		t.Fatal(err)
	}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{daemontest.New(w, contracts.HarnessClaude)},
		ExecAdapters: []contracts.RuntimeAdapter{claude.NewExec(base)}, Accounts: &fakeBinder{}})
	srv := httptest.NewServer(n.d.LocalHandler())
	defer srv.Close()
	got := make(chan int, 1)
	var b ExecBinding
	go func() {
		got <- postLocal(t, srv, PathLocalExecClaim, ExecClaimRequest{RunID: "run_early", Secret: "early-secret", ClientPID: os.Getpid()}, &b)
	}()
	time.Sleep(300 * time.Millisecond)
	sc := startCmd("ses_early", "run_early", contracts.HarnessClaude, t.TempDir())
	sc.Args = []string{ExecArg + "=" + claimHash("early-secret")}
	if res := n.do(command(contracts.CmdStartRun, sc)); res.Error != nil {
		t.Fatal(res.Error)
	}
	select {
	case code := <-got:
		if code != 200 || b.AccountID != "acct_1" {
			t.Fatalf("early claim: %d %+v", code, b)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("early claim never answered")
	}
}

type ownerLocator struct {
	contracts.RuntimeAdapter
	shared bool
	acct   string
	def    bool
	asked  string
}

func (o *ownerLocator) ResumeOwner(cwd, sid string) (bool, string, bool) {
	o.asked = sid
	return o.shared, o.acct, o.def
}

// A headless --resume binds the account that reaches the conversation:
// any (shared store), the profile's own account, or the default login; an
// explicit --account is kept.
func TestHeadlessResumePolicy(t *testing.T) {
	w := daemontest.NewWorld()
	loc := &ownerLocator{RuntimeAdapter: daemontest.New(w, contracts.HarnessClaude)}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{loc}, Accounts: &fakeBinder{}})
	elig := contracts.AccountPolicy{Mode: contracts.AccountEligible}
	sid := "0b7c1f3e-1111-4222-8333-444455556666"
	loc.shared = true
	if p := n.d.execPolicy(context.Background(), contracts.HarnessClaude, elig, "/w", sid); p.Mode != contracts.AccountEligible || loc.asked != sid {
		t.Fatalf("shared: %+v", p)
	}
	loc.shared, loc.acct = false, "acct_synthB"
	if p := n.d.execPolicy(context.Background(), contracts.HarnessClaude, elig, "/w", sid); p.Mode != contracts.AccountExplicit || p.AccountID != "acct_synthB" || !p.Strict {
		t.Fatalf("own profile: %+v", p)
	}
	loc.acct, loc.def = "", true
	if p := n.d.execPolicy(context.Background(), contracts.HarnessClaude, elig, "/w", sid); p.Mode != contracts.AccountAmbient {
		t.Fatalf("default login: %+v", p)
	}
	explicit := contracts.AccountPolicy{Mode: contracts.AccountExplicit, AccountID: "acct_mine"}
	if p := n.d.execPolicy(context.Background(), contracts.HarnessClaude, explicit, "/w", sid); p.AccountID != "acct_mine" {
		t.Fatalf("explicit: %+v", p)
	}
	if p := n.d.execPolicy(context.Background(), contracts.HarnessClaude, elig, "/w", ""); p.Mode != contracts.AccountEligible {
		t.Fatalf("no resume: %+v", p)
	}
}

type refusingBinder struct{ fakeBinder }

func (*refusingBinder) Bind(ctx context.Context, r AccountBindRequest) (AccountBinding, error) {
	return AccountBinding{}, contracts.Errorf(contracts.ErrCapacityExhausted, "no eligible anthropic subscription (synthetic)")
}

// A headless start the node refuses answers the CLI's claim at once with
// the reason (the CLI then runs the command without Pyre).
func TestHeadlessRefusedStartAnswersClaim(t *testing.T) {
	w := daemontest.NewWorld()
	base, err := claude.New(claude.Options{NodeID: "node_test", TmuxSocket: shortSock(t), HookCommand: []string{"true"}, ProfileAsConfigDir: true})
	if err != nil {
		t.Fatal(err)
	}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{daemontest.New(w, contracts.HarnessClaude)},
		ExecAdapters: []contracts.RuntimeAdapter{claude.NewExec(base)}, Accounts: &refusingBinder{}})
	srv := httptest.NewServer(n.d.LocalHandler())
	defer srv.Close()
	sc := startCmd("ses_r", "run_refused", contracts.HarnessClaude, t.TempDir())
	sc.Args = []string{ExecArg + "=" + claimHash("s")}
	if res := n.do(command(contracts.CmdStartRun, sc)); res.Error == nil {
		t.Fatal("start accepted")
	}
	start := time.Now()
	if code := postLocal(t, srv, PathLocalExecClaim, ExecClaimRequest{RunID: "run_refused", Secret: "s"}, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("claim of a refused start: %d", code)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("claim waited for the timeout")
	}
}

// shortSock is a tmux socket path under the macOS 100-byte limit (never
// started: headless runs use no tmux).
func shortSock(t *testing.T) string {
	d, err := os.MkdirTemp("", "px")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return filepath.Join(d, "t.sock")
}

// codexBinder binds a Codex profile the way accounts does: CODEX_HOME, and
// both harnesses' login variables to remove.
type codexBinder struct{ fakeBinder }

func (*codexBinder) Bind(ctx context.Context, r AccountBindRequest) (AccountBinding, error) {
	return AccountBinding{ProfileDir: "/profiles/acct_c/codex", Env: map[string]string{"CODEX_HOME": "/profiles/acct_c/codex"},
		Unset:      []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"},
		Assignment: &contracts.AccountAssignment{AssignmentID: "asg_c", AccountID: "acct_c"}}, nil
}

// pyre run -- codex exec: the node binds a Codex account (CODEX_HOME) and
// records the run on the codex exec adapter; the binding removes only
// Codex's own login variables (a codex keeps the caller's Anthropic key for
// its tools); the run ends with the thread the CLI saw. A later resume of
// that thread from the shared store prefers the account that ran it.
func TestHeadlessCodexExecRun(t *testing.T) {
	w := daemontest.NewWorld()
	base, err := codex.New(codex.Options{NodeID: "node_test", TmuxSocket: shortSock(t), StateRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	loc := &ownerLocator{RuntimeAdapter: daemontest.New(w, contracts.HarnessCodex), shared: true}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{loc}, ExecAdapters: []contracts.RuntimeAdapter{codex.NewExec(base)},
		Accounts: &codexBinder{}, AgentMCP: []string{"/synthetic/pyre", "mcp"}})
	srv := httptest.NewServer(n.d.LocalHandler())
	defer srv.Close()

	secret := "synthetic-codex-secret"
	sc := startCmd("ses_cx", "run_cx", contracts.HarnessCodex, t.TempDir())
	sc.Session.Tags = []string{"pyre run"}
	sc.Args = []string{ExecArg + "=" + claimHash(secret)}
	if res := n.do(command(contracts.CmdStartRun, sc)); res.Error != nil {
		t.Fatal(res.Error)
	}
	n.d.WaitLaunches()
	r := n.run("run_cx").Run
	if r.Adapter != codex.ExecAdapterName || r.Harness != contracts.HarnessCodex || r.AdapterData[adapterDataMode] != modeHeadless {
		t.Fatalf("run = %+v", r)
	}
	if _, ok := w.Proc("run_cx"); ok {
		t.Fatal("a process was started for a headless run")
	}
	var b ExecBinding
	if code := postLocal(t, srv, PathLocalExecClaim, ExecClaimRequest{RunID: "run_cx", Secret: secret, ClientPID: os.Getpid()}, &b); code != 200 {
		t.Fatalf("claim: %d", code)
	}
	slices.Sort(b.Unset)
	if b.AccountID != "acct_c" || b.Env["CODEX_HOME"] != "/profiles/acct_c/codex" || b.Env["CLAUDE_CONFIG_DIR"] != "" ||
		!slices.Equal(b.Unset, []string{"CODEX_ACCESS_TOKEN", "CODEX_API_KEY", "OPENAI_API_KEY"}) {
		t.Fatalf("binding = %+v", b)
	}
	for k := range b.Env {
		if strings.HasPrefix(k, "PYRE_") {
			t.Fatalf("pyre session env in a headless binding: %v", b.Env)
		}
	}
	if code := postLocal(t, srv, PathLocalExecStarted, ExecProgress{RunID: "run_cx", Secret: secret, PID: os.Getpid()}, nil); code != 200 {
		t.Fatalf("started: %d", code)
	}
	zero, thread := 0, "0199a213-81c0-7800-8aa1-bbab2a035a53"
	if code := postLocal(t, srv, PathLocalExecExited, ExecProgress{RunID: "run_cx", Secret: secret, ExitCode: &zero, NativeID: thread}, nil); code != 200 {
		t.Fatalf("exited: %d", code)
	}
	waitFor(t, "exit reported", func() bool {
		n.d.PollNow(context.Background(), "run_cx")
		return n.run("run_cx").Run.State == contracts.RunExited
	})
	got := n.run("run_cx").Run
	if got.ExitCode == nil || *got.ExitCode != 0 || got.NativeRef == nil || got.NativeRef.Kind != "codex_thread" || got.NativeRef.ID != thread {
		t.Fatalf("ended run = %+v", got)
	}

	elig := contracts.AccountPolicy{Mode: contracts.AccountEligible}
	if p := n.d.execPolicy(context.Background(), contracts.HarnessCodex, elig, "/w", thread); p.Mode != contracts.AccountEligible ||
		!slices.Equal(p.Prefer, []string{"acct_c"}) || loc.asked != thread {
		t.Fatalf("codex shared resume: %+v", p)
	}
	if p := n.d.execPolicy(context.Background(), contracts.HarnessCodex, elig, "/w", "0199a213-0000-7000-8000-00000000beef"); len(p.Prefer) != 0 {
		t.Fatalf("unknown thread: %+v", p)
	}
	loc.shared, loc.acct = false, "acct_own"
	if p := n.d.execPolicy(context.Background(), contracts.HarnessCodex, elig, "/w", thread); p.Mode != contracts.AccountExplicit || p.AccountID != "acct_own" || !p.Strict {
		t.Fatalf("codex own profile: %+v", p)
	}
}

// The conversation a headless command resumes (--pyre-exec-resume) reaches
// the launch: the run carries it as its native id from the start, for
// claude and codex alike.
func TestHeadlessResumeIsRunNative(t *testing.T) {
	cb, err := claude.New(claude.Options{NodeID: "node_test", TmuxSocket: shortSock(t), HookCommand: []string{"true"}, ProfileAsConfigDir: true})
	if err != nil {
		t.Fatal(err)
	}
	xb, err := codex.New(codex.Options{NodeID: "node_test", TmuxSocket: shortSock(t), StateRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	w := daemontest.NewWorld()
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{daemontest.New(w, contracts.HarnessClaude), daemontest.New(w, contracts.HarnessCodex)},
		ExecAdapters: []contracts.RuntimeAdapter{claude.NewExec(cb), codex.NewExec(xb)}, Accounts: &fakeBinder{}})
	for h, want := range map[contracts.Harness]contracts.NativeRef{
		contracts.HarnessClaude: {Kind: "claude_session", ID: "0b7c1f3e-1111-4222-8333-444455556666"},
		contracts.HarnessCodex:  {Kind: "codex_thread", ID: "0199a213-81c0-7800-8aa1-bbab2a035a53"},
	} {
		runID := "run_res" + string(h)
		sc := startCmd("ses_res"+string(h), runID, h, t.TempDir())
		sc.Args = []string{ExecArg + "=" + claimHash("s-"+string(h)), ExecResumeArg + "=" + want.ID}
		if res := n.do(command(contracts.CmdStartRun, sc)); res.Error != nil {
			t.Fatal(res.Error)
		}
		n.d.WaitLaunches()
		if got := n.run(runID).Run.NativeRef; got == nil || *got != want {
			t.Fatalf("%s: native ref %+v, want %+v", h, got, want)
		}
	}
}
