package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"

	"github.com/jhh3/pyre/internal/contracts"
)

// W-resilience: X on a run Pyre cannot stop (interrupted launch, no handle)
// never dead-ends: it confirms "archive anyway". A never-started run is
// ended (failed) and archives at once; R offers a relaunch.
func TestArchiveUnstoppableAndRelaunch(t *testing.T) {
	m := testModel(colorprofile.NoTTY)
	unknown := contracts.SessionView{Session: contracts.Session{SessionID: "ses_synth-u", Title: "synthetic u", Harness: contracts.HarnessClaude}, NodeName: "synth-box",
		Run: &contracts.Run{RunID: "run_synth-u", State: contracts.RunUnknown, StateReason: "pyred stopped during launch before a run handle was recorded; …",
			AdapterData: map[string]string{adapterDataNoHandle: "true"}}}
	m.archiveSession(unknown)
	c, ok := m.overlay.(*confirmOverlay)
	if !ok || c.title != "Archive anyway" || !strings.Contains(strings.Join(c.body, "\n"), "may keep running") {
		t.Fatalf("overlay = %#v", m.overlay)
	}
	m.overlay = nil

	never := contracts.SessionView{Session: contracts.Session{SessionID: "ses_synth-n", Title: "synthetic n", Harness: contracts.HarnessCodex}, NodeName: "synth-box",
		Run: &contracts.Run{RunID: "run_synth-n", State: contracts.RunFailed, StateReason: "launch interrupted (pyred restarted before the harness started); nothing ran — relaunch to retry",
			AdapterData: map[string]string{adapterDataNeverStarted: "true"}}}
	if !sessionEnded(never) || !runNeverStarted(never) || runNoHandle(never) {
		t.Fatal("never-started classification")
	}
	m.confirmRelaunch(never)
	if c, ok := m.overlay.(*confirmOverlay); !ok || c.title != "Relaunch session" || c.sid != "ses_synth-n" {
		t.Fatalf("relaunch overlay = %#v", m.overlay)
	}
}
