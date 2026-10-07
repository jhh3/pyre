package cli

import (
	"encoding/json"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

func auditApp(t *testing.T) (*App, env) {
	t.Helper()
	b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &testOwner})
	return &App{API: b}, newEnv(t, nil)
}

// The owner's question: when did an automatic account switch happen?
func TestAuditCommandShowsAccountSwitches(t *testing.T) {
	app, e := auditApp(t)
	r := run(t, e, app, "audit", "--kind", "account", "--since", "all")
	expectCode(t, r, 0)
	for _, want := range []string{
		"⚙ ", "switched ", " — limit reached", "hit a limit on claude-personal-a (weekly 41%); no other account is eligible (waiting for a reset)",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("missing %q\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "started ") {
		t.Errorf("--kind account shows session events:\n%s", r.stdout)
	}
	// Oldest first: the last line is the newest entry.
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if !strings.Contains(lines[len(lines)-1], "hit a limit") {
		t.Errorf("newest entry should be last:\n%s", r.stdout)
	}
}

func TestAuditCommandJSONAndFilters(t *testing.T) {
	app, e := auditApp(t)
	r := run(t, e, app, "--json", "audit", "--since", "all", "--session", "ses_synth-fb")
	expectCode(t, r, 0)
	var p c.Page[c.AuditEntry]
	if err := json.Unmarshal([]byte(r.stdout), &p); err != nil {
		t.Fatalf("json: %v\n%s", err, r.stdout)
	}
	if len(p.Items) == 0 {
		t.Fatal("no entries for the fallback session")
	}
	for _, it := range p.Items {
		if it.SessionID != "ses_synth-fb" || it.Summary == "" {
			t.Fatalf("filter/summary: %+v", it)
		}
	}
	r = run(t, e, app, "audit", "--kind", "bogus")
	expectCode(t, r, 2)
	r = run(t, e, app, "audit", "--since", "yesterday-ish")
	expectCode(t, r, 2)
	r = run(t, e, app, "audit", "--account", "claude-personal-a", "--since", "all")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "claude-personal-a") {
		t.Fatalf("account filter:\n%s", r.stdout)
	}
}

func TestAuditMCPToolIsReadOnly(t *testing.T) {
	for _, tl := range MCPTools() {
		if tl.Name != "audit_log" {
			continue
		}
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Fatalf("audit_log annotations: %+v", tl.Annotations)
		}
		return
	}
	t.Fatal("audit_log is not listed")
}
