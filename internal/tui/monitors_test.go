package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"

	"github.com/jhh3/pyre/internal/contracts"
)

func synthMonitor(id, target string, st contracts.RunState) contracts.SessionView {
	return contracts.SessionView{Session: contracts.Session{SessionID: id, Title: "monitor: gh pr checks " + id, Harness: contracts.HarnessShell,
		Tags: []string{contracts.TagMonitor, contracts.TagMonitorToPrefix + target}}, Run: &contracts.Run{RunID: "run_" + id, State: st}}
}

// Monitors are not rows; the session they wake shows "watching N" and
// lists them in its facts, and a monitor's patch updates that badge.
func TestMonitorsAreABadgeOnTheirTarget(t *testing.T) {
	m := testModel(colorprofile.NoTTY)
	parent := contracts.SessionView{Session: contracts.Session{SessionID: "ses_synth-parent", Title: "synthetic lead", Harness: contracts.HarnessCodex},
		Run:      &contracts.Run{RunID: "run_synth-parent", State: contracts.RunWaitingInput},
		Monitors: []contracts.MonitorRef{{SessionID: "ses_synth-m1", Title: "monitor: gh pr checks 7", State: contracts.RunRunning}}}
	legacy := synthMonitor("ses_synth-m0", "ses_synth-parent", contracts.RunRunning)
	ss := withoutMonitors([]contracts.SessionView{parent, legacy})
	if len(ss) != 1 || ss[0].Session.SessionID != "ses_synth-parent" {
		t.Fatalf("withoutMonitors: %+v", ss)
	}
	m.loads["sessions"] = &loadState{have: true, val: ss}

	if row := m.titleDoingCell(false, parent, 60); !strings.Contains(row, "synthetic lead") || !strings.Contains(row, "watching 1") {
		t.Fatalf("row: %q", row)
	}
	if facts := strings.Join(m.sessionFacts(parent, 100), "\n"); !strings.Contains(facts, "Watching") || !strings.Contains(facts, "gh pr checks 7") {
		t.Fatalf("facts:\n%s", facts)
	}

	// A new monitor's event adds to the badge, not to the list.
	m.applySessionPatch(contracts.GetManySessionsResponse{Items: []contracts.SessionView{synthMonitor("ses_synth-m2", "ses_synth-parent", contracts.RunRunning)}})
	got := m.sessions()
	if len(got) != 1 || len(got[0].Monitors) != 2 || got[0].Monitors[1].SessionID != "ses_synth-m2" {
		t.Fatalf("after a new monitor: %+v", got)
	}
	// An ended one leaves it.
	m.applySessionPatch(contracts.GetManySessionsResponse{Items: []contracts.SessionView{synthMonitor("ses_synth-m1", "ses_synth-parent", contracts.RunExited)}})
	got = m.sessions()
	if len(got) != 1 || len(got[0].Monitors) != 1 || got[0].Monitors[0].SessionID != "ses_synth-m2" {
		t.Fatalf("after a monitor ended: %+v", got)
	}
	if row := m.titleDoingCell(false, got[0], 60); !strings.Contains(row, "watching 1") {
		t.Fatalf("row: %q", row)
	}
	// The original view's badge was not mutated in place.
	if len(parent.Monitors) != 1 || parent.Monitors[0].SessionID != "ses_synth-m1" {
		t.Fatalf("patched in place: %+v", parent.Monitors)
	}
}
