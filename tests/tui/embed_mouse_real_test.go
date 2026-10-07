package tui_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/tmux"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// TestEmbeddedMouseRealHarness embeds a real Claude Code or Codex TUI
// (PYRE_REAL_MOUSE_HARNESS=claude|codex, the machine's default login,
// trivial prompts) running in a private tmux server with Pyre's generated
// config, and checks that after a few turns the wheel over the pane
// shows earlier conversation and brings the latest back, and that typing
// works. Manual verification only: skipped unless the variable is set.
// PYRE_REAL_MOUSE_DIR is the (trusted) project directory to start in.
func TestEmbeddedMouseRealHarness(t *testing.T) {
	harness := os.Getenv("PYRE_REAL_MOUSE_HARNESS")
	if harness == "" {
		t.Skip("set PYRE_REAL_MOUSE_HARNESS=claude|codex to run against a real harness")
	}
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}
	dir := os.Getenv("PYRE_REAL_MOUSE_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	conf := filepath.Join(t.TempDir(), "tmux.conf")
	if err := os.WriteFile(conf, []byte(tmux.DefaultConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("pyre-wembedmouse-real-%d", os.Getpid())
	run := func(args ...string) string {
		out, _ := exec.Command(bin, append([]string{"-L", name}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	t.Cleanup(func() {
		path := run("display", "-p", "#{socket_path}")
		run("kill-server")
		if strings.HasPrefix(path, "/") {
			_ = os.Remove(path)
		}
	})
	if out, err := exec.Command(bin, "-L", name, "-f", conf, "new-session", "-d", "-s", "embed", "-x", "100", "-y", "30", "-c", dir, harness).CombinedOutput(); err != nil {
		t.Fatalf("tmux new-session: %v: %s", err, out)
	}
	a := &mouseAPI{bin: bin, sock: run("display", "-p", "#{socket_path}")}
	var rec *tuitest.Recorder
	h, _ := start(t, 200, 50, func(api contracts.API) contracts.API {
		a.API = api
		rec = tuitest.NewRecorder(a)
		return rec
	}, func(o *tui.Options) { o.StartTerm = tui.StartPTY })
	selectSession(t, h, "ses_synth-a")
	text := func() string { return h.M.TermText("ses_synth-a") }
	h.WaitFor("harness ready", 60*sec, func() bool {
		return run("display", "-p", "-t", "embed", "#{mouse_any_flag}") == "1"
	})
	time.Sleep(2 * time.Second)
	t.Logf("mouse flags: %s", run("display", "-p", "-t", "embed", "any=#{mouse_any_flag} sgr=#{mouse_sgr_flag} alt=#{alternate_on}"))

	ask := func(prompt, want string) {
		t.Helper()
		h.Key("enter") // focus the pane
		h.WaitFor("pane focused", 10*sec, func() bool { return h.M.TermFocused() && run("list-clients", "-F", "#{client_readonly}") == "0" })
		h.Type(prompt)
		time.Sleep(500 * time.Millisecond)
		h.Key("enter")
		h.WaitFor("answer "+want, 120*sec, func() bool { return hasLine(strings.NewReplacer("⏺", "", "•", "").Replace(text()), want) })
		time.Sleep(2 * time.Second)
		h.Key("ctrl+]")
		h.WaitFor("watching", 10*sec, func() bool { return !h.M.TermFocused() })
	}
	ask("Print the words alpha-01 through alpha-60, one per line, nothing else.", "alpha-60")
	ask("Reply with exactly the word zebra-done and nothing else.", "zebra-done")
	t.Logf("before the wheel:\n%s", text())
	if strings.Contains(text(), "alpha-05") {
		t.Fatal("alpha-05 is already on screen; the pane is too tall for this check")
	}

	x, y := at(t, h, "zebra-done")
	for i := 0; i < 40 && !strings.Contains(text(), "alpha-05"); i++ {
		h.Send(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelUp})
		time.Sleep(150 * time.Millisecond)
		h.Settle()
	}
	if !strings.Contains(text(), "alpha-05") {
		t.Fatalf("wheel up did not show the earlier conversation:\n%s", text())
	}
	if h.M.TermFocused() {
		t.Fatal("the wheel took the keyboard")
	}
	t.Logf("after wheel up (watching, %d leases):\n%s", len(rec.Calls("AcquireLease")), text())
	latest := func() bool { return hasLine(strings.NewReplacer("⏺", "", "•", "").Replace(text()), "zebra-done") }
	scrolled := func() bool { return strings.Contains(text(), "Jump to bottom") || !latest() }
	for i := 0; i < 80 && scrolled(); i++ {
		h.Send(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelDown})
		time.Sleep(150 * time.Millisecond)
		h.Settle()
	}
	if scrolled() {
		t.Fatalf("wheel down did not return to the latest turn:\n%s", text())
	}
	t.Logf("after wheel down:\n%s", text())

	// Scroll up again; then a click on an element the harness draws
	// (Claude: "Jump to bottom (click)") focuses the pane and reaches the
	// application.
	for i := 0; i < 10; i++ {
		h.Send(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelUp})
		time.Sleep(100 * time.Millisecond)
	}
	h.WaitFor("scrolled up again", 10*sec, scrolled)
	target := "Ask Codex"
	if harness == "claude" {
		h.WaitFor("jump-to-bottom element", 10*sec, func() bool { return strings.Contains(text(), "Jump to bottom") })
		target = "Jump to bottom"
	}
	px, py := at(t, h, target)
	h.Send(tea.MouseClickMsg{X: px + 2, Y: py, Button: tea.MouseLeft})
	h.Send(tea.MouseReleaseMsg{X: px + 2, Y: py, Button: tea.MouseLeft})
	h.WaitFor("click focuses", 10*sec, func() bool { return h.M.TermFocused() })
	if harness == "claude" {
		h.WaitFor("the click jumped to the bottom", 10*sec, func() bool { return !scrolled() })
		t.Logf("after clicking Jump to bottom:\n%s", text())
	}
	h.Type("typed-after-click")
	h.WaitFor("typing after the click", 10*sec, func() bool { return strings.Contains(text(), "typed-after-click") })
	t.Logf("after click + typing:\n%s", text())
	h.Key("ctrl+u")
	h.Key("ctrl+]")
	h.Close()
}
