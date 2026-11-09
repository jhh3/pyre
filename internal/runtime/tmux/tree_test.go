package tmux

import (
	"os"
	"reflect"
	"slices"
	"testing"
)

func refPIDs(refs []procRef) []int {
	var out []int
	for _, r := range refs {
		out = append(out, r.pid)
	}
	slices.Sort(out)
	return out
}

// A synthetic process table shaped like a Claude pane on Linux
// (iss_…): pane 100 (claude) with a plugin
// wrapper and its server in its group, a Bash tool shell in a session of
// its own with a child, a detached server that left the group and
// session but is still a descendant, a command moved to a scope of its
// own, a `setsid -f` job (reparented to the subreaper), an orphan still
// in the pane's group, a zombie, and an unrelated process.
func syntheticTable() []procInfo {
	return []procInfo{
		{pid: 1, ppid: 0, pgid: 1, sid: 1, start: "1"},
		{pid: 50, ppid: 1, pgid: 50, sid: 50, start: "5"},       // systemd --user (subreaper)
		{pid: 100, ppid: 60, pgid: 100, sid: 100, start: "10"},  // pane: claude
		{pid: 101, ppid: 100, pgid: 100, sid: 100, start: "11"}, // bun run start
		{pid: 102, ppid: 101, pgid: 100, sid: 100, start: "12"}, // bun server.ts
		{pid: 110, ppid: 100, pgid: 110, sid: 110, start: "13"}, // Bash tool zsh
		{pid: 111, ppid: 110, pgid: 110, sid: 110, start: "14"}, // its command
		{pid: 120, ppid: 100, pgid: 120, sid: 120, start: "15"}, // detached MCP server
		{pid: 130, ppid: 100, pgid: 130, sid: 130, start: "16"}, // systemd-run --scope command
		{pid: 131, ppid: 130, pgid: 130, sid: 130, start: "17"}, // its child
		{pid: 140, ppid: 50, pgid: 140, sid: 140, start: "18"},  // setsid -f job
		{pid: 150, ppid: 50, pgid: 100, sid: 100, start: "19"},  // orphan still in the group
		{pid: 160, ppid: 100, pgid: 100, sid: 100, start: "20", zombie: true},
		{pid: 200, ppid: 50, pgid: 200, sid: 200, start: "21"},         // unrelated
		{pid: os.Getpid(), ppid: 50, pgid: 100, sid: 100, start: "22"}, // pyred itself
	}
}

func syntheticCgroup(pid int) string {
	switch pid {
	case 130, 131:
		return "/user.slice/app.slice/run-u1.scope"
	case 140:
		return "/pyre-runs.slice/pyre-run-x.scope" // setsid -f stays in the run's scope
	}
	return "/pyre-runs.slice/pyre-run-x.scope"
}

func TestTreeOfLiveRoot(t *testing.T) {
	got := refPIDs(treeOf(syntheticTable(), 100, true, syntheticCgroup))
	want := []int{100, 101, 102, 110, 111, 120, 150}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tree = %v, want %v (the setsid -f job 140, the scope command 130/131, the zombie, pyred and the unrelated process stay out)", got, want)
	}
}

func TestTreeOfDeadRoot(t *testing.T) {
	// The pane already exited: only its group and session are left to
	// find (the descendants were reparented).
	var procs []procInfo
	for _, p := range syntheticTable() {
		if p.pid != 100 {
			procs = append(procs, p)
		}
	}
	got := refPIDs(treeOf(procs, 100, false, syntheticCgroup))
	want := []int{101, 102, 150}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tree = %v, want %v", got, want)
	}
}

func TestTreeOfUnknownCgroupKeepsDescendants(t *testing.T) {
	// macOS (no cgroups): every descendant counts.
	got := refPIDs(treeOf(syntheticTable(), 100, true, func(int) string { return "" }))
	want := []int{100, 101, 102, 110, 111, 120, 130, 131, 150}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tree = %v, want %v", got, want)
	}
}

func withProcs(t *testing.T, procs []procInfo) {
	t.Helper()
	old := listProcs
	listProcs = func() ([]procInfo, error) { return procs, nil }
	t.Cleanup(func() { listProcs = old })
}

func TestLiveRefsDropsReusedAndZombies(t *testing.T) {
	withProcs(t, []procInfo{
		{pid: 10, start: "a"},
		{pid: 11, start: "other"}, // PID reused
		{pid: 12, start: "c", zombie: true},
	})
	got := refPIDs(liveRefs([]procRef{{10, "a"}, {11, "b"}, {12, "c"}, {13, "d"}}))
	if !reflect.DeepEqual(got, []int{10}) {
		t.Fatalf("live = %v, want [10]", got)
	}
}

func TestSignalRefsOnlyLive(t *testing.T) {
	withProcs(t, []procInfo{{pid: 10, start: "a"}, {pid: 11, start: "x"}})
	var sent []int
	old := signalProc
	signalProc = func(pid int, force bool) error { sent = append(sent, pid); return nil }
	t.Cleanup(func() { signalProc = old })
	signalRefs([]procRef{{10, "a"}, {11, "b"}}, true)
	if !reflect.DeepEqual(sent, []int{10}) {
		t.Fatalf("signalled %v, want [10]", sent)
	}
}

func TestLeaderReused(t *testing.T) {
	withProcs(t, []procInfo{{pid: 10, start: "a"}, {pid: 11, start: "b", zombie: true}})
	if !leaderReused(10) {
		t.Fatal("a live process with the pane's PID is a reuse")
	}
	if leaderReused(11) {
		t.Fatal("a zombie with the pane's PID is the pane itself")
	}
	if leaderReused(12) {
		t.Fatal("a free PID is no reuse")
	}
}

func TestParsePSTable(t *testing.T) {
	out := "  100     1   100 Ss   Thu Nov  5 10:09:02 2026\n  101   100   100 Z+   Thu Nov  5 10:09:03 2026\nbad line\n"
	got := parsePSTable(out)
	want := []procInfo{
		{pid: 100, ppid: 1, pgid: 100, sid: -1, start: "Thu Nov 5 10:09:02 2026"},
		{pid: 101, ppid: 100, pgid: 100, sid: -1, start: "Thu Nov 5 10:09:03 2026", zombie: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// The devbox case (iss_…): a Bash tool ran
// `pyre run -- claude -p`, whose nested Claude led its own group in the
// tool's session; both exited and the nested plugin server (300) was
// reparented to the subreaper, outside the pane's group, session and tree,
// but still in the run's scope.
func scopeTable() []procInfo {
	return append(syntheticTable(),
		procInfo{pid: 300, ppid: 50, pgid: 290, sid: 280, start: "30"},  // orphaned nested plugin server
		procInfo{pid: 310, ppid: 50, pgid: 310, sid: 310, start: "31"},  // setsid -f job (leads its session)
		procInfo{pid: 311, ppid: 310, pgid: 310, sid: 310, start: "32"}, // its child
		procInfo{pid: 320, ppid: 50, pgid: 320, sid: 281, start: "33"},  // leftover moved to a scope of its own
		procInfo{pid: 330, ppid: 50, pgid: 330, sid: 282, start: "34"},  // leftover of another run
		procInfo{pid: 340, ppid: 50, pgid: 340, sid: 283, start: "35"},  // leftover in the shared tmux scope
		procInfo{pid: 341, ppid: 50, pgid: 341, sid: 341, start: "36", zombie: true},
		procInfo{pid: 342, ppid: 50, pgid: 342, sid: 341, start: "37"}, // its session's leader is a zombie
		procInfo{pid: 350, ppid: 50, pgid: 350, sid: 284, start: "38"}, // leftover of run "x-y" (a longer name)
	)
}

func scopeCgroup(pid int) string {
	switch pid {
	case 320:
		return "/user.slice/app.slice/run-u2.scope"
	case 330:
		return "/pyre-runs.slice/pyre-run-run_other-k3j.scope"
	case 340:
		return "/pyre-runs.slice/pyre-tmux-5bd28fec259b.scope"
	case 350:
		return "/pyre-runs.slice/pyre-run-x-y-k3j.scope"
	}
	return "/pyre.slice/pyre-runs.slice/pyre-run-x-29s22iwx.scope"
}

func TestScopeOrphans(t *testing.T) {
	got := refPIDs(scopeOrphans(scopeTable(), "x", scopeCgroup))
	want := []int{300, 342}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("leftovers = %v, want %v (setsid -f job 310/311, other scopes 320/330/340/350, live-led sessions, zombies and pyred stay out)", got, want)
	}
	if got := scopeOrphans(scopeTable(), "", scopeCgroup); got != nil {
		t.Fatalf("no session name: %v", got)
	}
	// macOS: no session IDs and no cgroups.
	var mac []procInfo
	for _, p := range scopeTable() {
		p.sid = -1
		mac = append(mac, p)
	}
	if got := scopeOrphans(mac, "x", func(int) string { return "" }); got != nil {
		t.Fatalf("macOS leftovers = %v, want none", refPIDs(got))
	}
}

func TestScopeLeftoversUsesProcessTable(t *testing.T) {
	withProcs(t, scopeTable())
	old := procCgroup
	procCgroup = scopeCgroup
	t.Cleanup(func() { procCgroup = old })
	if got := refPIDs(scopeLeftovers("x")); !reflect.DeepEqual(got, []int{300, 342}) {
		t.Fatalf("leftovers = %v", got)
	}
}

func TestIsRunScope(t *testing.T) {
	for cg, want := range map[string]bool{
		"/user.slice/user-1001.slice/user@1001.service/pyre.slice/pyre-runs.slice/pyre-run-run_…-aficw216.scope": true,
		"/pyre-runs.slice/pyre-run-run_…-aficw216.scope/sub":                                                     false,
		"/pyre-runs.slice/pyre-run-run_…-.scope":                                                                 false,
		"/pyre-runs.slice/pyre-run-run_…-as-default-k3.scope":                                                    false,
		"/pyre-runs.slice/pyre-run-run_…-K3.scope":                                                               false,
		"/pyre-runs.slice/pyre-tmux-5bd28fec259b.scope":                                                                                   false,
		"": false,
	} {
		if got := isRunScope(cg, "run_…"); got != want {
			t.Errorf("isRunScope(%q) = %v, want %v", cg, got, want)
		}
	}
}
