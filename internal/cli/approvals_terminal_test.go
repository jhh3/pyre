package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
)

// approvalsAPI serves a fixed approval list and session views.
type approvalsAPI struct {
	c.API
	items    []c.Approval
	sessions map[string]c.SessionView
}

func (f *approvalsAPI) ListApprovals(ctx context.Context, r c.ListApprovalsRequest) (c.Page[c.Approval], error) {
	want := map[c.ApprovalState]bool{}
	for _, s := range r.States {
		want[s] = true
	}
	if len(want) == 0 {
		want[c.ApprovalPending] = true
	}
	var out []c.Approval
	for _, a := range f.items {
		if want[a.State] {
			out = append(out, a)
		}
	}
	return c.Page[c.Approval]{Items: out}, nil
}

func (f *approvalsAPI) GetSession(ctx context.Context, r c.GetRequest) (c.SessionView, error) {
	v, ok := f.sessions[r.ID]
	if !ok {
		return c.SessionView{}, c.Errorf(c.ErrNotFound, "no session")
	}
	return v, nil
}

// `pyre approvals` keeps an expired remote approval listed while its run
// still waits on the native prompt, saying how to resolve it; expired ones
// whose run moved on stay out of the default list.
func TestApprovalsListsPromptsWaitingInTheTerminal(t *testing.T) {
	waiting := c.SessionView{Session: c.Session{SessionID: "ses_synth-w", CurrentRunID: "run_synth-w"},
		Run: &c.Run{RunID: "run_synth-w", SessionID: "ses_synth-w", State: c.RunWaitingApproval}}
	movedOn := c.SessionView{Session: c.Session{SessionID: "ses_synth-m", CurrentRunID: "run_synth-m"},
		Run: &c.Run{RunID: "run_synth-m", SessionID: "ses_synth-m", State: c.RunWaitingInput}}
	api := &approvalsAPI{
		items: []c.Approval{
			{ApprovalID: "apr_synth-p", SessionID: "ses_synth-w", RunID: "run_synth-w", Title: "Allow Read?", State: c.ApprovalPending},
			{ApprovalID: "apr_synth-x", SessionID: "ses_synth-w", RunID: "run_synth-w", Title: "Allow Bash?", State: c.ApprovalExpired},
			{ApprovalID: "apr_synth-old", SessionID: "ses_synth-m", RunID: "run_synth-m", Title: "Allow Edit?", State: c.ApprovalExpired},
		},
		sessions: map[string]c.SessionView{"ses_synth-w": waiting, "ses_synth-m": movedOn},
	}
	o := &Ops{API: api}
	page, err := o.ListApprovals(context.Background(), ApprovalFilter{})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, a := range page.Items {
		ids[a.ApprovalID] = true
	}
	if !ids["apr_synth-p"] || !ids["apr_synth-x"] || ids["apr_synth-old"] || len(page.Items) != 2 {
		t.Fatalf("default list: %+v", page.Items)
	}
	var b bytes.Buffer
	printApprovals(&b, page.Items)
	out := b.String()
	if !strings.Contains(out, "in its terminal") || !strings.Contains(out, "pyre attach ses_synth-w") || !strings.Contains(out, "pyre interrupt ses_synth-w") {
		t.Fatalf("listing does not say how to resolve the terminal prompt:\n%s", out)
	}
}
