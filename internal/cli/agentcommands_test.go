package cli

import (
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/runtime/claudeplugin"
)

// TestAgentCommands: `pyre commands` lists the agent slash commands from
// the embedded skills (no hub needed) and prints one command's procedure.
func TestAgentCommands(t *testing.T) {
	e := newEnv(t, nil)
	r := run(t, e, nil, "commands")
	expectCode(t, r, 0)
	for _, u := range []string{"/kickoff [what to build]", "/open-items", "/ship [PR or branch]", "/brief", "/deliver [what]", "/cleanup", "/pair [question]"} {
		if !strings.Contains(r.stdout, "  "+u+" ") {
			t.Errorf("pyre commands lacks %q:\n%s", u, r.stdout)
		}
	}

	r = run(t, e, nil, "--json", "commands")
	expectCode(t, r, 0)
	got := mustJSON[[]claudeplugin.Skill](t, r)
	if len(got) != 7 || got[0].Name != "brief" || got[0].Short == "" || got[0].Usage != "/brief" {
		t.Fatalf("json: %+v", got)
	}

	if strings.Contains(r.stdout, "pyre-session:") {
		t.Errorf("pyre commands still names /pyre-session:<name>:\n%s", r.stdout)
	}

	r = run(t, e, nil, "commands", "/kickoff")
	expectCode(t, r, 0)
	if !strings.HasPrefix(r.stdout, "---\nname: kickoff\n") || !strings.Contains(r.stdout, "create_project") {
		t.Fatalf("procedure: %s", r)
	}
	r = run(t, e, nil, "--json", "commands", "pair")
	expectCode(t, r, 0)
	if d := mustJSON[CommandDoc](t, r); d.Name != "pair" || !strings.Contains(d.Text, "codex") {
		t.Fatalf("json doc: %+v", d)
	}
	expectCode(t, run(t, e, nil, "commands", "nope"), ExitUsage)

	r = run(t, e, nil, "--help")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "commands ") {
		t.Fatalf("pyre help does not list commands:\n%s", r.stdout)
	}
}
