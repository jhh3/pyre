package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/jhh3/pyre/internal/audit"
	"github.com/jhh3/pyre/internal/contracts"
)

// fakeAudit is an API offering audit.list (only); err makes the hub answer
// with that error (e.g. an older hub's unsupported).
type fakeAudit struct {
	contracts.API
	items []contracts.AuditEntry
	err   error
	reqs  []contracts.ListAuditRequest
}

func (f *fakeAudit) ListAudit(_ context.Context, req contracts.ListAuditRequest) (contracts.Page[contracts.AuditEntry], error) {
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return contracts.Page[contracts.AuditEntry]{}, f.err
	}
	var out []contracts.AuditEntry
	for _, e := range f.items {
		if len(req.Kinds) == 0 || req.Kinds[0] == e.Kind {
			out = append(out, e)
		}
	}
	return contracts.Page[contracts.AuditEntry]{Items: out}, nil
}

var activityNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func synthAudit() []contracts.AuditEntry {
	es := []contracts.AuditEntry{
		{AuditID: 3, At: activityNow.Add(-time.Minute), Kind: contracts.AuditAccount, Action: contracts.AuditAccountSwitched, Automatic: true,
			Actor: contracts.AuditActor{Kind: "pyre"}, NodeID: "node_synth-mac", NodeName: "synth-mac", SessionID: "ses_synth-1", SessionTitle: "synth perf",
			FromAccountID: "acct_synth-a", From: "synth-a", FromNote: "weekly 96%", ToAccountID: "acct_synth-b", To: "synth-b", ToNote: "weekly 0%", Reason: "usage_limit"},
		{AuditID: 2, At: activityNow.Add(-2 * time.Minute), Kind: contracts.AuditSession, Action: contracts.AuditSessionStarted,
			Actor: contracts.AuditActor{Kind: "owner"}, SessionID: "ses_synth-1", SessionTitle: "synth perf", To: "claude"},
		{AuditID: 1, At: activityNow.Add(-3 * time.Minute), Kind: contracts.AuditFleet, Action: contracts.AuditNodeConnected, NodeName: "synth-mac", To: "0.9.0"},
	}
	for i := range es {
		es[i].Summary = audit.Summarize(es[i])
	}
	return es
}

func activityModel(t *testing.T, api contracts.API) *Model {
	t.Helper()
	m := New(context.Background(), api, Options{Keymap: DefaultKeymap(), StateFile: "none", NoColor: true, Clock: fixedClock(activityNow)})
	t.Cleanup(m.Close)
	return m
}

// run executes a load command synchronously and feeds the result back.
func runLoad(m *Model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case loadedMsg:
		m.onLoaded(msg)
	case tea.BatchMsg:
		for _, c := range msg {
			runLoad(m, c)
		}
	}
}

func TestActivityFilterCycles(t *testing.T) {
	f := &fakeAudit{items: synthAudit()}
	m := activityModel(t, f)
	runLoad(m, m.runAction("global.activity"))
	if m.view != viewActivity || m.activityKey() != "audit:all" {
		t.Fatalf("view %s key %s", m.view.name(), m.activityKey())
	}
	if len(f.reqs) != 1 || len(f.reqs[0].Kinds) != 0 || f.reqs[0].Limit != auditPageLimit {
		t.Fatalf("first request %+v", f.reqs)
	}
	want := []struct {
		key  string
		kind contracts.AuditKind
	}{{"audit:accounts", contracts.AuditAccount}, {"audit:sessions", contracts.AuditSession}, {"audit:fleet", contracts.AuditFleet}}
	for i, w := range want {
		runLoad(m, m.runAction("manage.filter"))
		if m.activityKey() != w.key {
			t.Fatalf("after %d presses key = %s, want %s", i+1, m.activityKey(), w.key)
		}
		last := f.reqs[len(f.reqs)-1]
		if len(last.Kinds) != 1 || last.Kinds[0] != w.kind {
			t.Fatalf("%s requested kinds %v", w.key, last.Kinds)
		}
		out := ansiStrip(strings.Join(m.viewActivity(120, 30), "\n"))
		if !strings.Contains(out, "["+strings.TrimPrefix(w.key, "audit:")) {
			t.Fatalf("active filter not marked:\n%s", out)
		}
	}
	runLoad(m, m.runAction("manage.filter"))
	if m.activityKey() != "audit:all" {
		t.Fatalf("filter does not wrap: %s", m.activityKey())
	}
	out := ansiStrip(strings.Join(m.viewActivity(120, 30), "\n"))
	for _, s := range []string{"[all 3]", "accounts 1", "sessions 1", "fleet 1", "↻ switched 'synth perf' from synth-a (weekly 96%) to synth-b (weekly 0%) — limit reached"} {
		if !strings.Contains(out, s) {
			t.Fatalf("missing %q:\n%s", s, out)
		}
	}
	// From Accounts, a opens the account entries and esc returns there.
	m.view, m.back = viewAccounts, []viewID{viewSessions}
	runLoad(m, m.runAction("manage.activity"))
	if m.view != viewActivity || m.activityKey() != "audit:accounts" {
		t.Fatalf("manage.activity: view %s key %s", m.view.name(), m.activityKey())
	}
	m.viewKey(keyFor("esc"))
	if m.view != viewAccounts {
		t.Fatalf("esc returned to %s, want Accounts", m.view.name())
	}
}

func TestActivityUnsupported(t *testing.T) {
	const msg = "this hub does not offer the activity log; upgrade pyre-hub"
	// An API without audit.list at all.
	m := activityModel(t, nil)
	runLoad(m, m.runAction("global.activity"))
	if out := ansiStrip(strings.Join(m.viewActivity(100, 20), "\n")); !strings.Contains(out, msg) {
		t.Fatalf("no unsupported note:\n%s", out)
	}
	if l := m.recentSwitchLines(100); l != nil {
		t.Fatalf("RECENT SWITCHES shown without an audit log: %q", l)
	}
	// A hub that predates audit.list answers unsupported.
	f := &fakeAudit{err: contracts.Errorf(contracts.ErrUnsupported, "audit log not offered")}
	m = activityModel(t, f)
	runLoad(m, m.runAction("global.activity"))
	if out := ansiStrip(strings.Join(m.viewActivity(100, 20), "\n")); !strings.Contains(out, msg) {
		t.Fatalf("no unsupported note for an older hub:\n%s", out)
	}
	runLoad(m, m.loadRecentSwitches(true))
	if l := m.recentSwitchLines(100); l != nil {
		t.Fatalf("RECENT SWITCHES shown for an older hub: %q", l)
	}
}

func TestActivitySwitchesStandOut(t *testing.T) {
	es := synthAudit()
	f := &fakeAudit{items: es}
	m := activityModel(t, f)
	m.th = newTheme(colorprofile.TrueColor, true)
	runLoad(m, m.runAction("global.activity"))
	out := strings.Join(m.viewActivity(300, 30), "\n")
	if !strings.Contains(out, m.th.tone(toneWarn, es[0].Summary)) {
		t.Fatalf("switch row is not in the warn tone:\n%q", out)
	}
	if strings.Contains(out, m.th.tone(toneWarn, es[1].Summary)) {
		t.Fatalf("an ordinary entry is in the warn tone")
	}
	if !strings.Contains(out, m.th.accent.Render(autoGlyph)) {
		t.Fatalf("automatic entry lacks the auto glyph")
	}

	// Accounts: Recent switches lists only switches, newest first.
	runLoad(m, m.loadRecentSwitches(true))
	lines := m.recentSwitchLines(300)
	joined := ansiStrip(strings.Join(lines, "\n"))
	if !strings.Contains(joined, "Recent switches") || !strings.Contains(joined, audit.Line(es[0], activityNow)) || strings.Contains(joined, "connected") {
		t.Fatalf("recent switches:\n%s", joined)
	}
	if req := f.reqs[len(f.reqs)-1]; len(req.Kinds) != 1 || req.Kinds[0] != contracts.AuditAccount {
		t.Fatalf("switches request kinds %v", req.Kinds)
	}
	f.items = es[1:] // no switches
	runLoad(m, m.loadRecentSwitches(true))
	if joined := ansiStrip(strings.Join(m.recentSwitchLines(100), "\n")); !strings.Contains(joined, "no automatic account switches yet") {
		t.Fatalf("empty state:\n%s", joined)
	}
}

func TestAuditInvalidation(t *testing.T) {
	for _, k := range []contracts.EventKind{"account.fallback_planned", "session.created", "node.connected"} {
		keys := strings.Join(invalidationKeys(contracts.Event{Kind: k}), ",")
		if !strings.Contains(keys, "audit:") {
			t.Fatalf("%s does not invalidate the activity log: %s", k, keys)
		}
	}
}
