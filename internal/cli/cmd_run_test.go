//go:build !windows

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// The test binary doubles as a fake claude (invoked through a symlink named
// "claude") and as the pyre process under test (PYRE_TEST_ROLE=pyrerun), so
// pyre run sees real stdio: pipes, exit statuses and signals.
const fakeSessionID = "6f3c2a1e-8b7d-4c5e-9f0a-1b2c3d4e5f60"

func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "claude" {
		fakeClaudeMain()
		return
	}
	if filepath.Base(os.Args[0]) == "codex" {
		fakeCodexMain()
		return
	}
	if os.Getenv("PYRE_TEST_ROLE") == "pyrerun" {
		var args []string
		_ = json.Unmarshal([]byte(os.Getenv("PYRE_TEST_ARGS")), &args)
		a := NewApp()
		if os.Getenv("PYRE_TEST_NOAPI") == "" { // else: no hub login, as resolved from the environment
			a.API = &runAPI{log: os.Getenv("PYRE_TEST_STARTLOG")}
		}
		os.Exit(a.Main(context.Background(), args))
	}
	os.Exit(m.Run())
}

func fakeClaudeMain() {
	in, _ := io.ReadAll(os.Stdin)
	if p := os.Getenv("FAKE_OUT"); p != "" {
		rec := map[string]any{"args": os.Args[1:], "config_dir": os.Getenv("CLAUDE_CONFIG_DIR"), "api_key": os.Getenv("ANTHROPIC_API_KEY")}
		b, _ := json.Marshal(rec)
		_ = os.WriteFile(p, b, 0o600)
	}
	if p := os.Getenv("FAKE_PID"); p != "" {
		_ = os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	if d := os.Getenv("FAKE_BG"); d != "" {
		bg := exec.Command("sleep", d) // a background tool holding stdout
		bg.Stdout = os.Stdout
		_ = bg.Start()
	}
	os.Stdout.WriteString(`{"type":"result","subtype":"success","is_error":false,"result":"OK","session_id":"` + fakeSessionID + `"}` + "\n")
	os.Stdout.Write(in) // raw bytes back (byte-for-byte check)
	os.Stderr.WriteString("synthetic stderr line\n")
	if d, err := time.ParseDuration(os.Getenv("FAKE_SLEEP")); err == nil {
		time.Sleep(d)
	}
	code, _ := strconv.Atoi(os.Getenv("FAKE_EXIT"))
	os.Exit(code)
}

// fakeThreadID is the synthetic Codex thread of the fake codex.
const fakeThreadID = "0199a213-81c0-7800-8aa1-bbab2a035a53"

// fakeCodexMain is a fake `codex exec --json`: the thread.started event
// first, stdin back, a line on stderr; it records its argv and the
// environment pyre run gave it.
func fakeCodexMain() {
	in, _ := io.ReadAll(os.Stdin)
	if p := os.Getenv("FAKE_OUT"); p != "" {
		rec := map[string]any{"args": os.Args[1:], "codex_home": os.Getenv("CODEX_HOME"), "openai_key": os.Getenv("OPENAI_API_KEY"),
			"anthropic_key": os.Getenv("ANTHROPIC_API_KEY"), "session_token": os.Getenv("PYRE_SESSION_TOKEN"), "run_id": os.Getenv("PYRE_RUN_ID"),
			"session_id": os.Getenv("PYRE_SESSION_ID"), "hook_state": os.Getenv("PYRE_CLAUDE_STATE_DIR"), "socket": os.Getenv("PYRE_SOCKET")}
		b, _ := json.Marshal(rec)
		_ = os.WriteFile(p, b, 0o600)
	}
	os.Stdout.WriteString(`{"type":"thread.started","thread_id":"` + fakeThreadID + `"}` + "\n")
	os.Stdout.WriteString(`{"type":"turn.started"}` + "\n")
	os.Stdout.WriteString(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"OK"}}` + "\n")
	os.Stdout.Write(in)
	os.Stderr.WriteString("synthetic codex stderr\n")
	code, _ := strconv.Atoi(os.Getenv("FAKE_EXIT"))
	os.Exit(code)
}

// runAPI is the hub as pyre run sees it: StartSession only.
type runAPI struct {
	c.API
	log string
}

func (r *runAPI) GetNode(_ context.Context, req c.GetRequest) (c.Node, error) {
	return c.Node{NodeID: req.ID, Connection: c.NodeConnection{State: c.ConnConnected}}, nil
}

func (r *runAPI) StartSession(_ context.Context, req c.StartSessionRequest) (c.StartSessionResponse, error) {
	b, _ := json.Marshal(req)
	_ = os.WriteFile(r.log, b, 0o600)
	return c.StartSessionResponse{Session: c.SessionView{Session: c.Session{SessionID: "ses_synth-run"},
		Run: &c.Run{RunID: "run_synth-run", SessionID: "ses_synth-run", State: c.RunStarting}}}, nil
}

// fakeNode serves the node's local exec endpoints on a unix socket.
type fakeNode struct {
	binding *execBinding // the claim's answer (default: a claude profile)
	refuse  bool         // the claim answers "no eligible subscription"
	start   bool         // records runs itself with the node credential (localExecStartPath)
	deny    bool         // the start answers forbidden (the hub refused the node)
	mu      sync.Mutex
	starts  []map[string]any
	claims  []map[string]any
	started []map[string]any
	exited  []map[string]any
}

func (f *fakeNode) serve(t *testing.T, sock string) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+localExecPath, func(w http.ResponseWriter, r *http.Request) {
		info := map[string]any{"node_id": "node_synth-local", "harnesses": []string{"claude"}}
		if f.start {
			info["start"] = true
		}
		_ = json.NewEncoder(w).Encode(info)
	})
	rec := func(dst *[]map[string]any, reply any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			f.mu.Lock()
			*dst = append(*dst, m)
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(reply)
		}
	}
	b := execBinding{RunID: "run_synth-run", SessionID: "ses_synth-run", AccountID: "acct_synth-a",
		ProfileDir: "/synthetic/profiles/acct_synth-a/claude", Env: map[string]string{"CLAUDE_CONFIG_DIR": "/synthetic/profiles/acct_synth-a/claude"},
		Unset: []string{"ANTHROPIC_API_KEY"}}
	if f.binding != nil {
		b = *f.binding
	}
	claim := rec(&f.claims, b)
	mux.HandleFunc("POST "+localExecClaimPath, func(w http.ResponseWriter, r *http.Request) {
		if f.refuse {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": c.Errorf(c.ErrCapacityExhausted, "no eligible anthropic subscription on this node (synthetic)")})
			return
		}
		claim(w, r)
	})
	start := rec(&f.starts, map[string]string{"session_id": "ses_synth-run", "run_id": "run_synth-run"})
	mux.HandleFunc("POST "+localExecStartPath, func(w http.ResponseWriter, r *http.Request) {
		if !f.start {
			http.NotFound(w, r)
			return
		}
		if f.deny {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": c.Errorf(c.ErrForbidden, "a node credential starts only headless pyre run sessions on its own node (synthetic)")})
			return
		}
		start(w, r)
	})
	mux.HandleFunc("POST "+localExecStartedPath, rec(&f.started, map[string]string{}))
	mux.HandleFunc("POST "+localExecExitedPath, rec(&f.exited, map[string]string{}))
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

type runEnv struct {
	dir, bin, sock, startLog, fakeOut string
}

func newRunEnv(t *testing.T) runEnv {
	t.Helper()
	dir, err := os.MkdirTemp("", "pr") // short: unix socket path limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	exe, _ := os.Executable()
	bin := filepath.Join(dir, "bin")
	_ = os.MkdirAll(bin, 0o755)
	for _, name := range []string{"claude", "codex"} {
		if err := os.Symlink(exe, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	return runEnv{dir: dir, bin: bin, sock: filepath.Join(dir, "pyred.sock"), startLog: filepath.Join(dir, "start.json"), fakeOut: filepath.Join(dir, "fake.json")}
}

// command runs `pyre ARGS` (role pyrerun) or, with direct, the fake claude
// itself, with stdin and extra env.
func (e runEnv) command(direct bool, stdin []byte, env []string, args ...string) *exec.Cmd {
	exe, _ := os.Executable()
	var cmd *exec.Cmd
	if direct {
		cmd = exec.Command(filepath.Join(e.bin, "claude"), args...)
	} else {
		cmd = exec.Command(exe)
		b, _ := json.Marshal(args)
		env = append(env, "PYRE_TEST_ROLE=pyrerun", "PYRE_TEST_ARGS="+string(b))
	}
	cmd.Env = append([]string{"PATH=" + e.bin + ":/usr/bin:/bin", "HOME=" + e.dir, "PYRE_SOCKET=" + e.sock,
		"PYRE_TEST_STARTLOG=" + e.startLog, "FAKE_OUT=" + e.fakeOut, "ANTHROPIC_API_KEY=synthetic-key", "XDG_STATE_HOME=" + e.dir, "XDG_RUNTIME_DIR=" + e.dir}, env...)
	cmd.Stdin = bytes.NewReader(stdin)
	return cmd
}

func output(t *testing.T, cmd *exec.Cmd) (stdout, stderr string, code int) {
	t.Helper()
	var o, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &se
	err := cmd.Run()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatal(err)
	}
	return o.String(), se.String(), cmd.ProcessState.ExitCode()
}

var claudeArgs = []string{"-p", "reply OK", "--output-format", "json", "--dangerously-skip-permissions", "--verbose", "--model", "haiku"}

// pyre run passes stdio through byte for byte and exits with the child's
// code; the run is recorded (tag "pyre run", no parent, headless claim),
// the child runs with the bound account's profile and without the caller's
// API key, and the node hears the child's pid, exit code and session id.
func TestPyreRunRecordsAndPassesThrough(t *testing.T) {
	e := newRunEnv(t)
	node := &fakeNode{start: true} // a credential is present: the CLI records the run itself
	node.serve(t, e.sock)
	stdin := []byte("synthetic stdin \x00\xff bytes\n")
	env := []string{"FAKE_EXIT=3"}

	dOut, dErr, dCode := output(t, e.command(true, stdin, env, claudeArgs...))
	pOut, pErr, pCode := output(t, e.command(false, stdin, env, append([]string{"run", "--title", "synthetic run", "--", "claude"}, claudeArgs...)...))
	if pOut != dOut || pErr != dErr || pCode != dCode || pCode != 3 {
		t.Fatalf("pyre run differs from direct:\nstdout %q vs %q\nstderr %q vs %q\ncode %d vs %d", pOut, dOut, pErr, dErr, pCode, dCode)
	}
	var req c.StartSessionRequest
	b, _ := os.ReadFile(e.startLog)
	if err := json.Unmarshal(b, &req); err != nil {
		t.Fatal(err)
	}
	if req.Harness != c.HarnessClaude || req.NodeID != "node_synth-local" || req.Title != "synthetic run" || req.ParentSessionID != "" ||
		req.ReportToParent == nil || *req.ReportToParent || len(req.Tags) != 3 || req.Tags[0] != c.TagPyreRun || req.Tags[2] != c.TagArchiveWhenDone || len(req.Args) != 1 || !strings.HasPrefix(req.Args[0], execArg+"=") {
		t.Fatalf("start request = %+v", req)
	}
	var fake struct {
		Args      []string `json:"args"`
		ConfigDir string   `json:"config_dir"`
		APIKey    string   `json:"api_key"`
	}
	b, _ = os.ReadFile(e.fakeOut)
	_ = json.Unmarshal(b, &fake)
	if fake.ConfigDir != "/synthetic/profiles/acct_synth-a/claude" || fake.APIKey != "" || strings.Join(fake.Args, " ") != strings.Join(claudeArgs, " ") {
		t.Fatalf("child saw %+v", fake)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if len(node.claims) != 1 || len(node.started) != 1 || len(node.exited) != 1 || len(node.starts) != 0 {
		t.Fatalf("node calls: starts %v claims %v started %v exited %v", node.starts, node.claims, node.started, node.exited)
	}
	if ex := node.exited[0]; ex["exit_code"] != float64(3) || ex["native_id"] != fakeSessionID || ex["secret"] != node.claims[0]["secret"] {
		t.Fatalf("exit report %v", ex)
	}
}

// A --resume in the command tells the node which conversation it continues.
func TestPyreRunResumeHint(t *testing.T) {
	if got := resumeID([]string{"-p", "x", "--resume", fakeSessionID, "--output-format", "json"}); got != fakeSessionID {
		t.Fatalf("resume id %q", got)
	}
	if got := resumeID([]string{"-p", "--resume=" + fakeSessionID}); got != fakeSessionID {
		t.Fatalf("resume= id %q", got)
	}
	if got := resumeID([]string{"-p", "x"}); got != "" {
		t.Fatalf("no resume: %q", got)
	}
	e := newRunEnv(t)
	(&fakeNode{}).serve(t, e.sock)
	_, _, code := output(t, e.command(false, nil, nil, "run", "--", "claude", "-p", "x", "--resume", fakeSessionID))
	var req c.StartSessionRequest
	b, _ := os.ReadFile(e.startLog)
	_ = json.Unmarshal(b, &req)
	if code != 0 || len(req.Args) != 2 || req.Args[1] != execResumeArg+"="+fakeSessionID {
		t.Fatalf("code %d args %q", code, req.Args)
	}
}

// Without a node daemon the command runs exactly as given after one warning
// line on stderr; the output and exit code are the command's.
func TestPyreRunFallsBackWithoutNode(t *testing.T) {
	e := newRunEnv(t) // nothing listens on the socket
	env := []string{"FAKE_EXIT=0"}
	dOut, dErr, _ := output(t, e.command(true, []byte("in"), env, claudeArgs...))
	pOut, pErr, pCode := output(t, e.command(false, []byte("in"), env, append([]string{"run", "--", "claude"}, claudeArgs...)...))
	lines := strings.SplitN(pErr, "\n", 2)
	if pOut != dOut || pCode != 0 || len(lines) != 2 || !strings.HasPrefix(lines[0], "pyre run: ") || lines[1] != dErr {
		t.Fatalf("fallback: stdout %q vs %q, stderr %q, code %d", pOut, dOut, pErr, pCode)
	}
	var fake struct {
		APIKey string `json:"api_key"`
	}
	b, _ := os.ReadFile(e.fakeOut)
	_ = json.Unmarshal(b, &fake)
	if fake.APIKey != "synthetic-key" {
		t.Fatal("fallback changed the environment")
	}
	if _, err := os.Stat(e.startLog); err == nil {
		t.Fatal("fallback recorded a run")
	}
}

// A signal to pyre run reaches the child; pyre run then dies by the same
// signal (a caller sees the same wait status as killing claude).
func TestPyreRunForwardsSignals(t *testing.T) {
	e := newRunEnv(t)
	(&fakeNode{}).serve(t, e.sock)
	cmd := e.command(false, nil, []string{"FAKE_SLEEP=20s"}, "run", "--", "claude", "-p", "x")
	var se bytes.Buffer
	cmd.Stderr = &se
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	_ = cmd.Process.Signal(syscall.SIGTERM)
	err := cmd.Wait()
	ws, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if err == nil || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Fatalf("wait status %v (%v), stderr %q", ws, err, se.String())
	}
}

func TestSessionScannerAcrossWrites(t *testing.T) {
	var s sessionScanner
	out := `{"type":"system","session_id":"` + fakeSessionID + `"}`
	for i := 0; i < len(out); i += 7 {
		s.feed([]byte(out[i:min(i+7, len(out))]))
	}
	if s.id != fakeSessionID {
		t.Fatalf("id %q", s.id)
	}
}

// The review scripts feature-detect codex support from pyre run --help: a
// line naming codex that does not say it is refused or unsupported, and no
// line saying codex is refused or claude is the only harness.
func TestPyreRunHelpAdvertisesCodex(t *testing.T) {
	var out bytes.Buffer
	a := NewApp()
	a.Stdout, a.Stderr = &out, &out
	_ = a.Main(context.Background(), []string{"run", "--help"})
	positive := false
	for _, line := range strings.Split(out.String(), "\n") {
		l := strings.ToLower(line)
		bad := strings.Contains(l, "refuse") || strings.Contains(l, "not supported") || strings.Contains(l, "claude only")
		if strings.Contains(l, "codex") {
			if bad {
				t.Errorf("help line reads as codex unsupported: %q", line)
			} else {
				positive = true
			}
		}
		if strings.Contains(l, "only claude") {
			t.Errorf("help line: %q", line)
		}
	}
	if !positive || !strings.Contains(out.String(), "Supports claude (claude -p ...) and codex (codex exec ...)") {
		t.Fatalf("help does not advertise codex:\n%s", out.String())
	}
}

// codex: only exec runs headless (any form: prompt, resume, fork,
// review); the thread a resume or fork continues is the placement hint.
func TestCodexExecArgs(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		resume string
	}{
		{[]string{"exec", "--json", "--sandbox", "read-only", "--skip-git-repo-check", "--cd", "/synthetic/repo", "-o", "/synthetic/last.txt", "-"}, ""},
		{[]string{"exec", "--json", "-m", "gpt-6-luna", "-c", "model_reasoning_effort=xhigh", "-o", "last", "resume", fakeThreadID, "-"}, fakeThreadID},
		{[]string{"e", "resume", "--last", "-"}, ""},
		{[]string{"exec", "resume", "-m", "gpt-6-luna", fakeThreadID, "follow up"}, fakeThreadID},
		{[]string{"exec", "fork", fakeThreadID, "branch off"}, ""}, // a new thread
		{[]string{"exec", "resume", "--", fakeThreadID, "x"}, fakeThreadID},
		{[]string{"exec", "resume", "my-thread-name", "-"}, ""}, // a name, not an id: no hint
		{[]string{"exec", "-C", "resume", "prompt"}, ""},        // "resume" is -C's value here
		{[]string{"exec", "review", "--uncommitted"}, ""},
		{[]string{"exec", "--", "resume", fakeThreadID}, ""}, // the prompt
	} {
		got, err := codexExecResume(tc.args)
		if err != nil || got != tc.resume {
			t.Errorf("%q: resume %q err %v, want %q", tc.args, got, err, tc.resume)
		}
	}
	for _, args := range [][]string{nil, {"--json", "exec"}, {"resume", fakeThreadID}, {"login"}, {"-c", "k=v", "exec", "x"}, {"mcp-server"}, {"prompt text"}} {
		_, err := codexExecResume(args)
		if err == nil || ExitCodeFor(err) != ExitUsage || !strings.Contains(err.Error(), "codex exec only") {
			t.Errorf("%q: %v", args, err)
		}
	}
	a := &App{Stdout: io.Discard, Stderr: io.Discard, Getenv: func(string) string { return "" }}
	err := a.runHeadless(context.Background(), runOpts{}, []string{"codex", "resume", fakeThreadID})
	if err == nil || ExitCodeFor(err) != ExitUsage || !strings.Contains(err.Error(), "codex exec only") {
		t.Fatalf("interactive codex: %v", err)
	}
	if err := a.runHeadless(context.Background(), runOpts{}, []string{"gemini", "-p", "x"}); err == nil || ExitCodeFor(err) != ExitUsage {
		t.Fatalf("other harness: %v", err)
	}
}

func TestCodexThreadScanner(t *testing.T) {
	s := newIDScanner(c.HarnessCodex)
	out := `{"type":"thread.started","thread_id":"` + fakeThreadID + `"}` + "\n" +
		`{"type":"item.completed","item":{"type":"collab","thread_id":"11111111-1111-4111-8111-111111111111"}}` + "\n"
	for i := 0; i < len(out); i += 5 {
		s.feed([]byte(out[i:min(i+5, len(out))]))
	}
	if s.id != fakeThreadID {
		t.Fatalf("id %q (want the first thread)", s.id)
	}
}

// The calling session's identity never reaches the child.
func TestWithoutSessionIdentity(t *testing.T) {
	env := withoutSessionIdentity([]string{"PYRE_SESSION_ID=ses_synth-parent", "PYRE_RUN_ID=run_synth-parent", "PYRE_SESSION_TOKEN=synthetic-token",
		"PYRE_PARENT_SESSION_ID=ses_synth-gp", "PYRE_CLAUDE_STATE_DIR=/synthetic/run", "PYRE_SOCKET=/synthetic/pyred.sock", "PYRE_NODE_ID=node_synth", "HOME=/synthetic"})
	if strings.Join(env, " ") != "PYRE_SOCKET=/synthetic/pyred.sock PYRE_NODE_ID=node_synth HOME=/synthetic" {
		t.Fatalf("env %q", env)
	}
}

var codexArgs = []string{"exec", "--json", "--sandbox", "read-only", "--skip-git-repo-check", "-m", "gpt-6-luna", "-c", "model_reasoning_effort=xhigh", "-"}

// pyre run -- codex exec: stdio and exit code pass through, the run is
// recorded as harness codex with the claim (and the resume hint), the child
// runs on the bound CODEX_HOME without the caller's OpenAI key (keeping its
// Anthropic one) and without the calling session's identity, and the node
// hears the thread id from the --json output.
func TestPyreRunCodexExec(t *testing.T) {
	e := newRunEnv(t)
	home := "/synthetic/profiles/acct_synth-c/codex"
	node := &fakeNode{binding: &execBinding{RunID: "run_synth-run", SessionID: "ses_synth-run", AccountID: "acct_synth-c", ProfileDir: home,
		Env: map[string]string{"CODEX_HOME": home}, Unset: []string{"OPENAI_API_KEY", "CODEX_API_KEY"}}}
	node.serve(t, e.sock)
	stdin := []byte("review prompt \x00\xff\n")
	env := []string{"FAKE_EXIT=2", "OPENAI_API_KEY=synthetic-openai", "PYRE_SESSION_ID=ses_synth-parent", "PYRE_RUN_ID=run_synth-parent",
		"PYRE_SESSION_TOKEN=synthetic-parent-token", "PYRE_CLAUDE_STATE_DIR=/synthetic/parent-run"}
	direct := e.command(true, stdin, env, codexArgs...)
	direct.Path, direct.Args[0] = filepath.Join(e.bin, "codex"), "codex"
	dOut, dErr, _ := output(t, direct)
	pOut, pErr, pCode := output(t, e.command(false, stdin, env, append([]string{"run", "--title", "codex review", "--", "codex"}, codexArgs...)...))
	if pOut != dOut || pErr != dErr || pCode != 2 {
		t.Fatalf("pyre run differs from direct:\nstdout %q vs %q\nstderr %q vs %q\ncode %d", pOut, dOut, pErr, dErr, pCode)
	}
	var req c.StartSessionRequest
	b, _ := os.ReadFile(e.startLog)
	if err := json.Unmarshal(b, &req); err != nil {
		t.Fatal(err)
	}
	if req.Harness != c.HarnessCodex || req.Title != "codex review" || req.ReportToParent == nil || *req.ReportToParent || req.Tags[0] != c.TagPyreRun ||
		len(req.Args) != 1 || !strings.HasPrefix(req.Args[0], execArg+"=") {
		t.Fatalf("start request = %+v", req)
	}
	var fake map[string]any
	b, _ = os.ReadFile(e.fakeOut)
	_ = json.Unmarshal(b, &fake)
	if fake["codex_home"] != home || fake["openai_key"] != "" || fake["anthropic_key"] != "synthetic-key" || fake["session_token"] != "" ||
		fake["run_id"] != "" || fake["session_id"] != "" || fake["hook_state"] != "" || fake["socket"] != e.sock {
		t.Fatalf("child env %v", fake)
	}
	node.mu.Lock()
	if len(node.exited) != 1 || node.exited[0]["exit_code"] != float64(2) || node.exited[0]["native_id"] != fakeThreadID {
		t.Fatalf("exit report %v", node.exited)
	}
	node.mu.Unlock()

	// exec resume ID: the node is told which thread it continues; stdout
	// to a file still yields the thread id (from the file's start).
	out, _ := os.OpenFile(filepath.Join(e.dir, "codex.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	defer out.Close()
	rcmd := e.command(false, bytes.Repeat([]byte("y"), 2<<20), nil, "run", "--", "codex", "exec", "--json", "-o", filepath.Join(e.dir, "last"), "resume", fakeThreadID, "-")
	rcmd.Stdout = out
	if err := rcmd.Run(); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(e.startLog)
	req = c.StartSessionRequest{}
	_ = json.Unmarshal(b, &req)
	if len(req.Args) != 2 || req.Args[1] != execResumeArg+"="+fakeThreadID {
		t.Fatalf("resume args %q", req.Args)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if len(node.exited) != 2 || node.exited[1]["native_id"] != fakeThreadID {
		t.Fatalf("file stdout exit report %v", node.exited)
	}
}

// Fallback for codex: the own login is CODEX_HOME (or ~/.codex), the
// command runs as given but still without the calling session's identity.
func TestPyreRunCodexFallback(t *testing.T) {
	e := newRunEnv(t) // nothing listens
	_, se, code := output(t, e.command(false, nil, []string{"CODEX_HOME=/synthetic/own-codex", "OPENAI_API_KEY=synthetic-openai", "PYRE_SESSION_TOKEN=synthetic-parent-token"},
		"run", "--", "codex", "exec", "x"))
	if code != 0 || !strings.Contains(se, "CODEX_HOME=/synthetic/own-codex") {
		t.Fatalf("fallback: code %d stderr %q", code, se)
	}
	var fake map[string]any
	b, _ := os.ReadFile(e.fakeOut)
	_ = json.Unmarshal(b, &fake)
	if fake["codex_home"] != "/synthetic/own-codex" || fake["openai_key"] != "synthetic-openai" || fake["session_token"] != "" {
		t.Fatalf("fallback child env %v", fake)
	}
	f := newRunEnv(t)
	(&fakeNode{refuse: true}).serve(t, f.sock)
	_, se, code = output(t, f.command(false, nil, nil, "run", "--fallback", "fail", "--", "codex", "exec", "x"))
	if code != ExitPyreUnavailable || !strings.Contains(se, "Pyre declined the run") {
		t.Fatalf("fail: code %d stderr %q", code, se)
	}
}

// A caller that closes its end of stdout early (`| head -1`) does not kill
// pyre run: the child sees the broken pipe and pyre still reports the exit.
func TestPyreRunClosedStdout(t *testing.T) {
	e := newRunEnv(t)
	node := &fakeNode{}
	node.serve(t, e.sock)
	big := bytes.Repeat([]byte("x"), 1<<20)
	cmd := e.command(false, big, nil, "run", "--", "claude", "-p", "x")
	r, w, _ := os.Pipe()
	cmd.Stdout = w
	var se bytes.Buffer
	cmd.Stderr = &se
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	buf := make([]byte, 10)
	_, _ = r.Read(buf)
	r.Close() // the caller stops reading
	_ = cmd.Wait()
	// The fake (a Go program) may itself die of SIGPIPE writing to the
	// broken pipe, as claude would get EPIPE: pyre mirrors that status.
	ws, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
	node.mu.Lock()
	defer node.mu.Unlock()
	if len(node.exited) != 1 {
		t.Fatalf("exit not reported after a closed stdout (wait status %v, stderr %q)", ws, se.String())
	}
}

// SIGKILL of pyre run (Python subprocess timeout) takes claude with it
// (Linux parent-death signal; the node stops it elsewhere).
func TestPyreRunKilledTakesChild(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("parent-death signal is Linux only (the node covers macOS)")
	}
	e := newRunEnv(t)
	(&fakeNode{}).serve(t, e.sock)
	pidFile := filepath.Join(e.dir, "child.pid")
	cmd := e.command(false, nil, []string{"FAKE_SLEEP=30s", "FAKE_PID=" + pidFile}, "run", "--", "claude", "-p", "x")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var pid int
	for i := 0; i < 100 && pid == 0; i++ {
		b, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(string(b))
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("child never started")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	for i := 0; i < 100; i++ {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatal("claude outlived a SIGKILLed pyre run")
}

// Stdout redirected to a file: a background process claude left holding
// it does not keep pyre run waiting (a direct call returns at claude's
// exit); the session id is read from the file.
func TestPyreRunFileStdoutNoHang(t *testing.T) {
	e := newRunEnv(t)
	node := &fakeNode{}
	node.serve(t, e.sock)
	// Write-only, as a shell's `> out.json` opens it: pyre reopens it to
	// read (Linux /proc/self/fd, macOS F_GETPATH).
	out, _ := os.OpenFile(filepath.Join(e.dir, "out.json"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	defer out.Close()
	cmd := e.command(false, nil, []string{"FAKE_BG=10"}, "run", "--", "claude", "-p", "x")
	cmd.Stdout = out
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 6*time.Second { // the holder lives 10 s; -race binaries start slowly
		t.Fatalf("pyre run waited %s for a background holder of its stdout", d)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if len(node.exited) != 1 || node.exited[0]["native_id"] != fakeSessionID {
		t.Fatalf("exit report %v", node.exited)
	}
}

// Pyre declining (no eligible account) is told apart from Pyre being
// unavailable: by default the command still runs on the own login, named
// on stderr; --fallback fail exits 75 without running it. An unavailable
// node is remembered briefly so a burst does not wait each time.
func TestPyreRunFallbackModes(t *testing.T) {
	e := newRunEnv(t)
	(&fakeNode{refuse: true}).serve(t, e.sock)
	_, se, code := output(t, e.command(false, nil, []string{"CLAUDE_CONFIG_DIR=/synthetic/own"}, "run", "--", "claude", "-p", "x"))
	if code != 0 || !strings.Contains(se, "Pyre declined the run: ") || !strings.Contains(se, "CLAUDE_CONFIG_DIR=/synthetic/own") {
		t.Fatalf("refused, own: code %d stderr %q", code, se)
	}
	_ = os.Remove(e.fakeOut)
	_, se, code = output(t, e.command(false, nil, nil, "run", "--fallback", "fail", "--", "claude", "-p", "x"))
	if code != ExitPyreUnavailable || !strings.Contains(se, "not running it") {
		t.Fatalf("refused, fail: code %d stderr %q", code, se)
	}
	if _, err := os.Stat(e.fakeOut); err == nil {
		t.Fatal("--fallback fail ran the command")
	}

	u := newRunEnv(t) // nothing listens
	_, se, _ = output(t, u.command(false, nil, nil, "run", "--", "claude", "-p", "x"))
	if !strings.Contains(se, "Pyre is unavailable: no Pyre node daemon") {
		t.Fatalf("unavailable: %q", se)
	}
	_, se, _ = output(t, u.command(false, nil, nil, "run", "--", "claude", "-p", "x"))
	if !strings.Contains(se, "unavailable moments ago") {
		t.Fatalf("second call did not use the remembered outage: %q", se)
	}
}

// No hub login on this machine (no client config, no session token): the
// node records the run with its own credential. The socket is found at the
// default state path without PYRE_SOCKET, and with PYRE_SOCKET set. The
// output and exit code pass through, the claim secret matches the hash the
// node recorded, and nothing went to a hub client.
func TestPyreRunWithoutLoginUsesNode(t *testing.T) {
	for _, viaEnv := range []bool{false, true} {
		e := newRunEnv(t)
		sock := filepath.Join(e.dir, "pyre", "pyred.sock") // $XDG_STATE_HOME/pyre/pyred.sock
		env := []string{"PYRE_TEST_NOAPI=1", "FAKE_EXIT=4", "PYRE_SOCKET="}
		if runtime.GOOS == "darwin" {
			// ~/Library/Application Support/pyre/pyred.sock under a short
			// HOME (a unix socket path is at most 104 bytes on macOS).
			home, err := os.MkdirTemp("/tmp", "ph")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(home) })
			sock = filepath.Join(home, "Library", "Application Support", "pyre", "pyred.sock")
			env = append(env, "HOME="+home)
		}
		_ = os.MkdirAll(filepath.Dir(sock), 0o700)
		node := &fakeNode{start: true}
		node.serve(t, sock)
		if viaEnv {
			env[2] = "PYRE_SOCKET=" + sock
		}
		dOut, dErr, _ := output(t, e.command(true, []byte("in"), env, claudeArgs...))
		pOut, pErr, pCode := output(t, e.command(false, []byte("in"), env,
			append([]string{"run", "--title", "sentry triage", "--project", "sentry", "--", "claude"}, claudeArgs...)...))
		if pOut != dOut || pErr != dErr || pCode != 4 {
			t.Fatalf("PYRE_SOCKET %v: stdout %q vs %q, stderr %q vs %q, code %d", viaEnv, pOut, dOut, pErr, dErr, pCode)
		}
		if _, err := os.Stat(e.startLog); err == nil {
			t.Fatal("a hub client recorded the run")
		}
		node.mu.Lock()
		if len(node.starts) != 1 || len(node.claims) != 1 || len(node.exited) != 1 {
			t.Fatalf("node calls: starts %v claims %v exited %v", node.starts, node.claims, node.exited)
		}
		st, cl := node.starts[0], node.claims[0]
		sum := sha256.Sum256([]byte(cl["secret"].(string)))
		cwd, _ := os.Getwd()
		if st["claim_sha256"] != hex.EncodeToString(sum[:]) || st["title"] != "sentry triage" || st["project"] != "sentry" || st["cwd"] != cwd ||
			st["harness"] != "claude" || cl["run_id"] != "run_synth-run" || node.exited[0]["exit_code"] != float64(4) {
			t.Fatalf("start %v claim %v", st, cl)
		}
		var fake struct {
			ConfigDir string `json:"config_dir"`
		}
		b, _ := os.ReadFile(e.fakeOut)
		_ = json.Unmarshal(b, &fake)
		if fake.ConfigDir != "/synthetic/profiles/acct_synth-a/claude" {
			t.Fatalf("child saw %+v", fake)
		}
		node.mu.Unlock()
	}
}

// Rolling compatibility without a login: an older pyred (no node start)
// falls back on the own login with a reason that says so; a hub that
// refuses the node's start is a refusal (declined), not an outage.
func TestPyreRunWithoutLoginOldNode(t *testing.T) {
	e := newRunEnv(t)
	node := &fakeNode{} // PR #15 pyred: headless claims, no start
	node.serve(t, e.sock)
	_, se, code := output(t, e.command(false, nil, []string{"PYRE_TEST_NOAPI=1"}, "run", "--", "claude", "-p", "x"))
	if code != 0 || !strings.Contains(se, "Pyre is unavailable: ") || !strings.Contains(se, "cannot record a run itself") {
		t.Fatalf("old pyred: code %d stderr %q", code, se)
	}
	node.mu.Lock()
	if len(node.starts) != 0 || len(node.claims) != 0 {
		t.Fatalf("old pyred got starts %v claims %v", node.starts, node.claims)
	}
	node.mu.Unlock()

	d := newRunEnv(t)
	(&fakeNode{start: true, deny: true}).serve(t, d.sock)
	_, se, code = output(t, d.command(false, nil, []string{"PYRE_TEST_NOAPI=1"}, "run", "--fallback", "fail", "--", "claude", "-p", "x"))
	if code != ExitPyreUnavailable || !strings.Contains(se, "Pyre declined the run: this machine's node did not record the run: a node credential") {
		t.Fatalf("hub refused the node: code %d stderr %q", code, se)
	}
}
