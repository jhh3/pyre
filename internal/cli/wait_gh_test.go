package cli

import (
	"context"
	"encoding/base64"
	"slices"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

func TestWaitGitHubStartsMonitor(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	target := mustJSON[c.SessionView](t, run(t, e, nil, "--json", "sessions", "show", "ses_synth-a"))
	r := run(t, e, nil, "--json", "wait", "gh-pr", "https://github.com/synth-org/synth-repo/pull/7/files", "--until", "review-from:gemini-code-assist[bot]",
		"--to", "ses_synth-a", "--node", target.NodeName, "--timeout", "3h")
	expectCode(t, r, 0)
	res := mustJSON[WaitGitHubResult](t, r)
	if res.Target != "synth-org/synth-repo#7" || res.Until != "review-from:gemini-code-assist[bot]" || res.Timeout != "3h0m0s" || res.Notify != "ses_synth-a" || res.Reused {
		t.Fatalf("result: %+v", res)
	}
	sv := mustJSON[c.SessionView](t, run(t, e, nil, "--json", "sessions", "show", res.WaitID))
	s := sv.Session
	if !s.IsMonitor() || !s.IsGitHubWait() || s.MonitorTarget() != "ses_synth-a" || !strings.HasPrefix(s.Title, "monitor: wait gh-pr synth-org/synth-repo#7 until review-from") {
		t.Fatalf("wait session: %+v", s)
	}
	// The loop's arguments are shell-safe (the condition's [bot] is base64)
	// and parse back to the same wait.
	tg, cond, timeout, _ := parseWait(WaitGitHubParams{Kind: "gh-run", Target: "synth-org/synth-repo 900", Until: "failed"})
	args := waitRunArgs(tg, cond, "ses_synth-a", timeout, time.Time{})
	if argv := strings.Join(args, " "); strings.ContainsAny(argv, "[]#'\"$;|&*?") || !strings.Contains(argv, "--to ses_synth-a --timeout 2h0m0s") {
		t.Fatalf("args: %s", argv)
	}
	tb, _ := base64.RawURLEncoding.DecodeString(args[5])
	if back, _, _, err := parseWait(WaitGitHubParams{Kind: args[3], Target: string(tb), Until: "done"}); err != nil || back != tg {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	// The same wait again reuses it; another condition is a new one.
	again := mustJSON[WaitGitHubResult](t, run(t, e, nil, "--json", "wait", "gh-pr", "synth-org/synth-repo#7", "--until", "review-from:gemini-code-assist[bot]",
		"--to", "ses_synth-a", "--node", target.NodeName, "--timeout", "3h"))
	if !again.Reused || again.WaitID != res.WaitID {
		t.Fatalf("repeat: %+v", again)
	}
	other := mustJSON[WaitGitHubResult](t, run(t, e, nil, "--json", "wait", "gh-run", "synth-org/synth-repo", "900", "--until", "failed",
		"--to", "ses_synth-a", "--node", target.NodeName))
	if other.Reused || other.WaitID == res.WaitID || other.Target != "synth-org/synth-repo run 900" || other.Timeout != "2h0m0s" {
		t.Fatalf("run wait: %+v", other)
	}
	// --after (checks-done after a rerun) is its own wait and reaches the loop.
	plain := mustJSON[WaitGitHubResult](t, run(t, e, nil, "--json", "wait", "gh-pr", "synth-org/synth-repo#7", "--until", "checks-done",
		"--to", "ses_synth-a", "--node", target.NodeName))
	barrier := mustJSON[WaitGitHubResult](t, run(t, e, nil, "--json", "wait", "gh-pr", "synth-org/synth-repo#7", "--until", "checks-done", "--after", "now",
		"--to", "ses_synth-a", "--node", target.NodeName))
	if barrier.Reused || barrier.WaitID == plain.WaitID || barrier.After == "" {
		t.Fatalf("--after wait: %+v (plain %+v)", barrier, plain)
	}
	prT, prCond, _, _ := parseWait(WaitGitHubParams{Kind: "gh-pr", Target: "synth-org/synth-repo#7", Until: "checks-done"})
	if argv := strings.Join(waitRunArgs(prT, prCond, "ses_synth-a", timeout, time.Unix(1794000000, 0)), " "); !strings.HasSuffix(argv, "--after-unix 1794000000") {
		t.Fatalf("--after not passed to the loop: %s", argv)
	}
	for _, in := range []string{"now", "10m", "2026-11-06T23:12:00Z", "15:04"} {
		if at, err := parseAfter(in, time.Now()); err != nil || at.IsZero() {
			t.Errorf("parseAfter(%q) = %v, %v", in, at, err)
		}
	}
	// ls lists both (and monitor ls shows them as monitors); a plain monitor is not a wait.
	mon := mustJSON[MonitorAddResult](t, run(t, e, nil, "--json", "monitor", "add", "--session", "ses_synth-a", "--every", "10m", "--node", target.NodeName, "--cmd", "true"))
	waits := mustJSON[[]c.SessionView](t, run(t, e, nil, "--json", "wait", "ls"))
	var ids []string
	for _, w := range waits {
		ids = append(ids, w.Session.SessionID)
	}
	if !slices.Contains(ids, res.WaitID) || !slices.Contains(ids, other.WaitID) || slices.Contains(ids, mon.Session.Session.SessionID) {
		t.Fatalf("wait ls: %v", ids)
	}
	r = run(t, e, nil, "monitor", "ls")
	if expectCode(t, r, 0); !strings.Contains(r.stdout, res.WaitID) {
		t.Fatalf("monitor ls:\n%s", r.stdout)
	}
	r = run(t, e, nil, "wait", "ls")
	if expectCode(t, r, 0); !strings.Contains(r.stdout, "gh-run synth-org/synth-repo run 900 until failed") {
		t.Fatalf("wait ls:\n%s", r.stdout)
	}
	expectCode(t, run(t, e, nil, "wait", "cancel", mon.Session.Session.SessionID), 2) // a monitor, not a wait
	expectCode(t, run(t, e, nil, "wait", "cancel", other.WaitID), 0)
	if s := mustJSON[c.SessionView](t, run(t, e, nil, "--json", "sessions", "show", other.WaitID)).Session; !s.Archived && !slices.Contains(s.Tags, c.TagArchiveWhenDone) {
		t.Fatalf("cancelled wait still live: %+v", s)
	}

	// Bad input is a usage error before anything starts.
	for _, args := range [][]string{
		{"wait", "gh-pr", "synth-org/synth-repo", "--until", "merged", "--to", "ses_synth-a"},
		{"wait", "gh-pr", "synth-org/synth-repo#7", "--until", "approved", "--to", "ses_synth-a"},
		{"wait", "gh-pr", "synth-org/synth-repo#7", "--to", "ses_synth-a"},
		{"wait", "gh-pr", "synth-org/synth-repo#7", "--until", "merged", "--after", "now", "--to", "ses_synth-a"},
		{"wait", "gh-pr", "synth-org/synth-repo#7", "--until", "checks-done", "--after", "yesterday-ish", "--to", "ses_synth-a"},
		{"wait", "gh-run", "https://github.com/o/r/actions/runs/1", "--until", "merged", "--to", "ses_synth-a"},
		{"wait", "gh-pr", "synth-org/synth-repo#7", "--until", "merged", "--timeout", "10s", "--to", "ses_synth-a"},
	} {
		expectCode(t, run(t, e, nil, args...), 2)
	}
	// Outside a session with no --to there is nobody to message.
	expectCode(t, run(t, e, nil, "wait", "gh-pr", "synth-org/synth-repo#7", "--until", "merged"), 2)
}

// recipientAPI answers GetSession from a table (the rest of c.API is unused).
type recipientAPI struct {
	c.API
	sessions map[string]c.SessionView
}

func (r recipientAPI) GetSession(_ context.Context, req c.GetRequest) (c.SessionView, error) {
	if sv, ok := r.sessions[req.ID]; ok {
		return sv, nil
	}
	return c.SessionView{}, c.Errorf(c.ErrNotFound, "no session %s", req.ID)
}

// The message goes to the waiting session while it lives, else to the
// nearest live session above it.
func TestWaitRecipient(t *testing.T) {
	sess := func(id, parent string, archived bool, st c.RunState) c.SessionView {
		sv := c.SessionView{Session: c.Session{SessionID: id, Archived: archived, Lineage: c.Lineage{ParentSessionID: parent}}}
		if st != "" {
			sv.Run = &c.Run{State: st}
		}
		return sv
	}
	api := recipientAPI{sessions: map[string]c.SessionView{
		"ses_synth-lead":     sess("ses_synth-lead", "", false, c.RunWaitingInput),
		"ses_synth-worker":   sess("ses_synth-worker", "ses_synth-lead", false, c.RunRunning),
		"ses_synth-archived": sess("ses_synth-archived", "ses_synth-lead", true, c.RunExited),
		"ses_synth-ended":    sess("ses_synth-ended", "ses_synth-archived", false, c.RunExited),
		"ses_synth-orphan":   sess("ses_synth-orphan", "", true, c.RunExited),
		"ses_synth-alone":    sess("ses_synth-alone", "", false, c.RunExited),
	}}
	for _, tc := range []struct{ to, want, why string }{
		{"ses_synth-worker", "ses_synth-worker", ""},
		{"ses_synth-archived", "ses_synth-lead", "ses_synth-archived was archived"},
		{"ses_synth-ended", "ses_synth-lead", "ses_synth-ended has ended"}, // past an archived parent too
		{"ses_synth-alone", "ses_synth-alone", ""},                         // ended with nobody above: its mail waits
	} {
		got, why, err := waitRecipient(context.Background(), api, tc.to)
		if err != nil || got != tc.want || why != tc.why {
			t.Errorf("%s: got %s %q %v; want %s %q", tc.to, got, why, err, tc.want, tc.why)
		}
	}
	if _, _, err := waitRecipient(context.Background(), api, "ses_synth-orphan"); err == nil {
		t.Error("archived with no parent: want an error")
	}
	if _, _, err := waitRecipient(context.Background(), api, "ses_synth-missing"); err == nil {
		t.Error("gone with no parent: want an error")
	}
}
