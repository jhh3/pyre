package tui

import (
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestPickerSplit(t *testing.T) {
	for _, c := range []struct{ in, dir, seg string }{
		{"", "~", ""},
		{"~", "~", ""},
		{"~/", "~", ""},
		{"~/sr", "~", "sr"},
		{"~/src/", "~/src", ""},
		{"~/src//ap", "~/src", "ap"},
		{"/", "/", ""},
		{"/fa", "/", "fa"},
		{"/fast/worktrees/", "/fast/worktrees", ""},
		{"scratch", "~", "scratch"},
	} {
		if dir, seg := pickerSplit(c.in); dir != c.dir || seg != c.seg {
			t.Errorf("pickerSplit(%q) = %q %q, want %q %q", c.in, dir, seg, c.dir, c.seg)
		}
	}
}

func TestPickerMatchesHidesDotFolders(t *testing.T) {
	dirs := []string{".cache", ".config", "Documents", "scratch", "src"}
	for _, c := range []struct{ seg, want string }{
		{"", "Documents,scratch,src"},
		{"s", "scratch,src"},
		{"S", "scratch,src"},
		{"doc", "Documents"},
		{".", ".cache,.config"},
		{".co", ".config"},
		{"x", ""},
	} {
		if got := strings.Join(pickerMatches(dirs, c.seg), ","); got != c.want {
			t.Errorf("pickerMatches(%q) = %q, want %q", c.seg, got, c.want)
		}
	}
}

// ~ and / at the start of a segment jump home and to the root; elsewhere
// they are text, and "~/" typed in full keeps one slash.
func TestPickerTyping(t *testing.T) {
	for _, c := range []struct{ start, typed, want string }{
		{"~/src/", "~", "~/"},
		{"~/src/", "/", "/"},
		{"~/src/", "/fast/", "/fast/"},
		{"~/", "~/src", "~/src"},
		{"/", "~/", "~/"},
		{"~/src/a", "/", "~/src/a/"},
		{"~/", "My Docs", "~/My Docs"},
		{"~/a", "~b", "~/a~b"},
	} {
		b := &nsPicker{input: c.start}
		b.typeText(c.typed)
		if b.input != c.want {
			t.Errorf("%q + %q = %q, want %q", c.start, c.typed, b.input, c.want)
		}
	}
}

func TestTildeUnder(t *testing.T) {
	for _, c := range []struct{ p, home, want string }{
		{"/home/demo/wt", "/home/demo", "~/wt"},
		{"/home/demo", "/home/demo/", "~"},
		{"/home/demolition", "/home/demo", "/home/demolition"},
		{"/fast/worktrees", "/home/demo", "/fast/worktrees"},
		{"/fast/worktrees", "", "/fast/worktrees"},
	} {
		if got := tildeUnder(c.p, c.home); got != c.want {
			t.Errorf("tildeUnder(%q, %q) = %q, want %q", c.p, c.home, got, c.want)
		}
	}
}

func TestNodeLoadOrdersMachines(t *testing.T) {
	idle := contracts.NodeResources{CPUCount: 32, CPUPct: 7, MemTotalMB: 1000, MemAvailMB: 600, ActiveRuns: 1, MaxRuns: 16}
	busy := contracts.NodeResources{CPUCount: 10, CPUPct: 18, MemTotalMB: 1000, MemAvailMB: 370, ActiveRuns: 2, MaxRuns: 8}
	old := contracts.NodeResources{CPUCount: 4, Load1: 3.9, MemTotalMB: 1000, MemAvailMB: 900} // an older pyred: no cpu_pct
	if !(nodeLoad(idle) < nodeLoad(busy)) || !(nodeLoad(busy) < nodeLoad(old)) {
		t.Fatalf("load order: idle %.1f busy %.1f old %.1f", nodeLoad(idle), nodeLoad(busy), nodeLoad(old))
	}
	if got := nodeLoadText(idle); got != "1/16 runs · cpu 7% · mem 40%" {
		t.Fatalf("load text %q", got)
	}
	if got := nodeLoadText(old); got != "0 runs · load 3.9/4 · mem 10%" {
		t.Fatalf("older node load text %q", got)
	}
}

// pickerModel is a model with one node (connected or not) and a picker
// on it at input, with the given listings of this panel.
func pickerModel(t *testing.T, connected bool, input string, listings map[string]*loadState) (*Model, *nsPanel) {
	t.Helper()
	m := New(t.Context(), nil, Options{NoColor: true, KeysFile: "none", StateFile: "none"})
	st := contracts.ConnConnected
	if !connected {
		st = contracts.ConnDisconnected
	}
	m.loads["nodes"] = &loadState{have: true, val: []contracts.Node{{NodeID: "node_x", Name: "x", Connection: contracts.NodeConnection{State: st}}}}
	q := &nsPanel{instance: "op_t", picker: &nsPicker{node: "node_x", input: input}}
	for dir, ls := range listings {
		m.loads[q.pickKey("node_x", dir, false)] = ls
	}
	m.overlay = q
	return m, q
}

func listed(dirs ...string) *loadState {
	return &loadState{have: true, val: contracts.DirListResult{Path: "/home/x", Dirs: dirs, Home: "/home/x"}}
}

// A folder is new only on evidence (a listing without it, or the node
// saying it does not exist); while its listing loads Use waits; a listing
// that failed otherwise (a timeout) is not evidence.
func TestPickerNewFolderNeedsEvidence(t *testing.T) {
	stale := listed("api")
	stale.err = contracts.Errorf(contracts.ErrUnavailable, "timed out") // a failed reload keeps the earlier listing
	gone := &loadState{err: contracts.Errorf(contracts.ErrInvalid, `working directory "~/nope" does not exist on this node`)}
	timeout := &loadState{err: contracts.Errorf(contracts.ErrOutcomeIndeterminate, "dir.list: timed out")}
	denied := &loadState{err: contracts.Errorf(contracts.ErrInvalid, "cannot list /root: permission denied")}
	for _, c := range []struct {
		name, input string
		ls          *loadState
		isNew       bool
		known       bool
	}{
		{"listed, present", "~/src/api", listed("api"), false, true},
		{"listed, absent", "~/src/apx", listed("api"), true, true},
		{"stale error, present", "~/src/api", stale, false, true},
		{"does not exist", "~/nope/", gone, true, true},
		{"does not exist, segment", "~/nope/x", gone, true, true},
		{"timed out", "/fast/wt/", timeout, false, true},
		{"timed out, segment", "/fast/wt/x", timeout, false, true},
		{"permission denied", "/root/", denied, false, true},
		{"loading", "~/src/apx", &loadState{inflight: true}, false, false},
		{"not requested", "~/src/apx", nil, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, _ := pickerSplit(c.input)
			ls := map[string]*loadState{}
			if c.ls != nil {
				ls[dir] = c.ls
			}
			m, q := pickerModel(t, true, c.input, ls)
			use := q.pickerRows(m)[0]
			if use.isNew != c.isNew || q.pickerKnown(m) != c.known {
				t.Fatalf("isNew %v known %v, want %v %v", use.isNew, q.pickerKnown(m), c.isNew, c.known)
			}
		})
	}
	// A transient failure says so, and the picker keeps retrying.
	m, q := pickerModel(t, true, "/fast/wt/", map[string]*loadState{"/fast/wt": timeout})
	if got := strings.Join(q.pickerLines(m, 120, 8), "\n"); !strings.Contains(got, "retrying") || !strings.Contains(got, "not verified") || strings.Contains(got, "new folder") {
		t.Fatalf("timed-out listing:\n%s", got)
	}
	if !q.pickerWaiting(m) || q.loadPicker(m) == nil || !q.spinning {
		t.Fatal("a timed-out listing is not retried")
	}
}

// An offline machine: the picker says so, shows no spinner, and polls
// slowly; when the machine is back, the next tick lists the folder.
func TestPickerOfflineThenReconnect(t *testing.T) {
	m, q := pickerModel(t, false, "~/src/", nil)
	if !q.pickerKnown(m) {
		t.Fatal("offline machine: Use waits forever")
	}
	if got := strings.Join(q.pickerLines(m, 100, 8), "\n"); !strings.Contains(got, "x is offline: its folders cannot be listed") || strings.Contains(got, "listing") {
		t.Fatalf("offline lines:\n%s", got)
	}
	if cmd := q.updateMsg(m, nsSpinMsg{q: q}); cmd == nil || !q.spinning {
		t.Fatal("offline picker stopped polling")
	}
	if _, ok := m.loads[q.pickKey("node_x", "~/src", false)]; ok {
		t.Fatal("listed an offline machine")
	}
	m.loads["nodes"].val = []contracts.Node{{NodeID: "node_x", Name: "x", Connection: contracts.NodeConnection{State: contracts.ConnConnected}}}
	q.spinning = false
	q.updateMsg(m, nsSpinMsg{q: q})
	if st := m.loads[q.pickKey("node_x", "~/src", false)]; st == nil || !st.inflight {
		t.Fatal("reconnected machine not relisted")
	}
	if got := strings.Join(q.pickerLines(m, 100, 8), "\n"); !strings.Contains(got, "listing ~/src") {
		t.Fatalf("no spinner after reconnect:\n%s", got)
	}
}

// Dot-folders are a separate listing, asked for only for a "." segment.
func TestPickerHiddenListingOnlyForDot(t *testing.T) {
	m, q := pickerModel(t, true, "~/", map[string]*loadState{"~": listed("src")})
	q.loadPicker(m)
	if _, ok := m.loads[q.pickKey("node_x", "~", true)]; ok {
		t.Fatal("hidden listing fetched without a dot segment")
	}
	q.picker.typeText(".c")
	q.loadPicker(m)
	if _, ok := m.loads[q.pickKey("node_x", "~", true)]; !ok {
		t.Fatal("hidden listing not fetched for a dot segment")
	}
}

// Windows: drive roots, ← at ~ goes to the parent of home (not "."),
// "/" is the drive root, backslashes are separators.
func TestPickerWindowsPaths(t *testing.T) {
	for _, c := range []struct{ in, dir, seg string }{
		{"C:/", "C:/", ""},
		{"C:", "C:/", ""},
		{"C:/Users/", "C:/Users", ""},
		{"C:/Users/de", "C:/Users", "de"},
		{"D:/x//y/", "D:/x/y", ""},
	} {
		if dir, seg := pickerSplit(c.in); dir != c.dir || seg != c.seg {
			t.Errorf("pickerSplit(%q) = %q %q, want %q %q", c.in, dir, seg, c.dir, c.seg)
		}
	}
	for _, c := range []struct {
		dir, parent, name string
		ok                bool
	}{
		{"C:/Users/demo", "C:/Users", "demo", true},
		{"C:/Users", "C:/", "Users", true},
		{"C:/", "C:/", "", false},
		{"/", "/", "", false},
		{"/home", "/", "home", true},
	} {
		if p, n, ok := parentDir(c.dir); p != c.parent || n != c.name || ok != c.ok {
			t.Errorf("parentDir(%q) = %q %q %v", c.dir, p, n, ok)
		}
	}
	home := &loadState{have: true, val: contracts.DirListResult{Path: `C:\Users\demo`, Dirs: []string{"src"}, Home: `C:\Users\demo`}}
	m, q := pickerModel(t, true, "~/", map[string]*loadState{"~": home})
	q.syncPicker(m)
	q.pickerUp(m)
	if q.picker.input != "C:/Users/" || q.picker.hl != "dir:demo" {
		t.Fatalf("up from ~ = %q (hl %q)", q.picker.input, q.picker.hl)
	}
	q.picker.typeText("/")
	if q.picker.input != "C:/" {
		t.Fatalf("/ on windows = %q", q.picker.input)
	}
	q.pickerUp(m)
	if q.picker.input != "C:/" {
		t.Fatalf("up from the drive root = %q", q.picker.input)
	}
	q.picker.typeText(`Users\demo\src`)
	if q.picker.input != "C:/Users/demo/src" {
		t.Fatalf("backslashes = %q", q.picker.input)
	}
	q.picker.input = "~/"
	q.pickerPaste(m, `D:\work\pyre`)
	if q.picker.input != "D:/work/pyre" {
		t.Fatalf("pasted drive path = %q", q.picker.input)
	}
	// An older pyred on Windows (no home field): home from its ~ listing.
	old := &loadState{have: true, val: contracts.DirListResult{Path: `C:\Users\demo`, Dirs: []string{"src"}}}
	m2, q2 := pickerModel(t, true, "~/", map[string]*loadState{"~": old})
	q2.pickerUp(m2)
	if q2.picker.input != "C:/Users/" {
		t.Fatalf("older windows node: up from ~ = %q", q2.picker.input)
	}
}
