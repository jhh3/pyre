package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// fakeGrantsAPI is a backend with only the standing grants ops.
type fakeGrantsAPI struct {
	contracts.API
	revoked []contracts.RevokeGrantRequest
}

func (f *fakeGrantsAPI) CreateGrant(context.Context, contracts.CreateGrantRequest) (contracts.StandingGrant, error) {
	return contracts.StandingGrant{}, contracts.Errorf(contracts.ErrForbidden, "synthetic")
}

func (f *fakeGrantsAPI) ListGrants(context.Context, contracts.ListGrantsRequest) (contracts.ListGrantsResponse, error) {
	return contracts.ListGrantsResponse{}, nil
}

func (f *fakeGrantsAPI) RevokeGrant(_ context.Context, req contracts.RevokeGrantRequest) (contracts.StandingGrant, error) {
	f.revoked = append(f.revoked, req)
	return contracts.StandingGrant{GrantID: req.GrantID}, nil
}

// The project's Grants tab lists its standing go-aheads (the owner's own
// words only) and x revokes one after a confirmation.
func TestProjectGrantsTabListsAndRevokes(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	api := &fakeGrantsAPI{}
	m := New(context.Background(), api, Options{Keymap: DefaultKeymap(), StateFile: "none", NoColor: true, Clock: fixedClock(now)})
	defer m.Close()
	pid := "prj_synth-g"
	m.curProject = pid
	m.loads[grantsKey(pid)] = &loadState{have: true, at: now, val: []contracts.StandingGrant{
		{GrantID: "sgr_synth-1", Scope: contracts.GrantScopeProject, ProjectID: &pid, Action: "merge", Option: "merge", Quote: "yes, once green",
			AskID: "ask_synth-p", ExpiresAt: now.Add(48 * time.Hour), UsedCount: 2, Condition: "reviewed and green"},
		{GrantID: "sgr_synth-2", Scope: contracts.GrantScopeSession, SessionID: "ses_synth-x", ProjectID: &pid, Action: "tag", Option: "a",
			AskID: "ask_synth-q", ExpiresAt: now.Add(time.Hour)},
	}}
	out := strings.Join(m.projectGrantsView(contracts.Project{ProjectID: pid}, 120, 40), "\n")
	for _, want := range []string{"STANDING GO-AHEADS", "merge → merge", `you: "yes, once green" (approved ask_synth-p)`, "used 2×",
		"note (not checked): reviewed and green", "tag → a", "you approved proposal ask_synth-q (no words)", "tree ses_synth-x", "x revoke"} {
		if !strings.Contains(out, want) {
			t.Errorf("grants tab lacks %q:\n%s", want, out)
		}
	}
	if cmd := m.keyProjectGrants("x"); cmd != nil || m.overlay == nil {
		t.Fatalf("x did not ask to confirm")
	}
	c, ok := m.overlay.(*confirmOverlay)
	if !ok || !strings.Contains(strings.Join(c.body, " "), "sgr_synth-1") {
		t.Fatalf("confirm: %+v", m.overlay)
	}
	msg := c.yes(m)()
	if len(api.revoked) != 1 || api.revoked[0].GrantID != "sgr_synth-1" || api.revoked[0].OperationID == "" {
		t.Fatalf("revoke: %+v (%T)", api.revoked, msg)
	}
}
