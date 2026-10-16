//go:build !windows

package tmux

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestServer returns an isolated server in a short temp dir, killed at
// cleanup. It skips when tmux is unavailable.
func testServer(t *testing.T) *Server {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir, err := os.MkdirTemp("", "pt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := NewServer("", filepath.Join(dir, "t.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteConfig(); err != nil {
		t.Fatal(err)
	}
	bin := s.Bin
	t.Cleanup(func() {
		// Kill with the real tmux: tests swap s.Bin for a shim in a later
		// t.TempDir, which is removed before this cleanup runs (LIFO). A
		// failed kill leaves a server with exit-empty off running forever
		// on a deleted socket.
		k := *s
		k.Bin = bin
		if err := k.KillServer(context.Background()); err != nil {
			t.Errorf("kill test tmux server: %v", err)
		}
	})
	return s
}

// The test server is killed at cleanup even when the test replaced its
// tmux binary with a shim living in a temp dir that is removed first.
func TestTestServerCleanupKillsServerAfterShimSwap(t *testing.T) {
	var pid int
	t.Run("shimmed", func(t *testing.T) {
		s, _ := shimServer(t)
		if err := s.EnsureServer(context.Background()); err != nil {
			t.Fatal(err)
		}
		out, err := s.Run(context.Background(), nil, "display-message", "-p", "#{pid}")
		if err != nil {
			t.Fatal(err)
		}
		if pid, err = strconv.Atoi(strings.TrimSpace(out)); err != nil {
			t.Fatalf("server pid %q", out)
		}
	})
	if pid == 0 {
		t.Fatal("no server started")
	}
	if !waitFor(t, 5*time.Second, func() bool { return !syscallAlive(pid) }) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("tmux server pid %d outlived its test", pid)
	}
}

func waitFor(t *testing.T, d time.Duration, f func() bool) bool {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if f() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return f()
}

func syscallAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestParseVersion(t *testing.T) {
	for in, want := range map[string][2]int{"tmux 3.4\n": {3, 4}, "tmux 3.3a": {3, 3}, "tmux next-3.6": {3, 6}} {
		v, err := ParseVersion(in)
		if err != nil || v.Major != want[0] || v.Minor != want[1] {
			t.Fatalf("%q => %+v %v", in, v, err)
		}
	}
	if _, err := ParseVersion("garbage"); err == nil {
		t.Fatal("expected error")
	}
	if !(Version{Major: 3, Minor: 4}).AtLeast(3, 2) || (Version{Major: 3, Minor: 1}).AtLeast(3, 2) {
		t.Fatal("AtLeast")
	}
}

func TestValidateSocketPath(t *testing.T) {
	if ValidateSocketPath("rel/sock") == nil {
		t.Fatal("relative accepted")
	}
	if ValidateSocketPath("/"+strings.Repeat("a", 120)) == nil {
		t.Fatal("long accepted")
	}
	if err := ValidateSocketPath("/tmp/x.sock"); err != nil {
		t.Fatal(err)
	}
}

func TestShellQuoteAndAttachArgv(t *testing.T) {
	cases := map[string]string{"abc": "abc", "": "''", "a b": "'a b'", "it's": `'it'"'"'s'`, "$(x)": "'$(x)'"}
	for in, want := range cases {
		if got := ShellQuote(in); got != want {
			t.Fatalf("ShellQuote(%q)=%q want %q", in, got, want)
		}
	}
	a := AttachArgv("/usr/bin/tmux", "/s/t.sock", "run_1", true)
	if strings.Join(a, " ") != "/usr/bin/tmux -u -S /s/t.sock set-option -g mouse on ; attach-session -t =run_1 -r" {
		t.Fatalf("attach argv %q", a)
	}
	r, err := RemoteAttachArgv("server", "/usr/bin/tmux", "/home/x/my dir/t.sock", "run_1", false)
	if err != nil {
		t.Fatal(err)
	}
	want := `bash -lc '/usr/bin/tmux -u -S '"'"'/home/x/my dir/t.sock'"'"' set-option -g mouse on '"'"';'"'"' attach-session -t =run_1'`
	if len(r) != 4 || r[0] != "ssh" || r[1] != "-t" || r[2] != "server" || r[3] != want {
		t.Fatalf("remote argv %q\nwant %q", r, want)
	}
	// The remote string must round-trip through two shells to the exact argv.
	if _, err := exec.LookPath("bash"); err == nil {
		out, err := exec.Command("bash", "-c", "set -- "+strings.TrimPrefix(r[3], "bash -lc ")+"; eval \"printf '%s\\n' $1\"").Output()
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Split(strings.TrimSpace(string(out)), "\n")
		wantArgv := AttachArgv("/usr/bin/tmux", "/home/x/my dir/t.sock", "run_1", false)
		if strings.Join(got, "|") != strings.Join(wantArgv, "|") {
			t.Fatalf("round trip %q want %q", got, wantArgv)
		}
	}
	if _, err := RemoteAttachArgv("-oProxyCommand=x", "tmux", "/s", "n", false); err == nil {
		t.Fatal("option-like alias accepted")
	}
}

func TestParseClientsAndPane(t *testing.T) {
	cs := parseClients("/dev/ttys001\t123\t0\tattached,focused\n/dev/ttys002\t124\t1\tattached,read-only\n")
	if len(cs) != 2 || cs[0].PID != 123 || cs[0].ReadOnly || !cs[1].ReadOnly || WritableClients(cs) != 1 {
		t.Fatalf("%+v", cs)
	}
	p, err := parsePane("%1\t42\t1\t3\tbash\n")
	if err != nil || !p.Dead || p.ExitStatus == nil || *p.ExitStatus != 3 || p.PID != 42 {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	v, err := s.Version(ctx)
	if err != nil || !v.AtLeast(3, 2) {
		t.Skipf("tmux >= 3.2 required: %v %v", v, err)
	}
	if ok, err := s.HasSession(ctx, "run_a"); ok || err != nil {
		t.Fatalf("has before start: %v %v", ok, err)
	}
	dir := t.TempDir()
	// Argument with spaces and shell metacharacters must arrive verbatim.
	err = s.NewSession(ctx, SessionSpec{Name: "run_a", Cwd: dir, Env: map[string]string{"PYRE_X": "v a l$HOME"},
		Argv: []string{"sh", "-c", `printf '%s|%s|%s\n' "$1" "$PYRE_X" "$(pwd)"; exec cat`, "sh", "a b;$(echo no)"}})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.HasSession(ctx, "run_a"); !ok || err != nil {
		t.Fatalf("has after start: %v %v", ok, err)
	}
	// Prefix must not match.
	if ok, _ := s.HasSession(ctx, "run_"); ok {
		t.Fatal("prefix matched")
	}
	realDir, _ := filepath.EvalSymlinks(dir)
	if !waitFor(t, 3*time.Second, func() bool {
		out, _ := s.CapturePane(ctx, "run_a", CaptureOptions{Join: true})
		return strings.Contains(out, "a b;$(echo no)|v a l$HOME|"+realDir) || strings.Contains(out, "a b;$(echo no)|v a l$HOME|"+dir)
	}) {
		out, _ := s.CapturePane(ctx, "run_a", CaptureOptions{})
		t.Fatalf("argv/env not verbatim:\n%s", out)
	}
	// Paste goes through stdin; cat echoes it back.
	if err := s.Paste(ctx, "run_a", "pyre-test", []byte("pasted 'quoted' $text")); err != nil {
		t.Fatal(err)
	}
	if err := s.SendKeys(ctx, "run_a", KeyEnter); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 3*time.Second, func() bool {
		out, _ := s.CapturePane(ctx, "run_a", CaptureOptions{})
		return strings.Count(out, "pasted 'quoted' $text") >= 2 // tty echo + cat output
	}) {
		out, _ := s.CapturePane(ctx, "run_a", CaptureOptions{})
		t.Fatalf("paste not echoed:\n%s", out)
	}
	if err := s.SendKeys(ctx, "run_a", Key("x")); err == nil {
		t.Fatal("literal key accepted")
	}
	p, err := s.Pane(ctx, "run_a")
	if err != nil || p.Dead || p.PID == 0 {
		t.Fatalf("pane %+v %v", p, err)
	}
	cs, err := s.ListClients(ctx, "run_a")
	if err != nil || len(cs) != 0 {
		t.Fatalf("clients %+v %v", cs, err)
	}
	// Exit status is retained (remain-on-exit).
	if err := s.SendKeys(ctx, "run_a", KeyCtrlD); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 3*time.Second, func() bool { p, _ := s.Pane(ctx, "run_a"); return p.Dead }) {
		t.Fatal("pane not dead after EOF")
	}
	p, _ = s.Pane(ctx, "run_a")
	if p.ExitStatus == nil || *p.ExitStatus != 0 {
		t.Fatalf("exit status %+v", p)
	}
	if err := s.RespawnPane(ctx, "run_a", SessionSpec{Argv: []string{"sh", "-c", "exit 7"}}); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 3*time.Second, func() bool {
		p, _ := s.Pane(ctx, "run_a")
		return p.Dead && p.ExitStatus != nil && *p.ExitStatus == 7
	}) {
		t.Fatal("respawned pane status")
	}
	names, err := s.ListSessions(ctx)
	if err != nil || len(names) != 1 || names[0] != "run_a" {
		t.Fatalf("sessions %v %v", names, err)
	}
	if err := s.KillSession(ctx, "run_a"); err != nil {
		t.Fatal(err)
	}
	if err := s.KillSession(ctx, "run_a"); err != nil {
		t.Fatalf("second kill: %v", err)
	}
	if _, err := s.Pane(ctx, "run_a"); !errors.Is(err, ErrNoSession) && !errors.Is(err, ErrNoServer) {
		t.Fatalf("pane of missing session: %v", err)
	}
}

func TestSingleArgvAndSocketMode(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	dir := t.TempDir()
	script := filepath.Join(dir, "my script.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho single-ok\nexec cat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.NewSession(ctx, SessionSpec{Name: "one", Argv: []string{script}}); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 3*time.Second, func() bool {
		out, _ := s.CapturePane(ctx, "one", CaptureOptions{})
		return strings.Contains(out, "single-ok")
	}) {
		t.Fatal("single argv with space did not run")
	}
	st, err := os.Stat(filepath.Dir(s.Socket))
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir mode %v %v", st.Mode(), err)
	}
}

func TestUnsetInheritedEnv(t *testing.T) {
	s := testServer(t)
	// The server (and so every pane) inherits these from its first client.
	s.Env = append(os.Environ(), "PYRE_AMBIENT_SECRET=leak", "PYRE_KEEP=k", "PYRE_EXPLICIT=old")
	ctx := context.Background()
	err := s.NewSession(ctx, SessionSpec{Name: "u", Env: map[string]string{"PYRE_EXPLICIT": "new"},
		Unset: []string{"PYRE_AMBIENT_SECRET", "PYRE_EXPLICIT"},
		Argv:  []string{"sh", "-c", `echo "[${PYRE_AMBIENT_SECRET-unset}][$PYRE_KEEP][$PYRE_EXPLICIT]"; exec cat`}})
	if err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 3*time.Second, func() bool {
		out, _ := s.CapturePane(ctx, "u", CaptureOptions{})
		return strings.Contains(out, "[unset][k][new]")
	}) {
		out, _ := s.CapturePane(ctx, "u", CaptureOptions{})
		t.Fatalf("unset not applied:\n%s", out)
	}
	if err := (SessionSpec{Name: "x", Argv: []string{"a", "b"}, Unset: []string{"BAD-NAME"}}).Validate(); err == nil {
		t.Fatal("invalid unset name accepted")
	}
}

func TestServerArgvHasNoSecrets(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	if err := s.NewSession(ctx, SessionSpec{Name: "sec", Env: map[string]string{"PYRE_SESSION_TOKEN": "synthetic-secret-123"},
		Argv: []string{"sh", "-c", "exec cat"}}); err != nil {
		t.Fatal(err)
	}
	out, err := s.Run(ctx, nil, "list-sessions", "-F", "#{pid}")
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(strings.Split(out, "\n")[0])
	cmd, err := exec.Command("ps", "-o", "command=", "-p", pid).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cmd), "synthetic-secret-123") || strings.Contains(string(cmd), "new-session") {
		t.Fatalf("tmux server argv leaks the first session's command: %s", cmd)
	}
}

func TestTerminateSessionKillsIgnoringGroup(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	// A process that ignores SIGHUP/SIGTERM, with a child in its group.
	if err := s.NewSession(ctx, SessionSpec{Name: "stubborn", Argv: []string{"sh", "-c", `trap "" HUP TERM; sleep 300 & wait`}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	p, err := s.Pane(ctx, "stubborn")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := s.TerminateSession(ctx, "stubborn", 500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if groupAlive(p.PID) {
		t.Fatal("process group survived TerminateSession")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("terminate took too long")
	}
	if ok, _ := s.HasSession(ctx, "stubborn"); ok {
		t.Fatal("session survived")
	}
}

func TestMissingServer(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir, _ := os.MkdirTemp("", "pt")
	defer os.RemoveAll(dir)
	s, err := NewServer("", filepath.Join(dir, "none.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if ok, err := s.HasSession(ctx, "x"); ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if names, err := s.ListSessions(ctx); err != nil || len(names) != 0 {
		t.Fatalf("%v %v", names, err)
	}
	if _, err := s.Pane(ctx, "x"); !errors.Is(err, ErrNoServer) {
		t.Fatalf("want ErrNoServer, got %v", err)
	}
	if err := s.KillServer(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestParsePaneSize(t *testing.T) {
	p, err := parsePane("%1\t42\t0\t\tclaude\t80\t23\n")
	if err != nil || p.Width != 80 || p.Height != 23 || p.CurrentCommand != "claude" {
		t.Fatalf("%+v %v", p, err)
	}
}

// attachClient attaches a real client of the given size to session name on
// s: a second private tmux server runs `tmux attach` in a pane of that size
// (a tty that a test can size). It returns a function that kills the outer
// server, which detaches the client.
func attachClient(t *testing.T, s *Server, name string, w, h int) func() {
	t.Helper()
	outer := filepath.Join(filepath.Dir(s.Socket), "outer.sock")
	argv := []string{"tmux", "-S", outer, "-f", "/dev/null", "new-session", "-d", "-x", strconv.Itoa(w), "-y", strconv.Itoa(h),
		"env", "-u", "TMUX", "TERM=xterm-256color", s.Bin, "-u", "-S", s.Socket, "attach-session", "-t", SessionTarget(name)}
	if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("outer tmux: %v %s", err, out)
	}
	stop := func() { _ = exec.Command("tmux", "-S", outer, "kill-server").Run() }
	t.Cleanup(stop)
	return stop
}

func TestResizeWindowAfterNarrowClientLeft(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	if v, err := s.Version(ctx); err != nil || !v.AtLeast(3, 2) {
		t.Skipf("tmux >= 3.2 required: %v %v", v, err)
	}
	if err := s.NewSession(ctx, SessionSpec{Name: "run_r", Argv: []string{"sh", "-c", "exec cat"}}); err != nil {
		t.Fatal(err)
	}
	size := func() (int, int) {
		p, err := s.Pane(ctx, "run_r")
		if err != nil {
			t.Fatal(err)
		}
		return p.Width, p.Height
	}
	if w, h := size(); w != 200 || h != 50 {
		t.Fatalf("new session %dx%d, want 200x50", w, h)
	}
	clients := func() int {
		cs, _ := s.ListClients(ctx, "run_r")
		return len(cs)
	}
	// An 80x24 client attaches: the window follows it (window-size latest)
	// and stays 80 columns wide after it leaves.
	detach := attachClient(t, s, "run_r", 80, 24)
	if !waitFor(t, 5*time.Second, func() bool { w, _ := size(); return w == 80 && clients() == 1 }) {
		w, h := size()
		t.Fatalf("attached: %dx%d clients=%d, want 80 wide", w, h, clients())
	}
	detach()
	if !waitFor(t, 5*time.Second, func() bool { return clients() == 0 }) {
		t.Fatal("client did not detach")
	}
	if w, _ := size(); w != 80 {
		t.Fatalf("after detach width %d, want it to stay 80 (the defect this guards)", w)
	}
	if err := s.ResizeWindow(ctx, "run_r", 200, 50); err != nil {
		t.Fatal(err)
	}
	if w, h := size(); w != 200 || h != 50 {
		t.Fatalf("after ResizeWindow %dx%d", w, h)
	}
	out, err := s.Run(ctx, nil, "show-options", "-w", "-t", PaneTarget("run_r"), "window-size")
	if err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("window-size left set on the window: %q %v", out, err)
	}
	// The policy is back to latest: the next client still gets its size.
	attachClient(t, s, "run_r", 100, 30)
	if !waitFor(t, 5*time.Second, func() bool { w, _ := size(); return w == 100 }) {
		w, h := size()
		t.Fatalf("re-attach: %dx%d, want 100 wide", w, h)
	}
	if err := s.ResizeWindow(ctx, "run_r", 0, 50); err == nil {
		t.Fatal("zero width accepted")
	}
}
