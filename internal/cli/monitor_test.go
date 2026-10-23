package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

func TestMonitorLoopWakesOnlyOnChange(t *testing.T) {
	outputs := []string{"pending", "pending", "pending", "success", "success", "failure"}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var sent []string
	failNext := false
	var log bytes.Buffer
	i := 0
	m := &monitorLoop{To: "ses_synth-target", Label: "ci", Cmd: "gh pr checks 1", Every: time.Minute, Out: &log, Self: "ses_synth-mon",
		Now: func() time.Time { return now },
		Sleep: func(ctx context.Context, d time.Duration) error {
			now = now.Add(d)
			if i >= len(outputs) {
				return context.Canceled
			}
			return nil
		},
		Check: func(ctx context.Context, cmd string) string { i++; return outputs[i-1] },
		Send: func(ctx context.Context, text string) error {
			if failNext {
				failNext = false
				return errors.New("hub unreachable")
			}
			sent = append(sent, text)
			return nil
		},
		Target: func(ctx context.Context) monitorTargetState { return monitorTargetState{} },
	}
	if err := m.run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("run: %v", err)
	}
	if len(sent) != 2 || !strings.Contains(sent[0], "success") || !strings.Contains(sent[0], "previous: pending") ||
		!strings.Contains(sent[1], "failure") || !strings.Contains(sent[1], "pyre monitor rm ses_synth-mon") {
		t.Fatalf("sent %d: %q\nlog:\n%s", len(sent), sent, log.String())
	}
	if !strings.Contains(log.String(), "baseline") || strings.Count(log.String(), "unchanged") != 3 {
		t.Fatalf("log:\n%s", log.String())
	}

	// A failed wake is retried on the next check; --until-change stops after
	// the first delivered change; --initial reports the first result.
	outputs, i, sent, failNext = []string{"a", "b", "b"}, 0, nil, true
	m.UntilChange, m.Initial = true, true
	if err := m.run(context.Background()); err != nil {
		t.Fatalf("until-change: %v", err)
	}
	if len(sent) != 1 || !strings.Contains(sent[0], "first result") || !strings.Contains(sent[0], "has stopped") {
		t.Fatalf("until-change sent %q", sent)
	}

	// --for ends the loop.
	outputs, i, sent = []string{"x", "x", "x", "x", "x"}, 0, nil
	m.UntilChange, m.Initial, m.For = false, false, 2*time.Minute
	if err := m.run(context.Background()); err != nil || i != 3 {
		t.Fatalf("for: %v after %d checks", err, i)
	}

	// While the target is mid-turn, changes are held and only the latest
	// is sent once it is idle.
	busy := 3
	m.Target = func(ctx context.Context) monitorTargetState {
		if busy > 0 {
			busy--
			return monitorTargetState{Busy: true, Why: "running"}
		}
		return monitorTargetState{}
	}
	outputs, i, sent, m.For = []string{"a", "b", "c", "d", "d"}, 0, nil, 0
	_ = m.run(context.Background())
	if len(sent) != 1 || !strings.Contains(sent[0], "d") || !strings.Contains(sent[0], "previous: a") || strings.Count(log.String(), "held until it is idle") != 2 {
		t.Fatalf("coalesced %q\nlog:\n%s", sent, log.String())
	}

	// A target that is archived ends the monitor.
	m.Target = func(ctx context.Context) monitorTargetState { return monitorTargetState{Gone: true, Why: "archived"} }
	outputs, i, sent = []string{"a"}, 0, nil
	if err := m.run(context.Background()); err != nil || i != 0 || !strings.Contains(log.String(), "is archived") {
		t.Fatalf("gone: %v %d", err, i)
	}

	// More than the hourly wake budget: a final notice, then it stops.
	m.Target = func(ctx context.Context) monitorTargetState { return monitorTargetState{} }
	outputs, i, sent = []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9"}, 0, nil
	if err := m.run(context.Background()); err != nil || len(sent) != monitorWakeBudget+1 || !strings.Contains(sent[len(sent)-1], "has stopped: its output changed more than 6 times") || i != monitorWakeBudget+2 {
		t.Fatalf("budget: %v, %d sent after %d checks: %q", err, len(sent), i, sent[len(sent)-1])
	}
}

func TestMonitorTargetOf(t *testing.T) {
	h := newHub(t)
	if st := monitorTargetOf(context.Background(), h.b, "ses_synth-nonexistent"); !st.Gone {
		t.Fatalf("missing target: %+v", st)
	}
	if st := monitorTargetOf(context.Background(), h.b, "ses_synth-a"); st.Gone {
		t.Fatalf("live target: %+v", st)
	}
}

func TestMonitorCheckReportsExitStatus(t *testing.T) {
	if out := runMonitorCheck(context.Background(), "printf 'ok  \\n\\n'; exit 3", 10*time.Second); out != "ok\n[exit 3]" {
		t.Fatalf("check: %q", out)
	}
	// A timeout kills the whole tree, grandchildren holding the pipe too.
	began := time.Now()
	if out := runMonitorCheck(context.Background(), "sleep 30 & sleep 30; wait", 200*time.Millisecond); !strings.Contains(out, "timed out") || time.Since(began) > 2*time.Second {
		t.Fatalf("timeout: %q after %s", out, time.Since(began))
	}
	if out := runMonitorCheck(context.Background(), "head -c 3000000 /dev/zero | tr '\\0' x", 10*time.Second); len(out) > monitorMaxCapture+100 || !strings.Contains(out, "bytes of output dropped") {
		t.Fatalf("capture cap: %d bytes, tail %q", len(out), out[len(out)-60:])
	}
}

func TestMonitorAddStartsShellSession(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	target := mustJSON[c.SessionView](t, run(t, e, nil, "--json", "sessions", "show", "ses_synth-a"))
	r := run(t, e, nil, "--json", "monitor", "add", "--session", "ses_synth-a", "--every", "10m", "--node", target.NodeName,
		"--cmd", "gh pr checks 7 | sort", "--until-change")
	expectCode(t, r, 0)
	res := mustJSON[c.StartSessionResponse](t, r)
	s := res.Session.Session
	if s.Harness != c.HarnessShell || !slices.Contains(s.Tags, TagMonitor) || !slices.Contains(s.Tags, "monitor-to:ses_synth-a") || !strings.HasPrefix(s.Title, "monitor: gh pr checks 7") {
		t.Fatalf("monitor session: %+v", s)
	}
	for shell, want := range map[string]string{
		"/bin/zsh":                               `-lc exec "${PYRE_CLI:-pyre}" monitor run`,
		"/usr/bin/fish":                          `if set -q PYRE_CLI; exec $PYRE_CLI monitor run; else; exec pyre monitor run; end`,
		`C:\Program Files\PowerShell\7\pwsh.exe`: `$p = if ($env:PYRE_CLI) { $env:PYRE_CLI } else { 'pyre' }; & $p monitor run`,
		`C:\Windows\system32\cmd.exe`:            `if defined PYRE_CLI ("%PYRE_CLI%" monitor run) else (pyre monitor run)`,
	} {
		if argv := strings.Join(monitorShellArgs(shell, []string{"monitor", "run"}), " "); !strings.Contains(argv, want) {
			t.Errorf("%s: %s", shell, argv)
		}
	}
	if b64 := base64.RawURLEncoding.EncodeToString([]byte("gh pr checks 7 | sort")); strings.ContainsAny(b64, " '\"$;|&") {
		t.Fatalf("encoded command needs quoting: %s", b64)
	}

	r = run(t, e, nil, "monitor", "ls")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, s.SessionID) || !strings.Contains(r.stdout, "ses_synth-a") {
		t.Fatalf("ls:\n%s", r.stdout)
	}

	// The same request (command, machine, directory, interval, lifetime,
	// until-change, initial) for the same target reuses the running monitor;
	// different flags start a second one.
	r = run(t, e, nil, "--json", "monitor", "add", "--session", "ses_synth-a", "--every", "10m", "--node", target.NodeName,
		"--cmd", "gh pr checks 7 | sort", "--until-change")
	expectCode(t, r, 0)
	again := mustJSON[MonitorAddResult](t, r)
	if !again.Reused || again.Session.Session.SessionID != s.SessionID {
		t.Fatalf("repeat add should reuse %s: %+v", s.SessionID, again)
	}
	if strings.Contains(r.stderr, "Monitor tool") {
		t.Fatalf("hint outside Claude Code:\n%s", r.stderr)
	}
	// Inside Claude Code the add still works and hints at the built-in wake.
	r = run(t, e.with("CLAUDECODE", "1"), nil, "monitor", "add", "--session", "ses_synth-a", "--every", "10m", "--node", target.NodeName, "--cmd", "gh pr checks 7 | sort", "--until-change")
	if expectCode(t, r, 0); !strings.Contains(r.stdout, "reused monitor "+s.SessionID) || !strings.Contains(r.stderr, "Monitor tool") {
		t.Fatalf("reuse output with the Claude hint:\n%s", r)
	}
	// $PYRE_HARNESS wins over CLAUDECODE (which may leak from whatever started pyred).
	// (An invalid --every: the hint comes first, nothing starts.)
	r = run(t, e.with("CLAUDECODE", "1", EnvHarness, "codex", EnvSessionID, "ses_synth-a"), nil, "monitor", "add", "--every", "30s", "--cmd", "true")
	if expectCode(t, r, 2); strings.Contains(r.stderr, "Monitor tool") {
		t.Fatalf("a Codex session gets no Claude hint:\n%s", r)
	}
	r = run(t, e.with(EnvHarness, "claude", EnvSessionID, "ses_synth-a"), nil, "monitor", "add", "--every", "30s", "--cmd", "true")
	if expectCode(t, r, 2); !strings.Contains(r.stderr, "Monitor tool") {
		t.Fatalf("a Claude session (PYRE_HARNESS) gets the hint:\n%s", r)
	}
	other := mustJSON[MonitorAddResult](t, run(t, e, nil, "--json", "monitor", "add", "--session", "ses_synth-a", "--every", "5m", "--node", target.NodeName, "--cmd", "gh pr checks 7 | sort", "--title", "ci every 5m"))
	if other.Reused || other.Session.Session.SessionID == s.SessionID {
		t.Fatalf("the same command with other flags is a new monitor: %+v", other)
	}
	// A monitor whose node lost contact is never reused.
	if err := h.b.SetRunState(other.Session.Session.CurrentRunID, c.RunUnknown, "synthetic lost contact"); err != nil {
		t.Fatal(err)
	}
	fresh := mustJSON[MonitorAddResult](t, run(t, e, nil, "--json", "monitor", "add", "--session", "ses_synth-a", "--every", "5m", "--node", target.NodeName, "--cmd", "gh pr checks 7 | sort", "--title", "ci every 5m"))
	if fresh.Reused || fresh.Session.Session.SessionID == other.Session.Session.SessionID {
		t.Fatalf("an unknown-state monitor was reused: %+v", fresh)
	}

	// Session lists leave monitors out and count them on their target.
	list := mustJSON[c.Page[c.SessionView]](t, run(t, e, nil, "--json", "sessions"))
	for _, v := range list.Items {
		if v.Session.IsMonitor() {
			t.Fatalf("pyre sessions lists monitor %s", v.Session.SessionID)
		}
		if v.Session.SessionID == "ses_synth-a" && len(v.Monitors) != 2 {
			t.Fatalf("target's monitors: %+v", v.Monitors)
		}
	}
	r = run(t, e, nil, "sessions")
	if expectCode(t, r, 0); strings.Contains(r.stdout, s.SessionID) || !strings.Contains(r.stdout, "(watching 2)") {
		t.Fatalf("human list:\n%s", r.stdout)
	}
	r = run(t, e, nil, "sessions", "--monitors")
	if expectCode(t, r, 0); !strings.Contains(r.stdout, s.SessionID) {
		t.Fatalf("--monitors list:\n%s", r.stdout)
	}
	r = run(t, e, nil, "sessions", "show", s.SessionID)
	if expectCode(t, r, 0); !strings.Contains(r.stdout, "monitor:   wakes ses_synth-a") {
		t.Fatalf("show monitor:\n%s", r.stdout)
	}
	r = run(t, e, nil, "sessions", "show", "ses_synth-a")
	if expectCode(t, r, 0); !strings.Contains(r.stdout, "watching:  "+s.SessionID) {
		t.Fatalf("show target:\n%s", r.stdout)
	}
	if r = run(t, e, nil, "sessions", "show", s.Title); r.code != 0 {
		t.Fatalf("a monitor resolves by its title:\n%s", r)
	}

	// pyre prune archives ended monitors.
	if err := h.b.SetRunState(other.Session.Session.CurrentRunID, c.RunExited, "synthetic done"); err != nil {
		t.Fatal(err)
	}
	pr := mustJSON[PruneResult](t, run(t, e, nil, "--json", "sessions", "prune", "--dry-run"))
	pruned := false
	for _, v := range pr.Archived {
		pruned = pruned || v.Session.SessionID == other.Session.Session.SessionID
	}
	if !pruned {
		t.Fatalf("prune skips the ended monitor: %+v", pr)
	}

	expectCode(t, run(t, e, nil, "monitor", "rm", "ses_synth-a"), 2) // not a monitor
	expectCode(t, run(t, e, nil, "monitor", "rm", s.SessionID), 0)
	expectCode(t, run(t, e, nil, "monitor", "add", "--every", "30s", "--cmd", "true", "--session", "ses_synth-a"), 2)
}

// $PYRE_HARNESS (set by pyred) names the caller's harness; CLAUDECODE=1
// counts only outside a Pyre session, where it cannot have leaked from
// whatever started pyred.
func TestCallerHarness(t *testing.T) {
	for _, tc := range []struct {
		env  env
		want string
	}{
		{env{EnvHarness: "codex", "CLAUDECODE": "1", EnvSessionID: "ses_synth-a"}, "codex"},
		{env{EnvHarness: "claude", EnvSessionID: "ses_synth-a"}, "claude"},
		{env{"CLAUDECODE": "1"}, "claude"},
		{env{"CLAUDECODE": "1", EnvSessionID: "ses_synth-a"}, ""},
		{env{}, ""},
	} {
		a := &App{Getenv: tc.env.get}
		if got := a.callerHarness(); got != tc.want {
			t.Errorf("%v: %q, want %q", tc.env, got, tc.want)
		}
	}
}
