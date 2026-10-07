package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A bypass run starts its thread with approval never + danger-full-access
// (the app-server form of --dangerously-bypass-approvals-and-sandbox) and
// its attached TUI carries the flag; an ask run keeps the node defaults.
func TestCodexPermissionsBypass(t *testing.T) {
	e := newEnv(t, nil)
	h := e.launch(e.a, "run_byp", e.home, nil, map[string]string{PermissionsEnv: "bypass"})
	var m runMeta
	b, _ := os.ReadFile(filepath.Join(h.StateDir, "run.json"))
	if json.Unmarshal(b, &m) != nil || m.ApprovalPolicy != "never" || m.Sandbox != "danger-full-access" {
		t.Fatalf("bypass run meta: %s", b)
	}
	h2 := e.launch(e.a, "run_ask", e.home, nil, nil)
	var m2 runMeta
	b, _ = os.ReadFile(filepath.Join(h2.StateDir, "run.json"))
	if json.Unmarshal(b, &m2) != nil || m2.ApprovalPolicy == "never" || m2.Sandbox == "danger-full-access" {
		t.Fatalf("ask run meta: %s", b)
	}
	hs := &homeServer{exe: "/synthetic/codex", sock: "/synthetic/s.sock"}
	// codex 0.160 exits 1 on any permission override with `resume --remote`;
	// bypass lives on the thread (run meta above), never on the TUI argv.
	for _, mm := range []runMeta{m, m2} {
		argv := e.a.tuiArgv(&runRef{hs: hs, meta: mm})
		for _, bad := range []string{"--dangerously-bypass-approvals-and-sandbox", "--sandbox", "--ask-for-approval", "-a", "-s"} {
			if slices.Contains(argv, bad) {
				t.Fatalf("TUI argv carries a permission override %q (codex refuses it on resume --remote): %v", bad, argv)
			}
		}
	}
}
