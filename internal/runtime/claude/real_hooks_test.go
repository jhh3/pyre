package claude

// Real Claude Code check of the registered hook event names (S-E / review
// #12). Skipped unless PYRE_REAL_HARNESS=1. Uses the default login, an
// isolated tmux server, per-run --settings hooks, and three tiny prompts:
//
//  1. a Bash call denied through Pyre's broker (nothing runs);
//  2. a Bash call denied in the TUI dialog (Escape; nothing runs);
//  3. an allowed Bash call that fails (`touch` into a missing directory).
//
//   PYRE_REAL_HARNESS=1 PYRE_REAL_CWD=/path/to/scratch go test -run TestRealClaudeHookNames -v ./internal/runtime/claude/

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

func TestRealClaudeHookNames(t *testing.T) {
	if os.Getenv("PYRE_REAL_HARNESS") != "1" {
		t.Skip("set PYRE_REAL_HARNESS=1 to run against the real claude")
	}
	cwd := os.Getenv("PYRE_REAL_CWD")
	if cwd == "" {
		t.Fatal("PYRE_REAL_CWD must name a scratch directory (it will be trusted)")
	}
	dir, err := os.MkdirTemp("", "prh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	o := realOptions(t, dir)
	o.TrustWorkspace = func(c string) bool { return c == cwd }
	a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	defer a.srv.KillServer(ctx)

	l := contracts.ResolvedLaunch{RunID: "run_hooknames", SessionID: "ses_hooknames", Harness: contracts.HarnessClaude, Cwd: cwd,
		StateDir: filepath.Join(dir, "runs", "run_hooknames"), Args: realArgs}
	h, err := a.Launch(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	screen := func() string {
		s, _ := a.srv.CapturePane(ctx, h.TmuxTarget, tmux.CaptureOptions{})
		return s
	}
	wait := func(d time.Duration, label string, pred func(contracts.Observation) bool) contracts.Observation {
		t.Helper()
		end := time.Now().Add(d)
		var obs contracts.Observation
		for time.Now().Before(end) {
			obs, _ = a.Observe(ctx, h, "")
			if pred(obs) {
				t.Logf("%s: state=%s reason=%q", label, obs.State, obs.StateReason)
				return obs
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("%s: timeout; state=%s reason=%q\n%s", label, obs.State, obs.StateReason, screen())
		return obs
	}
	idle := func(o contracts.Observation) bool { return o.State == contracts.RunWaitingInput }
	wait(60*time.Second, "started", idle)

	// 1. No settings dialog for the registered names.
	s := screen()
	for _, bad := range []string{"Settings Warning", "Settings Error", "Unknown hook event", "Invalid settings"} {
		if strings.Contains(s, bad) {
			t.Fatalf("claude shows %q for the registered hook events %v:\n%s", bad, HookEvents, s)
		}
	}
	t.Logf("no settings dialog with %d hook events: %v", len(HookEvents), HookEvents)

	seen := 0 // events already reported
	newEvents := func() []hookRecord {
		recs, _, _ := readEvents(h.StateDir, 0)
		out := recs[seen:]
		seen = len(recs)
		var names []string
		for _, r := range out {
			names = append(names, r.Event)
		}
		t.Logf("  hook events: %s", strings.Join(names, " "))
		return out
	}
	newEvents()
	pending := func(label string) contracts.Approval {
		t.Helper()
		obs := wait(120*time.Second, label, func(o contracts.Observation) bool {
			for _, ap := range o.Approvals {
				if ap.State == contracts.ApprovalPending {
					return true
				}
			}
			return false
		})
		for _, x := range obs.Approvals {
			if x.State == contracts.ApprovalPending {
				return x
			}
		}
		return contracts.Approval{}
	}
	resolution := func(obs contracts.Observation, rid string) *contracts.ApprovalResolution {
		for _, x := range obs.Approvals {
			if x.RequestID == rid {
				return x.Resolution
			}
		}
		return nil
	}
	has := func(recs []hookRecord, event, tool string) bool {
		for _, r := range recs {
			if r.Event == event && (tool == "" || r.ToolName == tool) {
				return true
			}
		}
		return false
	}

	// 2. Deny through Pyre's broker: classified effective; nothing runs.
	target := filepath.Join(cwd, "pyre-se-denied.txt")
	_ = os.Remove(target)
	r, _ := a.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_hn1", AttemptID: "dlv_hn1",
		Text: "Use the Bash tool to run exactly: touch pyre-se-denied.txt  If it is denied, reply with the single word denied."})
	t.Logf("deliver 1: state=%s evidence=%s", r.State, r.Evidence)
	ap := pending("broker: approval pending")
	if _, err := a.Answer(ctx, h, ap, "deny", "denied by the Pyre hook-name check"); err != nil {
		t.Fatal(err)
	}
	obs := wait(120*time.Second, "broker deny effective", func(o contracts.Observation) bool {
		res := resolution(o, ap.RequestID)
		return idle(o) && res != nil && res.Effect == "effective"
	})
	if res := resolution(obs, ap.RequestID); res.Source != "pyre" || res.OptionID != "deny" {
		t.Fatalf("broker deny resolution %+v", res)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("broker-denied command ran")
	}
	newEvents()

	// 3. Deny in the TUI (Escape on the permission dialog): resolved natively.
	r, _ = a.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_hn2", AttemptID: "dlv_hn2",
		Text: "Use the Bash tool to run exactly: touch pyre-se-denied.txt  If it is denied, reply with the single word denied."})
	t.Logf("deliver 2: state=%s", r.State)
	ap = pending("tui: approval pending")
	time.Sleep(2 * time.Second) // let the TUI dialog render
	if err := a.srv.SendKeys(ctx, h.TmuxTarget, tmux.KeyEscape); err != nil {
		t.Fatal(err)
	}
	obs = wait(60*time.Second, "tui deny resolved", func(o contracts.Observation) bool {
		res := resolution(o, ap.RequestID)
		return idle(o) && res != nil
	})
	t.Logf("tui deny resolution: %+v", *resolution(obs, ap.RequestID))
	if res := resolution(obs, ap.RequestID); res.Source != "native" {
		t.Fatalf("TUI deny must be a native resolution: %+v", res)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("TUI-denied command ran")
	}
	tuiEvents := newEvents()
	t.Logf("TUI deny fired PostToolUseFailure=%v PermissionDenied=%v", has(tuiEvents, "PostToolUseFailure", "Bash"), has(tuiEvents, "PermissionDenied", ""))

	// 4. Allowed but failing command: PostToolUseFailure fires and the allow
	// is effective (the tool ran).
	r, _ = a.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_hn3", AttemptID: "dlv_hn3",
		Text: "Use the Bash tool to run exactly: touch pyre-se-missing-dir/x.txt  Then reply with the single word done."})
	t.Logf("deliver 3: state=%s", r.State)
	ap = pending("failing tool: approval pending")
	if _, err := a.Answer(ctx, h, ap, "allow", ""); err != nil {
		t.Fatal(err)
	}
	obs = wait(120*time.Second, "allow effective despite failure", func(o contracts.Observation) bool {
		res := resolution(o, ap.RequestID)
		return idle(o) && res != nil && res.Effect == "effective"
	})
	t.Logf("failing-tool allow resolution: %+v", *resolution(obs, ap.RequestID))
	failEvents := newEvents()
	if !has(failEvents, "PostToolUseFailure", "Bash") {
		t.Fatal("expected PostToolUseFailure for the failing Bash call")
	}
	for _, rec := range failEvents {
		if rec.Event == "PostToolUseFailure" {
			t.Logf("PostToolUseFailure: tool_use_id=%s payload keys=%v", rec.ToolUseID, payloadKeys(rec))
		}
	}
	rc, _ := a.Stop(ctx, h)
	t.Logf("stop: outcome=%s", rc.Outcome)
}

func payloadKeys(r hookRecord) []string {
	var ks []string
	for k := range r.payload() {
		ks = append(ks, k)
	}
	return ks
}
