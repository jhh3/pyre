//go:build !windows

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
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
		if d, err := time.ParseDuration(os.Getenv("PYRE_TEST_CODEX_STDIN_IDLE")); err == nil {
			codexStdinIdle = d
		}
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
	if p := os.Getenv("FAKE_STALL_COUNT"); p != "" {
		// Stuck before its thread on its first FAKE_STALL_N starts: stdin
		// read, nothing printed. Each start leaves its stdin and pid.
		b, _ := os.ReadFile(p)
		n := len(b) + 1
		_ = os.WriteFile(p, bytes.Repeat([]byte("x"), n), 0o600)
		_ = os.WriteFile(p+".stdin."+strconv.Itoa(n), in, 0o600)
		_ = os.WriteFile(p+".pid."+strconv.Itoa(n), []byte(strconv.Itoa(os.Getpid())), 0o600)
		if limit, _ := strconv.Atoi(os.Getenv("FAKE_STALL_N")); n <= limit {
			if os.Getenv("FAKE_STALL_HARD") != "" {
				signal.Ignore(syscall.SIGTERM) // stuck past SIGTERM too
			}
			time.Sleep(time.Minute)
		}
	}
	os.Stdout.WriteString(`{"type":"thread.started","thread_id":"` + fakeThreadID + `"}` + "\n")
	if os.Getenv("FAKE_OVERLOAD") != "" { // OpenAI at capacity: a retry, then the failed turn
		os.Stdout.WriteString(`{"type":"error","message":"Reconnecting... 1/5 (server_overloaded: synthetic)"}` + "\n")
		os.Stdout.WriteString(`{"type":"turn.failed","error":{"message":"synthetic capacity"}}` + "\n")
	}
	if d, err := time.ParseDuration(os.Getenv("FAKE_SLEEP")); err == nil {
		time.Sleep(d) // a long turn
	}
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
	again   int          // the first claims answer "still on its way; claim again"
	mu      sync.Mutex
	starts  []map[string]any
	claims  []map[string]any
	started []map[string]any
	exited  []map[string]any
	native  []map[string]any // with "exited_before": exit reports received before it
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
		f.mu.Lock()
		again := len(f.claims) < f.again
		f.mu.Unlock()
		if again {
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			f.mu.Lock()
			f.claims = append(f.claims, m)
			f.mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": c.Errorf(c.ErrUnavailable, "headless run run_synth-run has not reached this node yet (synthetic); claim again").
				WithDetail(execClaimAgain, true)})
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
	mux.HandleFunc("POST "+localExecNativePath, func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		f.mu.Lock()
		m["exited_before"] = len(f.exited)
		f.native = append(f.native, m)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{})
	})
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
	// The machine's own logins (HOME=dir): a fallback refuses to run with
	// none at all.
	synthLogin(t, filepath.Join(dir, ".claude"), "claude")
	synthLogin(t, filepath.Join(dir, ".codex"), "codex")
	return runEnv{dir: dir, bin: bin, sock: filepath.Join(dir, "pyred.sock"), startLog: filepath.Join(dir, "start.json"), fakeOut: filepath.Join(dir, "fake.json")}
}

// synthLogin writes a synthetic subscription login for harness h in home
// and returns home.
func synthLogin(t *testing.T, home, h string) string {
	t.Helper()
	p, body := filepath.Join(home, ".credentials.json"), `{"claudeAiOauth":{"accessToken":"synthetic"}}`
	if h == "codex" {
		p, body = filepath.Join(home, "auth.json"), `{"auth_mode":"chatgpt","tokens":{"refresh_token":"synthetic"}}`
	}
	_ = os.MkdirAll(home, 0o700)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
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

// A claim the node answers "claim again" (the start still on its way over
// a slow hub link) is claimed once more, not given up on: the command runs
// once on the binding, even with --fallback fail, and each claim tells the
// node to answer before this process's own deadline.
func TestPyreRunClaimsAgainOnce(t *testing.T) {
	e := newRunEnv(t)
	node := &fakeNode{again: 1}
	node.serve(t, e.sock)
	_, se, code := output(t, e.command(false, nil, []string{"FAKE_EXIT=5"}, "run", "--fallback", "fail", "--setup-timeout", "4s", "--", "claude", "-p", "x"))
	if code != 5 || strings.Contains(se, "pyre run:") {
		t.Fatalf("code %d stderr %q", code, se)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if len(node.claims) != 2 || len(node.started) != 1 || len(node.exited) != 1 {
		t.Fatalf("claims %v started %v exited %v", node.claims, node.started, node.exited)
	}
	for _, cl := range node.claims {
		wait, _ := cl["wait_ms"].(float64)
		if cl["secret"] != node.claims[0]["secret"] || cl["client_pid"] != node.claims[0]["client_pid"] || wait <= 0 || wait >= 4000 {
			t.Fatalf("claim %v", cl)
		}
	}

	// Still not there after the second claim: exit 75 with the node's
	// reason, after exactly two claims.
	u := newRunEnv(t)
	slow := &fakeNode{again: 5}
	slow.serve(t, u.sock)
	_, se, code = output(t, u.command(false, nil, nil, "run", "--fallback", "fail", "--setup-timeout", "2s", "--", "claude", "-p", "x"))
	if code != ExitPyreUnavailable || !strings.Contains(se, "has not reached this node yet") {
		t.Fatalf("code %d stderr %q", code, se)
	}
	slow.mu.Lock()
	defer slow.mu.Unlock()
	if len(slow.claims) != 2 {
		t.Fatalf("claims %d, want 2", len(slow.claims))
	}
	// A slow claim is not an outage: the next call is not told to skip Pyre.
	if _, err := os.Stat(filepath.Join(u.dir, fmt.Sprintf("pyre-run-down-%d", os.Getuid()))); err == nil {
		t.Fatal("a slow claim wrote the down marker")
	}
}

// With no subscription account bound and no own login at all, pyre run
// exits 75 with a clear line instead of running into the provider's 401,
// and a fresh down marker does not make it skip trying Pyre.
func TestPyreRunNoOwnLogin(t *testing.T) {
	hs := []string{"codex"}
	if runtime.GOOS != "darwin" { // macOS keeps Claude's login in the Keychain
		hs = append(hs, "claude")
	}
	for _, h := range hs {
		e := newRunEnv(t) // nothing listens
		empty := filepath.Join(e.dir, "empty-"+h)
		_ = os.MkdirAll(empty, 0o700)
		env, args := []string{"CODEX_HOME=" + empty}, []string{"run", "--", "codex", "exec", "x"}
		if h == "claude" {
			env, args = []string{"CLAUDE_CONFIG_DIR=" + empty}, []string{"run", "--", "claude", "-p", "x"}
		}
		_ = os.WriteFile(filepath.Join(e.dir, fmt.Sprintf("pyre-run-down-%d", os.Getuid())), nil, 0o600)
		_, se, code := output(t, e.command(false, nil, env, args...))
		if code != ExitPyreUnavailable || !strings.Contains(se, "no subscription account bound and this machine has no own "+h+" login") ||
			!strings.Contains(se, "no Pyre node daemon") {
			t.Fatalf("%s: code %d stderr %q", h, code, se)
		}
		if _, err := os.Stat(e.fakeOut); err == nil {
			t.Fatalf("%s: ran the command with no login", h)
		}
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

// Without a node daemon the command runs as given after one warning line on
// stderr; the output and exit code are the command's. The caller's
// ANTHROPIC_API_KEY never reaches it (subscriptions only).
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
	if fake.APIKey != "" {
		t.Fatal("fallback passed the caller's API key")
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
		"PYRE_PARENT_SESSION_ID=ses_synth-gp", "PYRE_CLAUDE_STATE_DIR=/synthetic/run", "PYRE_SOCKET=/synthetic/pyred.sock", "PYRE_NODE_ID=node_synth", "PYRE_NODE_NAME=synth-node", "HOME=/synthetic"})
	if strings.Join(env, " ") != "PYRE_SOCKET=/synthetic/pyred.sock PYRE_NODE_ID=node_synth PYRE_NODE_NAME=synth-node HOME=/synthetic" {
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
// command runs as given but still without the calling session's identity
// or the caller's OPENAI_API_KEY.
func TestPyreRunCodexFallback(t *testing.T) {
	e := newRunEnv(t) // nothing listens
	own := synthLogin(t, filepath.Join(e.dir, "own-codex"), "codex")
	_, se, code := output(t, e.command(false, nil, []string{"CODEX_HOME=" + own, "OPENAI_API_KEY=synthetic-openai", "PYRE_SESSION_TOKEN=synthetic-parent-token"},
		"run", "--", "codex", "exec", "x"))
	if code != 0 || !strings.Contains(se, "CODEX_HOME="+own) {
		t.Fatalf("fallback: code %d stderr %q", code, se)
	}
	var fake map[string]any
	b, _ := os.ReadFile(e.fakeOut)
	_ = json.Unmarshal(b, &fake)
	if fake["codex_home"] != own || fake["openai_key"] != "" || fake["session_token"] != "" {
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
	own := synthLogin(t, filepath.Join(e.dir, "own"), "claude")
	_, se, code := output(t, e.command(false, nil, []string{"CLAUDE_CONFIG_DIR=" + own}, "run", "--", "claude", "-p", "x"))
	if code != 0 || !strings.Contains(se, "Pyre declined the run: ") || !strings.Contains(se, "CLAUDE_CONFIG_DIR="+own) {
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

// A long codex exec (a 20-minute review) names its thread to the node
// while it still runs, from a pipe or a file stdout, so pyre read shows its
// transcript before it ends instead of nothing until the exit report
// (iss_…).
func TestPyreRunReportsNativeWhileRunning(t *testing.T) {
	e := newRunEnv(t)
	home := "/synthetic/profiles/acct_synth-c/codex"
	node := &fakeNode{binding: &execBinding{RunID: "run_synth-run", SessionID: "ses_synth-run", AccountID: "acct_synth-c", ProfileDir: home,
		Env: map[string]string{"CODEX_HOME": home}}}
	node.serve(t, e.sock)
	args := []string{"run", "--", "codex", "exec", "--json", "prompt"}
	env := []string{"FAKE_SLEEP=3s"}
	// A pipe stdout.
	if _, _, code := output(t, e.command(false, nil, env, args...)); code != 0 {
		t.Fatalf("pipe run exit %d", code)
	}
	// A file stdout, as `> out.jsonl` opens it.
	out, _ := os.OpenFile(filepath.Join(e.dir, "out.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	defer out.Close()
	cmd := e.command(false, nil, env, args...)
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if len(node.native) != 2 || len(node.exited) != 2 {
		t.Fatalf("native reports %v, exit reports %v", node.native, node.exited)
	}
	for i, m := range node.native {
		if m["native_id"] != fakeThreadID || m["run_id"] != "run_synth-run" || m["secret"] == "" || m["exited_before"] != i {
			t.Fatalf("native report %d (want before exit %d): %v", i, i, m)
		}
	}
}

// codex exec --json output as pyre run's caller sees it on stderr: error
// and turn.failed events repeated (not an item of type error, a warning),
// split across writes; nothing printed for a while said so, once codex
// prints that said too.
func TestCodexWatch(t *testing.T) {
	var se bytes.Buffer
	w := &codexWatch{stderr: &se}
	if w.quietNotice(3*time.Minute) || !strings.Contains(se.String(), "codex has printed nothing in 3m0s") {
		t.Fatalf("no-output notice: %q", se.String())
	}
	se.Reset()
	out := `{"type":"thread.started","thread_id":"` + fakeThreadID + `"}` + "\n" +
		`{"type":"item.completed","item":{"id":"item_0","type":"error","message":"Codex is ignoring 1 unrecognized configuration setting"}}` + "\n" +
		`{"type":"error","message":"Reconnecting... 2/5 (server_overloaded:\n try later)"}` + "\n" +
		`{"type":"turn.failed","error":{"message":"synthetic capacity"}}` + "\n"
	for i := 0; i < len(out); i += 7 {
		w.feed([]byte(out[i:min(i+7, len(out))]))
	}
	want := "pyre run: codex has started printing\npyre run: codex: Reconnecting... 2/5 (server_overloaded: try later)\npyre run: codex: turn failed: synthetic capacity\n"
	if se.String() != want {
		t.Fatalf("stderr %q\nwant   %q", se.String(), want)
	}
	if !w.quietNotice(time.Hour) {
		t.Fatal("codex printed: no notice")
	}
	if !codexJSONOutput([]string{"exec", "--json", "-"}) || codexJSONOutput([]string{"exec", "--", "--json"}) {
		t.Fatal("codexJSONOutput")
	}
}

// pyre run -- codex exec --json: codex's error events reach stderr whether
// stdout is a pipe or a (write-only) file, including those printed just
// before it exits.
func TestPyreRunCodexErrorsOnStderr(t *testing.T) {
	e := newRunEnv(t)
	node := &fakeNode{binding: &execBinding{RunID: "run_synth-run", SessionID: "ses_synth-run", Env: map[string]string{"CODEX_HOME": "/synthetic/codex"}}}
	node.serve(t, e.sock)
	args := append([]string{"run", "--", "codex"}, codexArgs...)
	want := "pyre run: codex: Reconnecting... 1/5 (server_overloaded: synthetic)\npyre run: codex: turn failed: synthetic capacity\n"
	_, se, _ := output(t, e.command(false, nil, []string{"FAKE_OVERLOAD=1"}, args...))
	if !strings.Contains(se, want) {
		t.Fatalf("pipe stdout: stderr %q", se)
	}
	out, _ := os.OpenFile(filepath.Join(e.dir, "codex.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	defer out.Close()
	cmd := e.command(false, nil, []string{"FAKE_OVERLOAD=1"}, args...)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("file stdout: stderr %q", stderr.String())
	}
}

// The watcher says when a file stdout stays empty, repeats it, and stops
// once codex writes.
func TestCodexWatchQuietFile(t *testing.T) {
	defer func(a, e, p time.Duration) { codexQuietAfter, codexQuietEvery, codexWatchPoll = a, e, p }(codexQuietAfter, codexQuietEvery, codexWatchPoll)
	codexQuietAfter, codexQuietEvery, codexWatchPoll = 30*time.Millisecond, 30*time.Millisecond, 5*time.Millisecond
	f, _ := os.OpenFile(filepath.Join(t.TempDir(), "codex.jsonl"), os.O_WRONLY|os.O_CREATE, 0o600)
	defer f.Close()
	var se syncBuffer
	w := &codexWatch{stderr: &se, file: f}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() { defer close(done); w.run(stop) }()
	time.Sleep(100 * time.Millisecond)
	f.WriteString(`{"type":"thread.started","thread_id":"` + fakeThreadID + `"}` + "\n")
	time.Sleep(100 * time.Millisecond)
	close(stop)
	<-done
	s := se.String()
	if n := strings.Count(s, "codex has printed nothing"); n < 2 || !strings.HasSuffix(s, "pyre run: codex has started printing\n") {
		t.Fatalf("stderr %q", s)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// A codex exec --json that prints nothing within --codex-stall is stopped
// and started once more with the same stdin (a pipe, or a file read again
// from where it began), and its output and exit are the second start's;
// the node hears both children and one exit.
func TestPyreRunCodexStallStartsAgain(t *testing.T) {
	e := newRunEnv(t)
	node := &fakeNode{binding: &execBinding{RunID: "run_synth-run", SessionID: "ses_synth-run", Env: map[string]string{"CODEX_HOME": "/synthetic/codex"}}}
	node.serve(t, e.sock)
	count := filepath.Join(e.dir, "stall")
	env := []string{"FAKE_STALL_COUNT=" + count, "FAKE_STALL_N=1", "FAKE_EXIT=3", "PYRE_RUN_CODEX_STALL=1500ms"}
	stdin := []byte("piped prompt \x00\xff\n")
	start := time.Now()
	so, se, code := output(t, e.command(false, stdin, env, "run", "--", "codex", "exec", "--json", "-"))
	if code != 3 || strings.Count(so, `"thread.started"`) != 1 || !strings.HasSuffix(so, string(stdin)) {
		t.Fatalf("code %d stdout %q stderr %q", code, so, se)
	}
	if !strings.Contains(se, "codex printed nothing in 1.5s") || !strings.Contains(se, "starting it once more") {
		t.Fatalf("stderr %q", se)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("took %s", d)
	}
	for _, n := range []string{"1", "2"} {
		if b, _ := os.ReadFile(count + ".stdin." + n); !bytes.Equal(b, stdin) {
			t.Fatalf("start %s read stdin %q", n, b)
		}
	}
	assertGone(t, count+".pid.1")
	node.mu.Lock()
	if len(node.started) != 2 || node.started[0]["pid"] == node.started[1]["pid"] || len(node.exited) != 1 ||
		node.exited[0]["exit_code"] != float64(3) || node.exited[0]["native_id"] != fakeThreadID {
		t.Fatalf("started %v exited %v", node.started, node.exited)
	}
	node.mu.Unlock()

	// stdin and stdout regular files (`< prompt > out.jsonl`).
	f := newRunEnv(t)
	(&fakeNode{binding: &execBinding{RunID: "run_synth-run", SessionID: "ses_synth-run", Env: map[string]string{"CODEX_HOME": "/synthetic/codex"}}}).serve(t, f.sock)
	count = filepath.Join(f.dir, "stall")
	inPath := filepath.Join(f.dir, "prompt.txt")
	_ = os.WriteFile(inPath, []byte("skip|file prompt\n"), 0o600)
	in, _ := os.Open(inPath)
	defer in.Close()
	_, _ = in.Seek(5, io.SeekStart) // the caller already read "skip|"
	out, _ := os.OpenFile(filepath.Join(f.dir, "out.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	defer out.Close()
	cmd := f.command(false, nil, []string{"FAKE_STALL_COUNT=" + count, "FAKE_STALL_N=1", "PYRE_RUN_CODEX_STALL=1500ms"}, "run", "--", "codex", "exec", "--json", "x")
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	for _, n := range []string{"1", "2"} {
		if b, _ := os.ReadFile(count + ".stdin." + n); string(b) != "file prompt\n" {
			t.Fatalf("start %s read stdin %q", n, b)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(f.dir, "out.jsonl")); bytes.Count(b, []byte(`"thread.started"`)) != 1 {
		t.Fatalf("file stdout %q", b)
	}
}

// A second stall ends pyre run with 124 and a clear error, both children
// gone; the node hears exit 124.
func TestPyreRunCodexStallTwiceExits(t *testing.T) {
	e := newRunEnv(t)
	node := &fakeNode{binding: &execBinding{RunID: "run_synth-run", SessionID: "ses_synth-run", Env: map[string]string{"CODEX_HOME": "/synthetic/codex"}}}
	node.serve(t, e.sock)
	count := filepath.Join(e.dir, "stall")
	so, se, code := output(t, e.command(false, nil, []string{"FAKE_STALL_COUNT=" + count, "FAKE_STALL_N=9"},
		"run", "--codex-stall", "1s", "--", "codex", "exec", "--json", "x"))
	if code != ExitCodexStalled || so != "" || !strings.Contains(se, "printed nothing in 1s again") || !strings.Contains(se, "giving up (exit 124)") {
		t.Fatalf("code %d stdout %q stderr %q", code, so, se)
	}
	if b, _ := os.ReadFile(count); len(b) != 2 {
		t.Fatalf("%d starts", len(b))
	}
	assertGone(t, count+".pid.1")
	assertGone(t, count+".pid.2")
	node.mu.Lock()
	defer node.mu.Unlock()
	if len(node.started) != 2 || len(node.exited) != 1 || node.exited[0]["exit_code"] != float64(ExitCodexStalled) {
		t.Fatalf("started %v exited %v", node.started, node.exited)
	}
}

// --codex-stall 0 (or the variable) leaves a quiet codex running; bad
// values are usage errors.
func TestPyreRunCodexStallOff(t *testing.T) {
	e := newRunEnv(t) // without Pyre: the own login, still watched
	count := filepath.Join(e.dir, "stall")
	cmd := e.command(false, nil, []string{"FAKE_STALL_COUNT=" + count, "FAKE_STALL_N=1", "PYRE_RUN_CODEX_STALL=0"}, "run", "--", "codex", "exec", "--json", "x")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if b, _ := os.ReadFile(count); len(b) != 1 {
		t.Fatalf("%d starts with the stall check off", len(b))
	}
	_ = cmd.Process.Signal(syscall.SIGTERM) // forwarded to the child
	_ = cmd.Wait()
	assertGone(t, count+".pid.1")
	for _, bad := range [][]string{{"run", "--codex-stall", "-1s", "--", "codex", "exec", "x"}} {
		if _, se, code := output(t, e.command(false, nil, nil, bad...)); code != 2 || !strings.Contains(se, "--codex-stall must not be negative") {
			t.Fatalf("%v: code %d stderr %q", bad, code, se)
		}
	}
	if _, se, code := output(t, e.command(false, nil, []string{"PYRE_RUN_CODEX_STALL=soon"}, "run", "--", "codex", "exec", "x")); code != 2 || !strings.Contains(se, "PYRE_RUN_CODEX_STALL") {
		t.Fatalf("bad variable: code %d stderr %q", code, se)
	}
}

// The stall window of a piped stdin starts when the caller closes it: a
// codex waiting for its input is not stuck.
func TestPyreRunCodexStallWaitsForStdin(t *testing.T) {
	e := newRunEnv(t)
	count := filepath.Join(e.dir, "stall")
	cmd := e.command(false, nil, []string{"FAKE_STALL_COUNT=" + count, "PYRE_RUN_CODEX_STALL=500ms"}, "run", "--", "codex", "exec", "--json", "-")
	cmd.Stdin = nil
	w, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("slow "))
	time.Sleep(1500 * time.Millisecond) // three windows with stdin still open
	_, _ = w.Write([]byte("caller\n"))
	w.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("%v: %s", err, se.String())
	}
	if b, _ := os.ReadFile(count); len(b) != 1 || strings.Contains(se.String(), "printed nothing") || !strings.HasSuffix(so.String(), "slow caller\n") {
		t.Fatalf("%d starts, stdout %q stderr %q", len(b), so.String(), se.String())
	}
}

// stdinCopy replays everything the caller sent, to a pipe made before or
// after it arrived.
func TestStdinCopyReplays(t *testing.T) {
	src, w := io.Pipe()
	s := newStdinCopy(src)
	r1, err := s.pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		w.Write(bytes.Repeat([]byte("a"), 200<<10))
		w.Write([]byte("end"))
		w.Close()
	}()
	b1, _ := io.ReadAll(r1)
	<-s.done
	r2, _ := s.pipe()
	b2, _ := io.ReadAll(r2)
	if len(b1) != 200<<10+3 || !bytes.Equal(b1, b2) {
		t.Fatalf("replay %d vs %d bytes", len(b1), len(b2))
	}
}

// assertGone fails unless the process whose pid is in path has ended.
func assertGone(t *testing.T, path string) {
	t.Helper()
	b, _ := os.ReadFile(path)
	pid, _ := strconv.Atoi(string(b))
	if pid <= 0 {
		t.Fatalf("no pid in %s", path)
	}
	for i := 0; i < 100; i++ {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("stalled codex %d still running", pid)
}

// A stalled codex that ignores SIGTERM gets SIGKILL after the grace; a
// caller's signal while it is being stopped means no second start.
func TestPyreRunCodexStallCallerSignal(t *testing.T) {
	e := newRunEnv(t)
	(&fakeNode{binding: &execBinding{RunID: "run_synth-run", SessionID: "ses_synth-run", Env: map[string]string{"CODEX_HOME": "/synthetic/codex"}}}).serve(t, e.sock)
	count := filepath.Join(e.dir, "stall")
	cmd := e.command(false, nil, []string{"FAKE_STALL_COUNT=" + count, "FAKE_STALL_N=9", "FAKE_STALL_HARD=1", "PYRE_RUN_CODEX_STALL=1s"},
		"run", "--", "codex", "exec", "--json", "x")
	var se bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &se
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second) // stopped at 1s, ignoring SIGTERM
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	if b, _ := os.ReadFile(count); len(b) != 1 || strings.Contains(se.String(), "starting it once more") {
		t.Fatalf("%d starts after the caller's SIGTERM; stderr %q", len(b), se.String())
	}
	ws, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("pyre run ended %v, want the child's SIGKILL", ws)
	}
	assertGone(t, count+".pid.1")
}

// An own login that bills an API key is never run on: a fallback onto it
// exits 75 without starting the command (Codex auth.json in API-key mode,
// Claude's apiKeyHelper), whichever --fallback.
func TestPyreRunFallbackRefusesAPIKeyLogin(t *testing.T) {
	e := newRunEnv(t) // nothing listens
	home := filepath.Join(e.dir, "own-codex")
	_ = os.MkdirAll(home, 0o700)
	_ = os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"synthetic-not-a-key"}`), 0o600)
	_, se, code := output(t, e.command(false, nil, []string{"CODEX_HOME=" + home}, "run", "--", "codex", "exec", "x"))
	if code != ExitPyreUnavailable || !strings.Contains(se, "API-key login") || strings.Contains(se, "synthetic-not-a-key") {
		t.Fatalf("codex: code %d stderr %q", code, se)
	}
	if _, err := os.Stat(e.fakeOut); err == nil {
		t.Fatal("codex ran on an API-key login")
	}
	cfg := filepath.Join(e.dir, "own-claude")
	_ = os.MkdirAll(cfg, 0o700)
	_ = os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"apiKeyHelper":"/synthetic/helper"}`), 0o600)
	_, se, code = output(t, e.command(false, nil, []string{"CLAUDE_CONFIG_DIR=" + cfg}, "run", "--", "claude", "-p", "x"))
	if code != ExitPyreUnavailable || !strings.Contains(se, "apiKeyHelper") {
		t.Fatalf("claude: code %d stderr %q", code, se)
	}
	if _, err := os.Stat(e.fakeOut); err == nil {
		t.Fatal("claude ran on an API-key login")
	}
}

// A codex exec whose prompt is an argument, under a caller that leaves a
// pipe open with no input (a background shell's inherited stdin), gets an
// empty stdin after the idle cutoff instead of waiting forever; input the
// caller pipes and closes still reaches it.
func TestPyreRunCodexInheritedStdin(t *testing.T) {
	e := newRunEnv(t)
	cmd := e.command(false, nil, []string{"PYRE_TEST_CODEX_STDIN_IDLE=300ms"}, "run", "--", "codex", "exec", "--json", "a prompt")
	cmd.Stdin = nil
	w, err := cmd.StdinPipe() // never written, closed only after the run
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var so, se syncBuffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil || !strings.Contains(so.String(), `"thread.started"`) || !strings.Contains(se.String(), "codex exec gets an empty stdin") {
			t.Fatalf("%v: stdout %q stderr %q", err, so.String(), se.String())
		}
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("codex still waiting on an open stdin; stderr %q", se.String())
	}
	// Piped input that arrives is passed (the cutoff is for no input).
	so2, se2, code := output(t, e.command(false, []byte("diff context\n"), []string{"PYRE_TEST_CODEX_STDIN_IDLE=300ms"}, "run", "--", "codex", "exec", "--json", "review this"))
	if code != 0 || !strings.HasSuffix(so2, "diff context\n") || strings.Contains(se2, "empty stdin") {
		t.Fatalf("piped: code %d stdout %q stderr %q", code, so2, se2)
	}
}

// With the prompt on stdin ("-"), an open stdin is waited for, and the
// quiet notice says the caller has not closed it instead of promising a
// stop it cannot make yet.
func TestCodexWatchQuietStdinOpen(t *testing.T) {
	var b bytes.Buffer
	w := &codexWatch{stderr: &b, then: "pyre run stops it at 5m0s and starts it once more", stdinOpen: func() bool { return true }}
	if w.quietNotice(3 * time.Minute) {
		t.Fatal("printed")
	}
	if !strings.Contains(b.String(), "its stdin is still open") || strings.Contains(b.String(), "stops it at") {
		t.Fatalf("notice %q", b.String())
	}
}

func TestCodexPromptArg(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"exec", "--json", "hi"}, true},
		{[]string{"exec", "--json", "-"}, false},
		{[]string{"exec", "--json"}, false},
		{[]string{"exec", "-m", "gpt", "-o", "out.txt", "hi"}, true},
		{[]string{"exec", "-m", "gpt"}, false},
		{[]string{"exec", "resume", fakeThreadID, "more"}, true},
		{[]string{"exec", "resume", fakeThreadID, "-"}, false},
		{[]string{"exec", "resume", fakeThreadID}, false},
		{[]string{"exec", "resume", "--last", "more"}, true},
		{[]string{"exec", "review"}, false},
		{[]string{"exec", "--json", "--", "-x prompt"}, true},
		{[]string{"exec", "--", "-"}, false},
	} {
		if got := codexPromptArg(tc.args); got != tc.want {
			t.Errorf("%v: got %v", tc.args, got)
		}
	}
}
