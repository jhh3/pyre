package claude

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// screenDangerRmSubagent is a synthetic pane capture of Claude Code
// 2.1.304's dangerous-rm safety prompt raised by an in-session subagent in
// bypass mode (iss_…): no PermissionRequest hook
// fires for it, only the dialog and a permission_prompt notification.
var screenDangerRmSubagent = strings.Join([]string{
	"● implementer(Clean up the synthetic run directories)",
	"  ⎿  Bash(ls runs/)",
	"     Waiting…",
	"",
	"✻ Waiting for 1 background agent to finish",
	"",
	strings.Repeat("─", 100),
	" Bash command · from the implementer agent",
	"",
	`   rm -rf "$(readlink runs/v1-synthetic/tmp)" runs/v1-synthetic`,
	"   Remove the synthetic run directory",
	"",
	" Dangerous rm operation on statically-unresolvable target: command substitution output",
	"",
	" Do you want to proceed?",
	" ❯ 1. Yes",
	"   2. No",
	"",
	" Esc to cancel · Tab to amend · ctrl+e to explain",
}, "\n")

func TestParseTUIPromptDangerRm(t *testing.T) {
	si := parseScreen(screenDangerRmSubagent, "")
	if si.DialogKind != "permission" || si.Prompt == nil {
		t.Fatalf("not a permission prompt: %+v", si)
	}
	p := si.Prompt
	if p.Title != "Bash command · from the implementer agent" || p.Question != "Do you want to proceed?" || p.Selected != 1 {
		t.Fatalf("prompt %+v", p)
	}
	if !strings.Contains(p.Body, "readlink runs/v1-synthetic/tmp") || !strings.Contains(p.Body, "Dangerous rm operation") {
		t.Fatalf("body %q", p.Body)
	}
	if len(p.Options) != 2 || p.Options[0] != (tuiOption{1, "Yes"}) || p.Options[1] != (tuiOption{2, "No"}) {
		t.Fatalf("options %+v", p.Options)
	}
	// The selection moving keeps the prompt's identity.
	moved := strings.Replace(strings.Replace(screenDangerRmSubagent, " ❯ 1. Yes", "   1. Yes", 1), "   2. No", " ❯ 2. No", 1)
	if q := parseScreen(moved, "").Prompt; q == nil || q.Selected != 2 || q.key() != p.key() {
		t.Fatalf("moved %+v", q)
	}
	// The three-option edit prompt gets allow / allow_always / deny.
	tr := newTUITrack(parseScreen(strings.Repeat("─", 40)+"\n"+realPermission, "").Prompt, time.Now())
	if tr.OptionIDs["allow"] != 1 || tr.OptionIDs["allow_always"] != 2 || tr.OptionIDs["deny"] != 3 {
		t.Fatalf("option ids %+v", tr.OptionIDs)
	}
	// An idle screen whose scrollback shows an old question is no prompt.
	rule := strings.Repeat("─", 40)
	if si := parseScreen(" Do you want to proceed?\n ❯ 1. Yes\n   2. No\n"+rule+"\n❯ \n"+rule+"\n", ""); si.Prompt != nil {
		t.Fatalf("idle screen read as a prompt: %+v", si.Prompt)
	}
}

// A permission prompt that no broker request covers becomes a pending
// approval after the grace and resolves once the screen stops showing it.
func TestObserveTUIPromptApproval(t *testing.T) {
	a := &Adapter{o: Options{Now: time.Now}}
	h := contracts.RunHandle{RunID: "run_tui", SessionID: "ses_tui", StateDir: t.TempDir()}
	rule := strings.Repeat("─", 40)
	dialog := parseScreen(screenDangerRmSubagent, "")
	working := parseScreen("● Removing…\n✻ Removing… (esc to interrupt)\n"+rule+"\n❯ \n"+rule+"\n", "")
	t0 := time.Now().Add(-time.Minute)
	rs := newRunState()
	rs.applyHook(hookRecord{Event: "SessionStart", At: t0})
	rs.applyHook(hookRecord{Event: "UserPromptSubmit", At: t0})
	rs.applyHook(hookRecord{Event: "Notification", At: t0, Payload: []byte(`{"notification_type":"permission_prompt"}`)})
	observe := func(si screenInfo, at time.Time) ([]contracts.Approval, contracts.RunState, string) {
		s, r := rs.derive()
		return a.observeApprovals(h, rs, s, r, &si, at)
	}
	now := time.Now()
	if aps, st, _ := observe(dialog, now); len(aps) != 0 || st != contracts.RunWaitingApproval {
		t.Fatalf("within grace: %+v %s", aps, st)
	}
	aps, st, reason := observe(dialog, now.Add(tuiPromptGrace))
	if len(aps) != 1 || st != contracts.RunWaitingApproval || !strings.Contains(reason, "pyre approve") {
		t.Fatalf("after grace: %+v %s %q", aps, st, reason)
	}
	ap := aps[0]
	if ap.State != contracts.ApprovalPending || !isTUIRequest(ap.RequestID) || ap.ApprovalID != approvalID(h.RunID, ap.RequestID) ||
		!strings.Contains(ap.Title, "from the implementer agent") || !strings.Contains(ap.Body, "Dangerous rm") ||
		len(ap.Options) != 2 || ap.Options[0].ID != "allow" || ap.Options[1].ID != "deny" {
		t.Fatalf("approval %+v", ap)
	}
	// Redrawn: the same request.
	if aps, _, _ := observe(dialog, now.Add(2*tuiPromptGrace)); len(aps) != 1 || aps[0].RequestID != ap.RequestID {
		t.Fatalf("redraw: %+v", aps)
	}
	// Answered in the terminal: resolved natively, the run works.
	aps, st, _ = observe(working, now.Add(3*tuiPromptGrace))
	if len(aps) != 1 || aps[0].State != contracts.ApprovalResolved || aps[0].Resolution.Source != "native" || st != contracts.RunRunning {
		t.Fatalf("answered: %+v %s", aps, st)
	}
	// The same prompt shown again is a new request.
	observe(dialog, now.Add(4*tuiPromptGrace))
	aps, _, _ = observe(dialog, now.Add(6*tuiPromptGrace))
	if len(aps) != 2 || aps[1].RequestID == ap.RequestID || aps[1].State != contracts.ApprovalPending {
		t.Fatalf("second prompt: %+v", aps)
	}
}

// A dialog the broker's PermissionRequest covers stays the broker's
// approval; no terminal approval duplicates it.
func TestObserveTUIPromptCoveredByBroker(t *testing.T) {
	a := &Adapter{o: Options{Now: time.Now}}
	h := contracts.RunHandle{RunID: "run_b", SessionID: "ses_b", StateDir: t.TempDir()}
	t0 := time.Now().Add(-time.Minute)
	rs := newRunState()
	rs.applyHook(hookRecord{Event: "SessionStart", At: t0})
	rs.applyHook(hookRecord{Event: "UserPromptSubmit", At: t0})
	rs.applyHook(hookRecord{Event: "PreToolUse", At: t0, ToolUseID: "tu1", ToolName: "Bash"})
	rs.applyHook(hookRecord{Event: "PermissionRequest", At: t0, RequestID: "r1", ToolUseID: "tu1", ToolName: "Bash", HookPID: os.Getpid()})
	si := parseScreen(strings.Repeat("─", 40)+"\n"+realPermission, "")
	now := time.Now()
	for i := 0; i < 3; i++ {
		s, r := rs.derive()
		aps, _, _ := a.observeApprovals(h, rs, s, r, &si, now.Add(time.Duration(i)*tuiPromptGrace))
		if len(aps) != 1 || aps[0].RequestID != "r1" {
			t.Fatalf("observation %d: %+v", i, aps)
		}
	}
}

// End to end against the fake Claude: the subagent's dangerous-rm prompt
// (no hook) is answered through Answer, which selects the option in the
// dialog.
func TestFakeTUIPromptAnsweredThroughPyre(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	h := f.launch("run_dangerrm", nil, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	ctx := context.Background()
	for i, tc := range []struct{ option, reply string }{{"deny", "rm-denied"}, {"allow", "rm-allowed"}} {
		deliver(t, f.a, h, "msg_rm"+tc.option, "FAKE_DANGER_RM", contracts.ModeQueue)
		obs := f.waitObs(f.a, h, 15*time.Second, func(o contracts.Observation) bool {
			return len(o.Approvals) == i+1 && o.Approvals[i].State == contracts.ApprovalPending
		})
		ap := obs.Approvals[i]
		if obs.State != contracts.RunWaitingApproval || !isTUIRequest(ap.RequestID) || !strings.Contains(ap.Body, "Dangerous rm") {
			t.Fatalf("%+v", obs)
		}
		if _, err := f.a.Answer(ctx, h, ap, "opt_9", ""); !contracts.IsCode(err, contracts.ErrInvalid) {
			t.Fatalf("unknown option: %v", err)
		}
		rc, err := f.a.Answer(ctx, h, ap, tc.option, "")
		if err != nil || !rc.Accepted {
			t.Fatalf("answer %s: %+v %v", tc.option, rc, err)
		}
		obs = f.waitObs(f.a, h, 15*time.Second, func(o contracts.Observation) bool {
			return o.State == contracts.RunWaitingInput && o.Approvals[i].State == contracts.ApprovalResolved
		})
		if res := obs.Approvals[i].Resolution; res.Source != "pyre" || res.OptionID != tc.option {
			t.Fatalf("resolution %+v", res)
		}
		if _, err := f.a.Answer(ctx, h, ap, tc.option, ""); !contracts.IsCode(err, contracts.ErrConflict) {
			t.Fatalf("late answer must conflict: %v", err)
		}
		resp, _ := f.a.ReadTranscript(ctx, h, contracts.ReadSessionRequest{Tail: true, Limit: 20})
		if !strings.Contains(resp.Screen, tc.reply) {
			t.Fatalf("%s: screen lacks %q:\n%s", tc.option, tc.reply, resp.Screen)
		}
	}
}
