package tui_test

import (
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// sessionIDByTitle finds a fixture session.
func sessionIDByTitle(t *testing.T, b *mock.Backend, title string) string {
	t.Helper()
	page, err := b.ListSessions(ctx(), contracts.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range page.Items {
		if s.Session.Title == title {
			return s.Session.SessionID
		}
	}
	t.Fatalf("no session %q", title)
	return ""
}

func setIcon(t *testing.T, b *mock.Backend, title, icon string) {
	t.Helper()
	sid := sessionIDByTitle(t, b, title)
	owner := contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synth-icon"}
	if _, err := b.UpdateSession(contracts.WithPrincipal(ctx(), owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-icon-" + sid}, SessionID: sid, Icon: &icon}); err != nil {
		t.Fatal(err)
	}
}

// W-archive2: a session's emoji shows before its title in the sidenav,
// the table, the grid and the switcher, with wide-emoji width math that
// keeps the columns aligned.
func TestSessionIconsRenderAligned(t *testing.T) {
	prep := func(b *mock.Backend) {
		setIcon(t, b, "outfit API client", "🚀")
		setIcon(t, b, "worker", "🐛")
	}
	h, _, _ := startMock(t, 100, 30, prep, nil)
	if !h.Contains("🚀 outfit API client") || !h.Contains("🐛 worker") {
		t.Fatalf("icons before titles:\n%s", h.View())
	}
	golden(t, "sessions-icon-100", h.View())
	h.Key("v") // the table
	h.Settle()
	golden(t, "sessions-icon-table-100", h.View())
	h.Key("v") // the grid
	h.Settle()
	if !h.Contains("🐛 worker") {
		t.Fatalf("grid tile title:\n%s", h.View())
	}
}

// :icon sets the selected session's emoji; :icon alone clears it.
func TestIconCommand(t *testing.T) {
	h, rec, b := startMock(t, 100, 30, nil, nil)
	sid := h.M.SelectedSession()
	h.Key(":")
	h.Type("icon 🧪")
	h.Key("enter")
	h.WaitFor("icon set", 5*sec, func() bool { return len(rec.Calls("UpdateSession")) == 1 })
	h.Settle()
	s, err := b.GetSession(ctx(), contracts.GetRequest{ID: sid})
	if err != nil || s.Session.Icon != "🧪" {
		t.Fatalf("icon: %v %q", err, s.Session.Icon)
	}
	h.WaitFor("icon shown", 5*sec, func() bool { return h.Contains("🧪 ") })
	h.Key(":")
	h.Type("icon")
	h.Key("enter")
	h.WaitFor("icon cleared", 5*sec, func() bool { return len(rec.Calls("UpdateSession")) == 2 })
	if s, _ := b.GetSession(ctx(), contracts.GetRequest{ID: sid}); s.Session.Icon != "" {
		t.Fatalf("cleared: %q", s.Session.Icon)
	}
}
