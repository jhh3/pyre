package claude

import (
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// iss_… (rc120): two server sessions read
// waiting_approval "needs human: ● implementer(Triage + lower 3 browser
// rows)" and "needs human: ✻ Waiting for 1 background agent to finish".
// The screens (testdata, synthetic text in the real shape of Claude Code
// 2.1.304 at 23:30 and 23:34 UTC on 2026-10-24) carry Claude's usage-limit
// menu at the bottom, shown after the turn's StopFailure ("You've hit your
// individual spend limit"); the reason named the pane's top row, an agent
// row or a spinner line, as the dialog's title.
func TestParseScreenLimitMenuUnderAgentRows(t *testing.T) {
	for _, name := range []string{"screen-limit-menu-agent-row-2.1.304.txt", "screen-limit-menu-waiting-agents-2.1.304.txt"} {
		si := parseScreen(readScreen(t, name), "")
		if si.HasInputBox || si.DialogKind != "limit" || si.Dialog != limitMenuDialog {
			t.Fatalf("%s: %+v", name, si)
		}
	}
}

// The same agent rows and spinner lines above a live input box are the
// session working on its own subagents: no dialog.
func TestParseScreenAgentRowsAreNotADialog(t *testing.T) {
	scr := readScreen(t, "screen-limit-menu-agent-row-2.1.304.txt")
	top := scr[:strings.Index(scr, "▔")]
	si := parseScreen(top+realIdle, "")
	if !si.HasInputBox || si.Dialog != "" {
		t.Fatalf("agent rows over the input box: %+v", si)
	}
	// Without the input box drawn yet (a redraw) they are not a dialog either.
	if si := parseScreen(top, ""); si.Dialog != "" {
		t.Fatalf("agent rows alone: %+v", si)
	}
}

// A dialog Pyre does not know is named by its own title (below the rule
// Claude draws above it), never by the transcript rows above it.
func TestDialogTitleIsTheDialogs(t *testing.T) {
	rows := "✻ Waiting for 1 background agent to finish\n\n● implementer(Speed up synthetic lowering)\n  ⎿  Backgrounded agent (↓ to manage · ctrl+o to expand)\n\n"
	for _, rule := range []string{strings.Repeat("▔", 60), strings.Repeat("─", 60)} {
		si := parseScreen(rows+rule+"\n Pick a color\n\n ❯ 1. Red\n   2. Blue\n\n Enter to confirm · Esc to cancel", "")
		if si.DialogKind != "unknown" || si.Dialog != "Pick a color" {
			t.Fatalf("rule %q: %+v", rule[:3], si)
		}
	}
	// No rule: the line above the options.
	si := parseScreen(rows+" Pick a color\n ❯ 1. Red\n   2. Blue\n Enter to confirm", "")
	if si.DialogKind != "unknown" || si.Dialog != "Pick a color" {
		t.Fatalf("no rule: %+v", si)
	}
}

// Claude's limit menu variants (2.1.304 binary): "Stop" alone, the
// auto-resume option and the extra-usage options.
func TestParseScreenLimitMenuVariants(t *testing.T) {
	rule := strings.Repeat("▔", 60)
	for _, opts := range [][]string{
		{"Stop", "Wait here, then continue automatically when the limit resets"},
		{"Stop and wait for limit to reset", "Don’t continue automatically"},
		{"Ask your admin for more usage", "Stop and wait for limit to reset"},
		{"Switch to usage credits", "Stop and wait for limit to reset", "Upgrade your plan"},
	} {
		var b strings.Builder
		b.WriteString("● Done.\n" + rule + "\n   What do you want to do?\n\n")
		for i, o := range opts {
			if i == 0 {
				b.WriteString("   ❯ 1. " + o + "\n")
			} else {
				b.WriteString("     " + string(rune('1'+i)) + ". " + o + "\n")
			}
		}
		b.WriteString("\n   Enter to confirm · Esc to cancel")
		if si := parseScreen(b.String(), ""); si.DialogKind != "limit" {
			t.Fatalf("%v: %+v", opts, si)
		}
	}
	// "What do you want to do?" over other options is not the limit menu.
	si := parseScreen(rule+"\n   What do you want to do?\n\n   ❯ 1. Keep editing\n     2. Discard\n\n   Enter to confirm · Esc to cancel", "")
	if si.DialogKind != "unknown" || si.Dialog != "What do you want to do?" {
		t.Fatalf("other menu: %+v", si)
	}
}

// limitMenuShown waits for the fake's usage-limit menu after a FAKE_LIMIT
// turn.
func limitMenuShown(t *testing.T, f *fakeEnv, h contracts.RunHandle) {
	t.Helper()
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	deliver(t, f.a, h, "msg_lm0", "FAKE_LIMIT", contracts.ModeQueue)
	f.waitObs(f.a, h, 15*time.Second, func(o contracts.Observation) bool {
		return o.LimitEvent != nil && screenShows(t, f, h, "Stop and wait for limit to reset")
	})
}

// The usage-limit menu over a session's agent rows reads as the failed
// turn it is (waiting_input, the limit reported), never as "needs human"
// (iss_…: repeated "needs input" reports to the
// parent). A message closes the menu and is delivered.
func TestFakeLimitMenuIsNotNeedsHuman(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	h := f.launch("run_lm1", map[string]string{"FAKE_LIMIT_MENU": "1"}, nil)
	limitMenuShown(t, f, h)
	end := time.Now().Add(2 * time.Second)
	for time.Now().Before(end) {
		obs, _ := f.a.Observe(t.Context(), h, "")
		if obs.State != contracts.RunWaitingInput || strings.Contains(obs.StateReason, "needs human") || !strings.Contains(obs.StateReason, limitMenuReason) {
			t.Fatalf("limit menu: %s (%s)", obs.State, obs.StateReason)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if ok, why := f.a.InputReady(t.Context(), h, contracts.ModeQueue); !ok {
		t.Fatalf("not ready under the limit menu: %s", why)
	}
	r := deliver(t, f.a, h, "msg_lm1", "reply ok", contracts.ModeQueue)
	if r.State != contracts.DeliveryAccepted {
		t.Fatalf("deliver under the limit menu: %+v", r)
	}
	if screenShows(t, f, h, "Stop and wait for limit to reset") {
		t.Fatal("menu still shown")
	}
}

// An account switch's stop closes the menu and exits with /exit (not
// kill-session).
func TestFakeStopClosesLimitMenu(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	h := f.launch("run_lm2", map[string]string{"FAKE_LIMIT_MENU": "1"}, nil)
	limitMenuShown(t, f, h)
	rc, err := f.a.Stop(t.Context(), h)
	if err != nil || rc.Outcome != contracts.OutcomeSucceeded || !strings.Contains(rc.Detail, "/exit") {
		t.Fatalf("stop %+v %v", rc, err)
	}
}
