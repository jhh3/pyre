package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// TestParseHookReviewScreen0167 reads the screen captured from Codex 0.167
// (iss_…): same layout as 0.166.
func TestParseHookReviewScreen0167(t *testing.T) {
	b, err := os.ReadFile("testdata/codex-0.167-hooks-review.txt")
	if err != nil {
		t.Fatal(err)
	}
	s, ok := parseHookReviewScreen(string(b))
	if !ok {
		t.Fatal("captured 0.167 screen not recognized")
	}
	if s.Detail != "2 hooks are new or changed." || s.Selected != 0 || s.row("trust all") != 1 || s.row("continue without trusting") != 2 {
		t.Fatalf("parsed %+v", s)
	}
}

// TestParseHookReviewScreen reads the screen captured from Codex 0.166 on
// server (iss_…).
func TestParseHookReviewScreen(t *testing.T) {
	b, err := os.ReadFile("testdata/codex-0.166-hooks-review.txt")
	if err != nil {
		t.Fatal(err)
	}
	s, ok := parseHookReviewScreen(string(b))
	if !ok {
		t.Fatal("captured screen not recognized")
	}
	if s.Detail != "1 hook is new or changed." || s.Selected != 0 || len(s.Rows) != 3 {
		t.Fatalf("parsed %+v", s)
	}
	if s.row("trust all") != 1 || s.row("continue without trusting") != 2 {
		t.Fatalf("rows %q", s.Rows)
	}
	moved := strings.Replace(strings.Replace(string(b), "› 1.", "  1.", 1), "  3.", "› 3.", 1)
	if s, _ := parseHookReviewScreen(moved); s.Selected != 2 {
		t.Fatalf("selection after moving: %+v", s)
	}
	for _, other := range []string{
		">_ OpenAI Codex (v0.166.0)\n› Ask Codex to do anything\n",
		"  Folder access\n  /tmp/x\n  Trust this folder?\n› 1. Trust and continue\n",
		"the agent wrote: Hooks need review is a Codex screen", // prose, no options
	} {
		if _, ok := parseHookReviewScreen(other); ok {
			t.Fatalf("recognized %q", other)
		}
	}
}

func TestUntrustedHooksNamesWhatStaysUntrusted(t *testing.T) {
	hooks := []hookMetadata{
		{Key: "acme@acme-plugins:hooks/hooks.json:session_start:0:0", PluginID: "acme@acme-plugins", EventName: "sessionStart", TrustStatus: "modified"},
		{Key: "/w/.codex/hooks.json:stop:0:0", SourcePath: "/w/.codex/hooks.json", EventName: "stop", TrustStatus: "untrusted"},
		{Key: "mirrored", TrustStatus: "untrusted"},
		{Key: "ok", TrustStatus: "trusted"},
	}
	got := untrustedHooks(hooks, map[string]hookState{"mirrored": {}})
	want := []string{"acme@acme-plugins sessionStart (changed)", "/w/.codex/hooks.json stop (new)"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q", got)
	}
}

// TestCodexHookReviewScreenNeedsOwner: while the TUI shows "Hooks need
// review" the run is waiting_approval with a pending approval (never idle,
// so no "finished" report goes out), and answering it selects the row in
// the terminal and closes the screen.
func TestCodexHookReviewScreenNeedsOwner(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *Options) { o.DisableTUI = false })
	ctx := context.Background()
	hooks, _ := json.Marshal([]map[string]any{{"key": "synthetic@market:hooks/hooks.json:session_start:0:0", "pluginId": "synthetic@market",
		"eventName": "sessionStart", "currentHash": "sha256:new", "trustStatus": "modified"}})
	if err := os.WriteFile(filepath.Join(e.home, "fake-hooks.json"), hooks, 0o600); err != nil {
		t.Fatal(err)
	}
	h := e.launch(e.a, "run_hr", e.home, nil, map[string]string{"PYRE_TEST_ROLE": "faketui", "PYRE_FAKE_HOOK_REVIEW": "1"})
	review := func(o contracts.Observation) *contracts.Approval {
		for i := range o.Approvals {
			if strings.HasPrefix(o.Approvals[i].RequestID, hookReviewRequestPrefix) {
				return &o.Approvals[i]
			}
		}
		return nil
	}
	obs := e.wait(e.a, h, 10*time.Second, func(o contracts.Observation) bool { return review(o) != nil })
	ap := review(obs)
	if obs.State != contracts.RunWaitingApproval || !strings.Contains(obs.StateReason, "Codex hook review (1 hook is new or changed)") {
		t.Fatalf("state %s %q", obs.State, obs.StateReason)
	}
	if ap.State != contracts.ApprovalPending || len(ap.Options) != 2 || !strings.Contains(ap.Body, "synthetic@market sessionStart (changed)") {
		t.Fatalf("approval %+v", ap)
	}
	if _, err := e.a.Answer(ctx, h, *ap, "bogus", ""); err == nil {
		t.Fatal("unknown option accepted")
	}
	rc, err := e.a.Answer(ctx, h, *ap, "continue", "")
	if err != nil || rc.Outcome != contracts.OutcomeSucceeded {
		s, _ := e.a.srv.CapturePane(ctx, h.RunID, tmux.CaptureOptions{})
		t.Fatalf("answer: %+v %v\n%s", rc, err, s)
	}
	if s, _ := e.a.srv.CapturePane(ctx, h.RunID, tmux.CaptureOptions{}); !strings.Contains(s, "FAKE HOOKS ANSWERED: Continue without trusting") {
		t.Fatalf("wrong row answered:\n%s", s)
	}
	obs, err = e.a.Observe(ctx, h, "")
	if err != nil {
		t.Fatal(err)
	}
	ap = review(obs)
	if ap == nil || ap.State != contracts.ApprovalResolved || ap.Resolution.OptionID != "continue" || ap.Resolution.Source != "pyre" {
		t.Fatalf("resolution not reported: %+v", obs.Approvals)
	}
	if obs.State == contracts.RunWaitingApproval {
		t.Fatalf("still waiting on the closed screen: %q", obs.StateReason)
	}
	if _, err := e.a.Answer(ctx, h, *ap, "continue", ""); err == nil {
		t.Fatal("answered a closed screen twice")
	}
}
