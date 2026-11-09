//go:build !windows

package shell

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// launchWithPrompt starts shell in an isolated HOME whose rc file (rcName)
// holds rc, with prompt as the initial prompt.
func launchWithPrompt(t *testing.T, a *Adapter, dir, run, shell, rcName, rc, prompt string) (contracts.RunHandle, error) {
	t.Helper()
	if _, err := exec.LookPath(shell); err != nil {
		t.Skip(shell + " not installed")
	}
	home := filepath.Join(dir, "home-"+run)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, rcName), []byte(rc), 0o600); err != nil {
		t.Fatal(err)
	}
	return a.Launch(context.Background(), contracts.ResolvedLaunch{RunID: run, SessionID: "ses_" + run, Harness: contracts.HarnessShell, Cwd: dir,
		Executable: shell, StateDir: filepath.Join(dir, "runs", run), InitialPrompt: prompt,
		Env: map[string]string{"HOME": home, "ZDOTDIR": home, "ENV": "", "HISTFILE": filepath.Join(home, ".hist")}})
}

func waitFile(t *testing.T, path string) string {
	t.Helper()
	var b []byte
	if !eventually(t, 10*time.Second, func() bool {
		var err error
		b, err = os.ReadFile(path)
		return err == nil && strings.HasSuffix(string(b), "done\n")
	}) {
		t.Fatalf("%s not written: %q", path, b)
	}
	return string(b)
}

// A shell session's initial prompt runs once, after the login rc files
// (which take a second and export a variable the command uses), with its
// quotes, "$" and line breaks intact: bash and zsh (line editors, seen
// in the terminal mode) and sh/dash (no line editor: the quiet screen).
func TestShellInitialPromptRuns(t *testing.T) {
	a, dir := newTestAdapter(t)
	cases := []struct{ name, shell, rcName string }{
		{"bash", "/bin/bash", ".bash_profile"},
		{"zsh", "zsh", ".zshrc"},
		{"sh", "/bin/sh", ".profile"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shell := tc.shell
			if p, err := exec.LookPath(shell); err == nil {
				shell = p
			}
			out := filepath.Join(dir, tc.name+".out")
			count := filepath.Join(dir, tc.name+".count")
			rc := "sleep 1\nexport RC_MARK=loaded\nPS1='$ '\n"
			// Quotes of both kinds, "$" left for the shell, a variable the
			// rc file set, a multi-line loop and a trailing newline.
			prompt := "echo x >> '" + count + "'\n" +
				"printf '%s\\n' \"it's \\\"quoted\\\" \\$HOME-not-expanded rc=$RC_MARK\" > '" + out + "'\n" +
				"for i in 1 2; do\n  echo \"line $i; `echo tick`\" >> '" + out + "'\ndone\n" +
				"echo done >> '" + out + "'\n"
			h, err := launchWithPrompt(t, a, dir, "run_p_"+tc.name, shell, tc.rcName, rc, prompt)
			if err != nil {
				t.Fatal(err)
			}
			got := waitFile(t, out)
			want := "it's \"quoted\" $HOME-not-expanded rc=loaded\nline 1; tick\nline 2; tick\ndone\n"
			if got != want {
				t.Fatalf("output %q, want %q", got, want)
			}
			time.Sleep(300 * time.Millisecond)
			if b, _ := os.ReadFile(count); string(b) != "x\n" {
				t.Fatalf("command ran %q times", b)
			}
			// The shell stays for the user afterwards.
			if obs, _ := a.Observe(context.Background(), h, ""); obs.State != contracts.RunRunning {
				t.Fatalf("after the prompt: %+v", obs)
			}
			if r, err := a.ReadTranscript(context.Background(), h, contracts.ReadSessionRequest{}); err == nil {
				t.Logf("screen:\n%s", r.Screen)
			}
			_, _ = a.Stop(context.Background(), h)
		})
	}
}

// A prompt longer than tmux's 16 KiB client command limit is typed whole:
// it never travels on a tmux command line.
func TestShellInitialPromptLong(t *testing.T) {
	a, dir := newTestAdapter(t)
	out := filepath.Join(dir, "long.out")
	payload := strings.Repeat("0123456789abcdef", 1500) // 24000 bytes
	prompt := "printf %s '" + payload + "' | wc -c | tr -d ' ' > '" + out + "'; echo done >> '" + out + "'"
	h, err := launchWithPrompt(t, a, dir, "run_long", "/bin/bash", ".bash_profile", "PS1='$ '\n", prompt)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background(), h)
	if got := waitFile(t, out); got != "24000\ndone\n" {
		t.Fatalf("output %q", got)
	}
}

// A shell that never becomes ready (an rc file still running) fails the
// launch within ReadyTimeout, and the command is never typed.
func TestShellInitialPromptNotReady(t *testing.T) {
	a, dir := newTestAdapter(t)
	a.o.ReadyTimeout = 2 * time.Second
	out := filepath.Join(dir, "never.out")
	start := time.Now()
	_, err := launchWithPrompt(t, a, dir, "run_never", "/bin/bash", ".bash_profile", "echo banner; sleep 30\n", "echo ran > '"+out+"'")
	if !contracts.IsCode(err, contracts.ErrUnavailable) || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("launch: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %s", d)
	}
	if ok, _ := a.srv.HasSession(context.Background(), "run_never"); ok {
		t.Fatal("the failed launch left its session")
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(out); err == nil {
		t.Fatal("the prompt ran")
	}
}

func TestInitialCommand(t *testing.T) {
	for in, want := range map[string]string{"": "", " \n\t": "", "ls\n": "ls", "a\nb\r\n\n": "a\nb", "  x": "  x"} {
		if got := initialCommand(in); got != want {
			t.Errorf("initialCommand(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"/bin/bash": "bash", "-zsh": "zsh", "zsh": "zsh", `C:\Program Files\PowerShell\7\pwsh.exe`: "pwsh"} {
		if got := commandName(in); got != want {
			t.Errorf("commandName(%q) = %q, want %q", in, got, want)
		}
	}
}
