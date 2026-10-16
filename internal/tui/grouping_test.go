package tui

import (
	"reflect"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestRepoFromPath(t *testing.T) {
	cases := map[string]string{
		"/home/synth/src/outfit":                              "outfit",
		"/home/synth/src/outfit/":                             "outfit",
		"/home/synth/src/outfit/internal/api":                 "api", // a subdirectory reads as itself without git info
		"/home/synth/src/outfit/.claude/worktrees/fix-login":  "outfit",
		"/home/synth/src/outfit/.claude/worktrees/x/sub/dir":  "outfit",
		"/Users/synth/code/pyre/.worktrees/w-grid":            "pyre",
		"/Users/synth/code/pyre/.git/worktrees/w-grid":        "pyre",
		"/Users/synth/code/pyre-worktrees/W-tuigroup":         "pyre",
		"/Users/synth/code/pyre-worktrees/W-tuigroup/android": "pyre",
		"/Users/synth/code/pyre-worktrees":                    "pyre-worktrees",
		"/home/synth":                                         "~",
		"/Users/synth":                                        "~",
		"/root":                                               "~",
		"~":                                                   "~",
		"~/src/hive":                                          "hive",
		`C:\Users\synth\src\rota`:                             "rota",
		"":                                                    "",
		"/":                                                   "",
		"/srv/.claude/worktrees":                              "worktrees",
	}
	for in, want := range cases {
		if got := repoFromPath(in); got != want {
			t.Errorf("repoFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSessionRepoPrefersCheckoutPath(t *testing.T) {
	s := sv("a", "server", "/tmp/elsewhere")
	s.Run.Checkout = &contracts.CheckoutBinding{Path: "/home/synth/src/hive/.claude/worktrees/b1", Mode: contracts.CheckoutWorktree}
	if got := sessionRepo(s); got != "hive" {
		t.Fatalf("sessionRepo = %q", got)
	}
	s.Run = nil
	if got := sessionRepo(s); got != "" {
		t.Fatalf("no run: sessionRepo = %q", got)
	}
}

func TestPinTags(t *testing.T) {
	tags := []string{"title:auto", "billing"}
	p := withPin(tags, true, 1700000000)
	if !reflect.DeepEqual(p, []string{"title:auto", "billing", "pinned", "pinned_at:1700000000"}) {
		t.Fatalf("pin tags %v", p)
	}
	if !tagsPinned(p) || pinnedAt(p) != 1700000000 {
		t.Fatalf("pinned %v at %d", tagsPinned(p), pinnedAt(p))
	}
	// Re-pinning replaces the time instead of adding a second one.
	if p2 := withPin(p, true, 1700000100); pinnedAt(p2) != 1700000100 || len(p2) != 4 {
		t.Fatalf("re-pin %v", p2)
	}
	u := withPin(p, false, 0)
	if !reflect.DeepEqual(u, []string{"title:auto", "billing"}) || tagsPinned(u) || pinnedAt(u) != 0 {
		t.Fatalf("unpin %v", u)
	}
	// A pinned_at without the marker is not a pin; the marker alone is.
	if tagsPinned([]string{"pinned_at:5"}) || pinnedAt([]string{"pinned_at:5"}) != 0 || !tagsPinned([]string{"pinned"}) {
		t.Fatal("marker semantics")
	}
	if got := userTags(p); !reflect.DeepEqual(got, []string{"billing"}) {
		t.Fatalf("reserved pin tags shown as user tags: %v", got)
	}
}

func sv(id, node, cwd string, tags ...string) contracts.SessionView {
	return contracts.SessionView{
		Session:  contracts.Session{SessionID: id, Title: id, Tags: tags},
		NodeName: node,
		Run:      &contracts.Run{RunID: "run_" + id, Cwd: cwd, State: contracts.RunWaitingInput},
	}
}

func ids(ss []contracts.SessionView) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Session.SessionID
	}
	return out
}

func TestSortGroupedPinnedFirstInEveryMode(t *testing.T) {
	base := []contracts.SessionView{
		sv("c", "server", "/src/pyre"),
		sv("a", "mac", "/src/zeta"),
		sv("p2", "server", "/src/zeta", "pinned", "pinned_at:200"),
		sv("b", "mac", "/src/pyre-worktrees/w1"),
		sv("p1", "mac", "/src/pyre", "pinned", "pinned_at:100"),
		sv("p0", "mac", "/src/pyre", "pinned"), // no time: after timed pins
		sv("d", "", "/src/alpha"),
	}
	want := map[groupMode][]string{
		groupMachine: {"p1", "p2", "p0", "a", "b", "c", "d"}, // machines by name, no machine last
		groupRepo:    {"p1", "p2", "p0", "d", "b", "c", "a"}, // alpha, pyre (worktree + dir), zeta
	}
	for mode, w := range want {
		m := &Model{}
		m.grouping.mode = mode
		ss := append([]contracts.SessionView(nil), base...)
		m.sortGrouped(ss)
		if got := ids(ss); !reflect.DeepEqual(got, w) {
			t.Errorf("%s: order %v, want %v", mode, got, w)
		}
	}
}

func TestSortFlatNeedsYouThenRecent(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	old := sv("old", "server", "/a")
	old.Run.StartedAt = now.Add(-time.Hour)
	recent := sv("recent", "server", "/b")
	recent.Run.StartedAt = now.Add(-time.Minute)
	needs := sv("needs", "mac", "/c")
	needs.Run.StartedAt = now.Add(-2 * time.Hour)
	needs.Attention = 1
	pin := sv("pin", "mac", "/d", "pinned", "pinned_at:1")
	m := &Model{}
	m.grouping.mode = groupNone
	ss := []contracts.SessionView{old, recent, pin, needs}
	m.sortGrouped(ss)
	if got := ids(ss); !reflect.DeepEqual(got, []string{"pin", "needs", "recent", "old"}) {
		t.Fatalf("flat order %v", got)
	}
}

func TestGroupedIDsFoldAndState(t *testing.T) {
	m := &Model{}
	m.grouping.mode = groupRepo
	vs := []contracts.SessionView{
		sv("p", "mac", "/src/zeta", "pinned"),
		sv("a", "server", "/src/hive"),
		sv("b", "mac", "/src/hive/.claude/worktrees/x"),
		sv("c", "mac", "/src/zeta"),
	}
	m.sortGrouped(vs)
	if got := m.groupedIDs(vs); !reflect.DeepEqual(got, []string{"p", "b", "a", "c"}) {
		t.Fatalf("rows %v", got)
	}
	if m.grouping.counts["hive"] != 2 || m.grouping.counts[pinnedGroup] != 1 {
		t.Fatalf("counts %v", m.grouping.counts)
	}
	m.grouping.collapsed = map[string]bool{"repo:hive": true, "machine:mac": true}
	if got := m.groupedIDs(vs); !reflect.DeepEqual(got, []string{"p", "@group:hive", "c"}) {
		t.Fatalf("folded rows %v", got)
	}
	// A filter shows every match: folds do not apply.
	m.filter = "x"
	if got := m.groupedIDs(vs); len(got) != 4 {
		t.Fatalf("filtered rows %v", got)
	}
	m.filter = ""
	// State round trip: mode and folds survive a restart.
	var st tuiState
	m.grouping.save(&st)
	if st.Group != "repo" || !reflect.DeepEqual(st.Collapsed, []string{"machine:mac", "repo:hive"}) {
		t.Fatalf("saved %+v", st)
	}
	var g groupingState
	g.load(st)
	if g.mode != groupRepo || !g.collapsed["repo:hive"] || !g.collapsed["machine:mac"] {
		t.Fatalf("loaded %+v", g)
	}
	// No choice is not written; an explicit choice is, with the version.
	var st2 tuiState
	(&groupingState{}).save(&st2)
	if st2.Group != "" || st2.GroupVersion != 0 {
		t.Fatalf("unchosen mode written: %+v", st2)
	}
	(&groupingState{mode: groupMachine}).save(&st2)
	if st2.Group != "machine" || st2.GroupVersion != groupStateVersion {
		t.Fatalf("chosen machine: %+v", st2)
	}
}

// Project is the default for anyone who never chose: a state file from
// before the version bump (machine was the unwritten default) switches to
// project; a chosen non-default mode and any versioned choice are kept.
func TestGroupStateMigration(t *testing.T) {
	cases := []struct {
		st   tuiState
		want groupMode
	}{
		{tuiState{}, groupProject},
		{tuiState{Group: "machine"}, groupProject},                  // old default, never really chosen
		{tuiState{Group: "repo"}, groupRepo},                        // old explicit choice
		{tuiState{Group: "none"}, groupNone},                        // old explicit choice
		{tuiState{Group: "machine", GroupVersion: 2}, groupMachine}, // chosen after the bump
		{tuiState{Group: "project", GroupVersion: 2}, groupProject},
		{tuiState{Group: "bogus", GroupVersion: 2}, groupProject},
	}
	for _, c := range cases {
		g := groupingState{mode: groupRepo}
		g.load(c.st)
		m := &Model{grouping: g}
		if got := m.groupMode(); got != c.want {
			t.Errorf("load %+v: mode %s, want %s", c.st, got, c.want)
		}
	}
	// The cycle starts from the default.
	if groupModes[0] != groupProject || !reflect.DeepEqual(groupModes, []groupMode{groupProject, groupAttention, groupMachine, groupRepo, groupNone}) {
		t.Fatalf("cycle %v", groupModes)
	}
}

// Project groups keep a fixed order (by name, unknown last) and rows keep
// creation order inside a project: a run start or a rename moves nothing
// (owner request 2026-10-13).
func TestSortProjectsStable(t *testing.T) {
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	mk := func(id, node, prj string, min int, tags ...string) contracts.SessionView {
		s := sv(id, node, "/src/"+id, tags...)
		if prj != "" {
			s.Session.ProjectID = &prj
		}
		s.Run.StartedAt = at.Add(time.Duration(min) * time.Minute)
		return s
	}
	ss := []contracts.SessionView{
		mk("a1", "server", "prj_a", -100), mk("loose", "mac", "", 0), mk("b1", "server", "prj_b", -10),
		mk("a2", "mac", "prj_a", -90), mk("b2", "mac", "prj_b", -200), mk("pin", "server", "prj_a", 5, "pinned", "pinned_at:1"),
	}
	m := &Model{}
	m.sortGrouped(ss)
	want := []string{"pin", "a1", "a2", "b1", "b2", "loose"}
	if got := ids(ss); !reflect.DeepEqual(got, want) {
		t.Fatalf("project order %v, want %v", got, want)
	}
	// b2 starts a new run and a1 is renamed: nothing moves.
	for i := range ss {
		switch ss[i].Session.SessionID {
		case "b2":
			ss[i].Run.StartedAt = at.Add(time.Hour)
		case "a1":
			ss[i].Session.Title = "zzz renamed"
		}
	}
	m.sortGrouped(ss)
	if got := ids(ss); !reflect.DeepEqual(got, want) {
		t.Fatalf("order changed after a run start / rename: %v, want %v", got, want)
	}
	if m.groupLabel(noGroup) != "No project" {
		t.Fatalf("no-project label %q", m.groupLabel(noGroup))
	}
}

func TestParseGroupMode(t *testing.T) {
	for in, want := range map[string]groupMode{"machine": groupMachine, "Repo": groupRepo, " project ": groupProject, "none": groupNone, "flat": groupNone} {
		if got, ok := parseGroupMode(in); !ok || got != want {
			t.Errorf("parseGroupMode(%q) = %q %v", in, got, ok)
		}
	}
	if _, ok := parseGroupMode("color"); ok {
		t.Error("unknown mode accepted")
	}
}

// The attention grouping classes sessions by what they need from you; the
// attention sort floats needs-you sessions to the top inside any grouping;
// both are remembered.
func TestAttentionGroupingAndSort(t *testing.T) {
	mk := func(id, node string, st contracts.RunState, attn int) contracts.SessionView {
		s := sv(id, node, "/src/"+id)
		s.Run.State, s.Attention = st, attn
		return s
	}
	ss := []contracts.SessionView{
		mk("a-idle", "server", contracts.RunWaitingInput, 0),
		mk("b-work", "server", contracts.RunRunning, 0),
		mk("c-apr", "server", contracts.RunWaitingApproval, 0),
		mk("d-done", "server", contracts.RunExited, 0),
		mk("e-attn", "server", contracts.RunWaitingInput, 1),
		mk("f-fail", "server", contracts.RunFailed, 0),
	}
	m := &Model{}
	m.grouping.mode = groupAttention
	got := append([]contracts.SessionView(nil), ss...)
	m.sortGrouped(got)
	// A failed run with nothing open is history, not "needs you" (it reads
	// ✗ failed in its row).
	if g := ids(got); !reflect.DeepEqual(g, []string{"c-apr", "e-attn", "b-work", "a-idle", "d-done", "f-fail"}) {
		t.Fatalf("attention groups %v", g)
	}
	if l := m.groupLabel(m.groupKey(ss[4])); l != "Needs you" {
		t.Fatalf("attention count reads %q", l)
	}
	if l := m.groupLabel(m.groupKey(ss[3])); l != "Ended" {
		t.Fatalf("exited reads %q", l)
	}
	// Machine grouping with the attention sort: needs you first per machine.
	m.grouping.mode, m.grouping.sort = groupMachine, sortAttention
	got = append([]contracts.SessionView(nil), ss...)
	m.sortGrouped(got)
	if g := ids(got); g[0] != "c-apr" || g[len(g)-1] != "f-fail" || g[len(g)-2] != "d-done" || g[2] != "b-work" {
		t.Fatalf("attention sort %v", g)
	}
	var st tuiState
	m.grouping.save(&st)
	if st.Sort != "attention" {
		t.Fatalf("sort not saved: %+v", st)
	}
	var g groupingState
	g.load(st)
	if g.sort != sortAttention {
		t.Fatalf("sort not loaded: %+v", g)
	}
	if md, ok := parseGroupMode("needs"); !ok || md != groupAttention {
		t.Fatal(":group needs")
	}
}

// The Sessions view (sidenav, table, grid) round-trips through the state
// file; the default is sidenav and is not written.
func TestSessionsModeState(t *testing.T) {
	var st tuiState
	(&sessViewState{}).save(&st)
	if st.SessionsView != "" {
		t.Fatalf("default written: %+v", st)
	}
	(&sessViewState{mode: modeTable}).save(&st)
	var v sessViewState
	v.load(st)
	if v.mode != modeTable || v.lastList != modeTable {
		t.Fatalf("loaded %+v", v)
	}
	for in, want := range map[string]sessMode{"sidenav": modeSidenav, "list": modeSidenav, "Table": modeTable, "grid": modeGrid} {
		if got, ok := parseSessMode(in); !ok || got != want {
			t.Errorf("parseSessMode(%q) = %q", in, got)
		}
	}
}
