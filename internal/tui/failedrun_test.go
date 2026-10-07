package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"

	"github.com/jhh3/pyre/internal/contracts"
)

// A run whose launch failed has nothing on its machine to preview; the
// preview used to read "preview unavailable: not_found: no run of this
// session on this node". It now says why the run failed.
func TestFailedLaunchPreviewShowsReason(t *testing.T) {
	m := testModel(colorprofile.NoTTY)
	m.loads["accounts"] = &loadState{have: true, val: []contracts.AccountView{{Account: contracts.Account{AccountID: "acct_01SYNTHX", Label: "synth-x", Provider: contracts.ProviderOpenAI}}}}
	sv := contracts.SessionView{Session: contracts.Session{SessionID: "ses_synth-fail", Title: "synthetic", Harness: contracts.HarnessCodex}, NodeName: "synth-box",
		Run: &contracts.Run{RunID: "run_synth-fail", State: contracts.RunFailed, Capabilities: []string{contracts.CapTranscript},
			StateReason: "launch failed: no eligible openai subscription on this node: acct_01SYNTHX: verified at checkout: availability unknown: account/rateLimits/read failed: wham/usage failed: 401 Unauthorized"}}
	m.loads["preview:ses_synth-fail"] = &loadState{err: contracts.Errorf(contracts.ErrNotFound, "no run of this session on this node")}
	want := "✗ Couldn't start: synth-x's Codex login on synth-box needs re-auth (401). Fix: pyre accounts login --node synth-box"
	for name, out := range map[string]string{
		"activity": strings.Join(m.activityLines(sv, 200, 20), "\n"),
		"embed":    strings.Join(m.snapshotLoading(sv, 200), "\n"),
	} {
		if !strings.Contains(out, want) || strings.Contains(out, "preview unavailable") {
			t.Errorf("%s:\n%s", name, out)
		}
	}
	// A running session's read error is still shown as such.
	sv.Run.State = contracts.RunRunning
	if out := strings.Join(m.activityLines(sv, 200, 20), "\n"); !strings.Contains(out, "preview unavailable") {
		t.Errorf("running:\n%s", out)
	}
}
