//go:build !windows

package tmux

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// specialText is synthetic prompt text with everything a shell or tmux
// could reinterpret: quotes, newlines, "$", backticks, backslashes, tmux's
// ";" separator, a trailing ";", tabs, CR and non-ASCII.
const specialText = "it's \"quoted\" $HOME ${PATH} $(id) `whoami` \\n \\\\ ; echo hi; ~user *glob? [a-z] {a,b} #comment\n" +
	"line two\twith a tab\r\nünïcødé — 日本語 🔥 \x01\x7f end;"

// bigText repeats specialText to n bytes or more.
func bigText(n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(specialText)
	}
	return b.String()
}

// argvDumper returns a pane argv that writes its arguments (after the
// dumper's own) NUL-terminated to out, then stays up.
func argvDumper(out string, args ...string) []string {
	return append([]string{"sh", "-c", `out=$1; shift; for a in "$@"; do printf '%s\0' "$a"; done > "$out.tmp" && mv "$out.tmp" "$out"; exec cat`, "sh", out}, args...)
}

func wantArgv(t *testing.T, out string, want []string) {
	t.Helper()
	if !waitFor(t, 10*time.Second, func() bool { _, err := os.Stat(out); return err == nil }) {
		t.Fatal("pane did not write its argv")
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var wb bytes.Buffer
	for _, a := range want {
		wb.WriteString(a)
		wb.WriteByte(0)
	}
	if !bytes.Equal(b, wb.Bytes()) {
		t.Fatalf("pane argv differs (%d bytes, want %d)", len(b), wb.Len())
	}
}

// TestLaunchLongArgvRealTmux launches, with the real tmux on an isolated
// socket, a Claude-shaped argv far over tmux's 16 KiB client command limit
// (a 12 KB system prompt and a 20 KB initial prompt) with and without an
// environment, and respawns it: every argument reaches the pane byte for
// byte, and the tmux client argv stays small.
func TestLaunchLongArgvRealTmux(t *testing.T) {
	s, log := shimServer(t)
	ctx := context.Background()
	system, prompt := bigText(12<<10), bigText(20<<10)
	args := []string{"--settings", "/x/settings.json", "--append-system-prompt", system, "--plugin-dir=/x/plugin", "", specialText, prompt}
	dir := t.TempDir()

	withEnv := SessionSpec{Name: "long", Env: map[string]string{"PYRE_PLAIN": "plain"}, Argv: argvDumper(filepath.Join(dir, "a"), args...)}
	if err := s.NewSession(ctx, withEnv); err != nil {
		t.Fatalf("new-session with a long argv: %v", err)
	}
	wantArgv(t, filepath.Join(dir, "a"), args)

	// Control: the same argv on the tmux command line (the bug) is refused
	// by tmux. It bypasses the argv-recording shim.
	plain := *s
	var err error
	if plain.Bin, err = exec.LookPath("tmux"); err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Run(ctx, nil, append([]string{"new-session", "-d", "-s", "control", "--"}, argvDumper(filepath.Join(dir, "control"), args...)...)...); err == nil {
		t.Log("control: this tmux accepted the plain command line")
	} else if !strings.Contains(err.Error(), "too long") {
		t.Fatalf("control: unexpected error: %v", err)
	} else {
		t.Logf("control: the plain command line is refused: %v", err)
	}

	// Respawn (resume, account switch, daily restart) takes the same path.
	respawn := withEnv
	respawn.Argv = argvDumper(filepath.Join(dir, "b"), args...)
	if err := s.RespawnPane(ctx, "long", respawn); err != nil {
		t.Fatalf("respawn-pane with a long argv: %v", err)
	}
	wantArgv(t, filepath.Join(dir, "b"), args)

	// No environment: a long argv still goes through the launch file.
	if err := s.NewSession(ctx, SessionSpec{Name: "longplain", Argv: argvDumper(filepath.Join(dir, "c"), args...)}); err != nil {
		t.Fatalf("new-session with a long argv and no environment: %v", err)
	}
	wantArgv(t, filepath.Join(dir, "c"), args)

	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(s.Socket), envFilePrefix+"*")); len(m) != 0 {
		t.Fatalf("launch file left behind: %v", m)
	}
	lb, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(lb, []byte("\n")) {
		if len(line) > 2<<10 {
			t.Fatalf("a tmux client command is %d bytes: %.200q", len(line), line)
		}
	}
}

// TestShortArgvStaysInline: a spec without environment changes and a short
// argv keeps the plain command (no launcher, no file); one over
// maxInlineArgv goes through the launcher.
func TestShortArgvStaysInline(t *testing.T) {
	s := &Server{Bin: "/usr/bin/tmux", Socket: filepath.Join(t.TempDir(), "sock", "t.sock"), ExecHelper: "/opt/pyre/pyred"}
	cmd, file, err := s.paneCommand(SessionSpec{Name: "p", Argv: []string{"sh", "-c", "x;"}})
	if err != nil || file != "" || strings.Join(cmd, " ") != `sh -c x\;` {
		t.Fatalf("short argv: %q %q %v", cmd, file, err)
	}
	long := []string{"sh", "-c", strings.Repeat("x", maxInlineArgv)}
	cmd, file, err = s.paneCommand(SessionSpec{Name: "p", Argv: long})
	if err != nil || file == "" || strings.Join(cmd, " ") != "/opt/pyre/pyred "+ExecEnvArg+" "+file+" --" {
		t.Fatalf("long argv: %q %q %v", cmd, file, err)
	}
	lf, err := readLaunchFile(file)
	if err != nil || strings.Join(lf.Argv, "\x00") != strings.Join(long, "\x00") || len(lf.Set)+len(lf.Unset) != 0 {
		t.Fatalf("launch file %+v %v", lf, err)
	}
}

// TestLaunchFileRoundTrip: argv elements (empty ones, special characters,
// large ones) and the environment survive encodeLaunch/readLaunchFile.
func TestLaunchFileRoundTrip(t *testing.T) {
	argv := []string{"claude", "", specialText, bigText(64 << 10), "a=b", "S", "U", "A"}
	env := map[string]string{"K": specialText, "EMPTY": ""}
	p := filepath.Join(t.TempDir(), "launch")
	if err := os.WriteFile(p, encodeLaunch(env, []string{"GONE"}, argv), 0o600); err != nil {
		t.Fatal(err)
	}
	lf, err := readLaunchFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(lf.Argv, "\x00") != strings.Join(argv, "\x00") || len(lf.Argv) != len(argv) {
		t.Fatalf("argv %d elements, want %d", len(lf.Argv), len(argv))
	}
	if strings.Join(lf.Set, "\x00") != "EMPTY=\x00K="+specialText || strings.Join(lf.Unset, ",") != "GONE" {
		t.Fatalf("env set=%q unset=%q", lf.Set, lf.Unset)
	}
	// A truncated file is refused, not run with a partial argv.
	b := encodeLaunch(nil, nil, []string{"sh", "-c", "x"})
	if err := os.WriteFile(p, b[:len(b)-1], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLaunchFile(p); err == nil {
		t.Fatal("unterminated launch file accepted")
	}
}
