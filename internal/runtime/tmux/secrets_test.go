//go:build !windows

package tmux

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Obviously synthetic secrets; tests assert they never reach a command line.
const (
	synthToken  = "SYNTHETIC-pyre-session-token-4f1d9c"
	synthAPIKey = "SYNTHETIC-api-key-it's \"quoted\" $HOME\nline2 ;|&"
)

func secretSpec(name string, argv ...string) SessionSpec {
	return SessionSpec{Name: name, Env: map[string]string{
		"PYRE_SESSION_TOKEN": synthToken, "PYRE_TEST_API_KEY": synthAPIKey, "PYRE_PLAIN": "plain",
	}, Unset: []string{"PYRE_AMBIENT"}, Argv: argv}
}

func containsSecret(s string) bool {
	return strings.Contains(s, synthToken) || strings.Contains(s, "SYNTHETIC-api-key")
}

// TestLaunchArgvHasNoSecrets checks the exact tmux client argv for
// new-session and respawn-pane: no environment value appears, the pane
// command is the launcher with a private file, and that file holds the
// environment.
func TestLaunchArgvHasNoSecrets(t *testing.T) {
	dir := t.TempDir()
	s := &Server{Bin: "/usr/bin/tmux", Socket: filepath.Join(dir, "sock", "t.sock"), Config: "/dev/null", ExecHelper: "/opt/pyre/pyred"}
	spec := secretSpec("run_x", "claude", "--settings", "/x/settings.json")
	for _, build := range []func(SessionSpec) ([]string, string, error){s.newSessionArgs, s.respawnArgs} {
		args, envFile, err := build(spec)
		if err != nil {
			t.Fatal(err)
		}
		full := strings.Join(s.argv(args...), "\x00")
		if containsSecret(full) || strings.Contains(full, "-e\x00") || strings.Contains(full, "PYRE_SESSION_TOKEN") {
			t.Fatalf("tmux argv carries the environment: %q", full)
		}
		want := []string{"--", "/opt/pyre/pyred", ExecEnvArg, envFile, "--", "claude", "--settings", "/x/settings.json"}
		if got := args[len(args)-len(want):]; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("pane command %q, want %q", got, want)
		}
		st, err := os.Stat(envFile)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("env file mode %v %v", st, err)
		}
		if dst, _ := os.Stat(filepath.Dir(envFile)); dst.Mode().Perm() != 0o700 {
			t.Fatalf("env dir mode %v", dst.Mode())
		}
		set, unset, err := readEnvFile(envFile)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(envFile); !os.IsNotExist(err) {
			t.Fatalf("env file not deleted after read: %v", err)
		}
		joined := strings.Join(set, "\x00")
		if !strings.Contains(joined, "PYRE_SESSION_TOKEN="+synthToken) || !strings.Contains(joined, "PYRE_TEST_API_KEY="+synthAPIKey) ||
			len(unset) != 1 || unset[0] != "PYRE_AMBIENT" {
			t.Fatalf("env file content set=%q unset=%q", set, unset)
		}
	}
	// No environment: plain argv, no launcher, no file.
	args, envFile, err := s.newSessionArgs(SessionSpec{Name: "p", Argv: []string{"sh", "-l"}})
	if err != nil || envFile != "" || args[len(args)-2] != "sh" {
		t.Fatalf("plain spec: %q %q %v", args, envFile, err)
	}
	if err := (SessionSpec{Name: "n", Argv: []string{"a", "b"}, Env: map[string]string{"K": "a\x00b"}}).Validate(); err == nil {
		t.Fatal("NUL value accepted")
	}
}

func TestReadEnvFileRefusesSharedFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(p, encodeEnv(map[string]string{"A": "b"}, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readEnvFile(p); err == nil {
		t.Fatal("group/world-readable env file accepted")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("refused env file not removed")
	}
}

// shimServer returns a test server whose tmux binary is a shim recording
// every client argv (one NUL-joined line per call) before exec'ing tmux.
func shimServer(t *testing.T) (*Server, string) {
	t.Helper()
	s := testServer(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "argv.log")
	shim := filepath.Join(dir, "tmux-shim")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\0' \"$a\"; done >> " + ShellQuote(log) + "\nprintf '\\n' >> " + ShellQuote(log) +
		"\nexec " + ShellQuote(s.Bin) + " \"$@\"\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s.Bin = shim
	return s, log
}

// TestLaunchEnvReachesPaneOnly runs real tmux: the pane process gets the
// environment exactly (quotes, newlines, $), Unset applies, but no tmux
// client argv, the session/global environment, or a window opened later in
// the session holds it, and the env file is gone once the pane started.
func TestLaunchEnvReachesPaneOnly(t *testing.T) {
	s, log := shimServer(t)
	s.Env = append(os.Environ(), "PYRE_AMBIENT=ambient")
	ctx := context.Background()
	out := filepath.Join(t.TempDir(), "pane.env")
	spec := secretSpec("sec", "sh", "-c", `env > "$1"; exec cat`, "sh", out)
	if err := s.NewSession(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 5*time.Second, func() bool { b, _ := os.ReadFile(out); return bytes.Contains(b, []byte("PYRE_PLAIN=plain")) }) {
		pane, _ := s.CapturePane(ctx, "sec", CaptureOptions{})
		t.Fatalf("pane did not start:\n%s", pane)
	}
	time.Sleep(100 * time.Millisecond)
	b, _ := os.ReadFile(out)
	if !bytes.Contains(b, []byte("PYRE_SESSION_TOKEN="+synthToken+"\n")) || !bytes.Contains(b, []byte("PYRE_TEST_API_KEY="+synthAPIKey+"\n")) {
		t.Fatalf("pane environment lacks the secrets:\n%s", b)
	}
	if bytes.Contains(b, []byte("PYRE_AMBIENT=")) {
		t.Fatal("Unset not applied")
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(s.Socket), envFilePrefix+"*")); len(m) != 0 {
		t.Fatalf("env file left behind: %v", m)
	}
	// Respawn with a new secret environment.
	out2 := filepath.Join(t.TempDir(), "pane2.env")
	if err := s.RespawnPane(ctx, "sec", secretSpec("", "sh", "-c", `env > "$1"; exec cat`, "sh", out2)); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 5*time.Second, func() bool { b, _ := os.ReadFile(out2); return bytes.Contains(b, []byte(synthToken)) }) {
		t.Fatal("respawned pane lacks the token")
	}
	// The session and global environments never held it, so a window a
	// human opens later does not inherit it.
	for _, args := range [][]string{{"show-environment", "-t", SessionTarget("sec")}, {"show-environment", "-g"}} {
		env, err := s.Run(ctx, nil, args...)
		if err != nil {
			t.Fatal(err)
		}
		if containsSecret(env) || strings.Contains(env, "PYRE_SESSION_TOKEN") {
			t.Fatalf("%v holds the secret:\n%s", args, env)
		}
	}
	out3 := filepath.Join(t.TempDir(), "window.env")
	if _, err := s.Run(ctx, nil, "new-window", "-t", SessionTarget("sec"), "--", "sh", "-c", `env > "$1"; exec cat`, "sh", out3); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 5*time.Second, func() bool { b, _ := os.ReadFile(out3); return len(b) > 0 }) {
		t.Fatal("new window did not start")
	}
	time.Sleep(100 * time.Millisecond)
	if b, _ := os.ReadFile(out3); containsSecret(string(b)) || bytes.Contains(b, []byte("PYRE_SESSION_TOKEN")) {
		t.Fatal("a window opened later inherited the run's secret environment")
	}
	lb, err := os.ReadFile(log)
	if err != nil || !bytes.Contains(lb, []byte("new-session")) || !bytes.Contains(lb, []byte("respawn-pane")) {
		t.Fatalf("shim log incomplete: %v", err)
	}
	if containsSecret(string(lb)) || bytes.Contains(lb, []byte("PYRE_SESSION_TOKEN")) {
		t.Fatalf("a tmux client argv carried the secret environment:\n%q", lb)
	}
}

// processCommandLines returns the command line of every process: on Linux
// from /proc/*/cmdline (NULs as spaces), elsewhere from ps.
func processCommandLines() []string {
	if runtime.GOOS == "linux" {
		ents, _ := os.ReadDir("/proc")
		var out []string
		for _, e := range ents {
			if e.Name()[0] < '0' || e.Name()[0] > '9' {
				continue
			}
			if b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline"); err == nil && len(b) > 0 {
				out = append(out, string(bytes.ReplaceAll(b, []byte{0}, []byte{' '})))
			}
		}
		return out
	}
	b, _ := exec.Command("ps", "-axww", "-o", "command=").Output()
	return strings.Split(string(b), "\n")
}

// slowShim writes an executable that sleeps before exec'ing target with its
// arguments, so short-lived processes (the tmux client of new-session and
// respawn-pane, the pane launcher) stay in the process table long enough
// for a ps-based sampler (macOS) to see their command line, which the shim
// shares.
func slowShim(t *testing.T, target, match string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "slow-shim")
	script := "#!/bin/sh\ncase \" $* \" in *" + match + "*) sleep 0.15;; esac\nexec " + ShellQuote(target) + " \"$@\"\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLaunchProcessTableHasNoSecrets samples every process's command line
// continuously while sessions with secrets are created and respawned, and
// requires that the samples see the tmux clients, the pane launcher and the
// panes (a canary argument) but never a secret.
func TestLaunchProcessTableHasNoSecrets(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	const canary = "pyre-argv-canary-7c2e"
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s.Bin = slowShim(t, s.Bin, "leak") // the sessions below are named leak*
	s.ExecHelper = slowShim(t, exe, ExecEnvArg)
	var clientSeen, launcherSeen atomic.Int64
	var (
		stop             atomic.Bool
		wg               sync.WaitGroup
		samples, canSeen atomic.Int64
		mu               sync.Mutex
		leaks            []string
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			for _, l := range processCommandLines() {
				if containsSecret(l) {
					mu.Lock()
					leaks = append(leaks, l)
					mu.Unlock()
				}
				if strings.Contains(l, canary) {
					canSeen.Add(1)
				}
				if strings.Contains(l, " -t =leak") || strings.Contains(l, " -s leak") {
					clientSeen.Add(1)
				}
				if strings.Contains(l, ExecEnvArg) && strings.Contains(l, canary) {
					launcherSeen.Add(1)
				}
			}
			samples.Add(1)
		}
	}()
	for i := 0; i < 5; i++ {
		name := "leak" + string(rune('a'+i))
		if err := s.NewSession(ctx, secretSpec(name, "sh", "-c", "sleep 30; exit 0", canary)); err != nil {
			t.Fatal(err)
		}
		if err := s.RespawnPane(ctx, name, secretSpec("", "sh", "-c", "sleep 30; exit 0", canary)); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
	t.Logf("%d process-table samples; seen: tmux clients %d, pane launcher %d, panes %d",
		samples.Load(), clientSeen.Load(), launcherSeen.Load(), canSeen.Load())
	if len(leaks) > 0 {
		t.Fatalf("secret on a command line: %q", leaks)
	}
	if canSeen.Load() == 0 || clientSeen.Load() == 0 || launcherSeen.Load() == 0 {
		t.Fatal("sampler missed the clients, launcher or panes; the check is not effective")
	}
}
