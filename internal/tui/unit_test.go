package tui

import (
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestListStateKeepsSelectionByID(t *testing.T) {
	var l listState
	l.setIDs([]string{"a", "b", "c", "d"})
	l.move(2)
	if l.sel != "c" {
		t.Fatalf("sel = %s", l.sel)
	}
	// Insertions and reordering keep the same item selected.
	l.setIDs([]string{"x", "y", "d", "c", "a", "b"})
	if l.sel != "c" || l.idx != 3 {
		t.Fatalf("after reorder sel=%s idx=%d", l.sel, l.idx)
	}
	// Removal selects the neighbour at the same position.
	l.setIDs([]string{"x", "y", "d", "a", "b"})
	if l.sel != "a" || l.idx != 3 {
		t.Fatalf("after removal sel=%s idx=%d", l.sel, l.idx)
	}
	// Shrinking clamps.
	l.setIDs([]string{"x"})
	if l.sel != "x" || l.idx != 0 {
		t.Fatalf("after shrink sel=%s idx=%d", l.sel, l.idx)
	}
	l.setIDs(nil)
	if l.sel != "" {
		t.Fatalf("empty sel = %q", l.sel)
	}
}

func TestListWindowKeepsCursorVisible(t *testing.T) {
	var l listState
	ids := make([]string, 50)
	for i := range ids {
		ids[i] = itoa(i)
	}
	l.setIDs(ids)
	l.move(30)
	s, e := l.window(10)
	if l.idx < s || l.idx >= e || e-s != 10 {
		t.Fatalf("window [%d,%d) idx %d", s, e, l.idx)
	}
	l.move(-30)
	s, _ = l.window(10)
	if s != 0 {
		t.Fatalf("top = %d", s)
	}
}

func TestAge(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	cases := map[time.Duration]string{0: "now", 4 * time.Second: "4s", 7 * time.Minute: "7m", 3 * time.Hour: "3h", 72 * time.Hour: "3d"}
	for d, want := range cases {
		if got := age(now, now.Add(-d)); got != want {
			t.Errorf("age(%s) = %s want %s", d, got, want)
		}
	}
	if age(now, time.Time{}) != "—" {
		t.Error("zero time")
	}
	if got := until(now, now.Add(90*time.Minute)); got != "in 1h" {
		t.Errorf("until = %s", got)
	}
}

func TestPadTruncWideRunes(t *testing.T) {
	if got := pad("界界界", 4); width(got) != 4 {
		t.Fatalf("pad width = %d (%q)", width(got), got)
	}
	if got := trunc("a very long session name", 8); width(got) != 8 || got[len(got)-3:] != "…" {
		t.Fatalf("trunc = %q", got)
	}
}

func TestMatchQuery(t *testing.T) {
	if !matchQuery("api LINUX", "outfit API client", "synth-linux") {
		t.Error("multi-term match")
	}
	if matchQuery("mac", "outfit", "synth-linux") {
		t.Error("false match")
	}
}

func TestSessionStatusNeverInfersFromLostContact(t *testing.T) {
	run := &contracts.Run{State: contracts.RunRunning}
	sv := contracts.SessionView{Run: run}
	off := &contracts.Node{Connection: contracts.NodeConnection{State: contracts.ConnDisconnected}}
	if st := sessionStatus(sv, off); st.label != "offline" || st.sym == "" {
		t.Fatalf("offline: %+v", st)
	}
	run.Freshness.Stale = true
	if st := sessionStatus(sv, nil); st.label != "stale" {
		t.Fatalf("stale: %+v", st)
	}
	run.State = contracts.RunUnknown
	run.Freshness.Stale = false
	if st := sessionStatus(sv, nil); st.label != "unknown" {
		t.Fatalf("unknown: %+v", st)
	}
	// Every state has a distinct symbol+label pair.
	seen := map[string]contracts.RunState{}
	for _, s := range []contracts.RunState{contracts.RunStarting, contracts.RunRunning, contracts.RunWaitingInput, contracts.RunWaitingApproval,
		contracts.RunWaitingBackground, contracts.RunExiting, contracts.RunExited, contracts.RunFailed, contracts.RunUnknown} {
		k := runStatus(s).String()
		if prev, dup := seen[k]; dup {
			t.Fatalf("%s and %s render the same (%q)", prev, s, k)
		}
		seen[k] = s
	}
}

func TestInvalidationKeys(t *testing.T) {
	pid := "prj_x"
	ev := contracts.Event{Kind: contracts.EvArtifactPublished, ProjectID: &pid}
	got := invalidationKeys(ev)
	want := map[string]bool{"art:": true, "artifact:": true, "proj:prj_x:arts": true}
	for _, k := range got {
		delete(want, k)
	}
	if len(want) != 0 {
		t.Fatalf("missing %v in %v", want, got)
	}
	if ks := invalidationKeys(contracts.Event{Kind: contracts.EvRunObserved, SessionID: "ses_a"}); len(ks) != 2 || ks[1] != "preview:ses_a" {
		t.Fatalf("run keys = %v", ks)
	}
}

func TestKVLabelsNeverRunIntoValues(t *testing.T) {
	m := New(t.Context(), nil, Options{NoColor: true, KeysFile: "none"})
	defer m.Close()
	plain := func(ss []string) []string {
		for i := range ss {
			ss[i] = ansi.Strip(ss[i])
		}
		return ss
	}
	if got := ansi.Strip(m.kvl("Unavailable", "attach", 11)); got != "Unavailable attach" {
		t.Fatalf("kvl = %q", got)
	}
	// Room beside the label: wrapped with a hanging indent.
	got := plain(m.kvWrap("Unavailable", "attach, interrupt, stop, steer — no live run", m.th.dim.Render, 12, 40))
	if len(got) != 2 || got[0] != "Unavailable attach, interrupt, stop," || got[1] != "            steer — no live run" {
		t.Fatalf("kvWrap wide = %q", got)
	}
	// A narrow pane puts the value under the label.
	got = plain(m.kvWrap("Unavailable", "attach, interrupt — no live run", m.th.dim.Render, 12, 30))
	if len(got) < 2 || got[0] != "Unavailable" || got[1] != "  attach, interrupt — no live" {
		t.Fatalf("kvWrap narrow = %q", got)
	}
	if got := plain(m.kvFit("Directory", "~/work/a/very/long/path/that/goes/on", 12, 30)); len(got) != 2 || got[0] != "Directory" {
		t.Fatalf("kvFit narrow = %q", got)
	}
}

func TestFailureLineIsPlain(t *testing.T) {
	m := New(t.Context(), nil, Options{NoColor: true, KeysFile: "none"})
	defer m.Close()
	now := time.Now()
	code := 3
	cases := []struct {
		r    contracts.Run
		want string
	}{
		{contracts.Run{StateReason: "launch failed: no eligible account"}, "Could not start: no eligible account."},
		{contracts.Run{ExitCode: &code}, "Failed: the run exited with code 3."},
		{contracts.Run{StateReason: "harness crashed", ExitCode: &code}, "Failed: harness crashed (exit code 3)."},
		{contracts.Run{}, "Failed; no reason was reported."},
	}
	for _, c := range cases {
		if got := m.failureLine(&c.r, now); got != c.want {
			t.Errorf("failureLine(%+v) = %q, want %q", c.r, got, c.want)
		}
	}
}
