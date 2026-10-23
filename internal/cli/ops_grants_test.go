package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// fakeGrants offers the asks and standing grants ops (W-grants).
type fakeGrants struct {
	fakeAsks
	createdG []c.CreateGrantRequest
	listedG  []c.ListGrantsRequest
	revokedG []c.RevokeGrantRequest
}

func synthGrant(id string) c.StandingGrant {
	p := "prj_synth-1"
	return c.StandingGrant{GrantID: id, Scope: c.GrantScopeProject, ProjectID: &p, Action: "merge", Option: "merge", Condition: "reviewed and synthetic CI green",
		Quote: "synthetic: merge when green", AskID: "ask_synth-p", RecordedBy: c.Principal{Kind: c.PrincipalOwner, ID: "owner"},
		ExpiresAt: time.Now().Add(6 * 24 * time.Hour), UsedCount: 2, CreatedAt: time.Now().Add(-time.Hour)}
}

func (f *fakeGrants) CreateGrant(_ context.Context, req c.CreateGrantRequest) (c.StandingGrant, error) {
	f.createdG = append(f.createdG, req)
	return synthGrant("sgr_synth-1"), nil
}

func (f *fakeGrants) ListGrants(_ context.Context, req c.ListGrantsRequest) (c.ListGrantsResponse, error) {
	f.listedG = append(f.listedG, req)
	return c.ListGrantsResponse{Grants: []c.StandingGrant{synthGrant("sgr_synth-1")}}, nil
}

func (f *fakeGrants) RevokeGrant(_ context.Context, req c.RevokeGrantRequest) (c.StandingGrant, error) {
	f.revokedG = append(f.revokedG, req)
	g := synthGrant(req.GrantID)
	now := time.Now()
	g.RevokedAt, g.RevokeReason = &now, req.Reason
	return g, nil
}

func TestGrantTools(t *testing.T) {
	f := &fakeGrants{}
	a := synthAsk("ask_synth-p", c.AskOpen)
	f.create = c.CreateAskResponse{Status: c.AskCreated, Ask: &a}
	// No tool records a grant or a fact directly: sessions propose.
	// (add_fact leaves with ops_waiting.go; its pre-approval is gone, mcp_allow_test.go.)
	for _, gone := range []string{"record_grant", "create_grant"} {
		for _, tl := range MCPTools() {
			if tl.Name == gone {
				t.Errorf("MCP tool %s exists: sessions must not record the owner's words", gone)
			}
		}
	}
	text, err := callAskTool(t, f, "propose_grant", `{"action":" merge ","option":"merge","note":"reviewed and green","why":"synthetic: the owner merged the last 5 like this","scope":"Project","expires_days":3}`)
	if err != nil {
		t.Fatal(err)
	}
	r := f.created[0]
	if r.Proposal == nil || r.Proposal.Kind != c.ProposalGrant || r.Proposal.Grant == nil || r.OperationID == "" || r.Context != "synthetic: the owner merged the last 5 like this" {
		t.Fatalf("request: %+v", r)
	}
	if g := r.Proposal.Grant; g.Action != "merge" || g.Option != "merge" || g.Condition != "reviewed and green" || g.Scope != c.GrantScopeProject || g.ExpiresInDays != 3 {
		t.Fatalf("proposal: %+v", g)
	}
	if !strings.Contains(text, "asked ask_synth-p") || !strings.Contains(text, "Nothing is recorded unless the owner approves") {
		t.Errorf("propose text:\n%s", text)
	}
	for args, want := range map[string]string{`{"action":"merge"}`: "option is required", `{"option":"q"}`: "action is required",
		`{"action":"m","option":"q","scope":"repo"}`: "scope is project or session", `{"action":"m","option":"q","expires_days":31}`: "expires is 1 to 30"} {
		if _, err := callAskTool(t, f, "propose_grant", args); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", args, err, want)
		}
	}
	if _, err := callAskTool(t, f, "propose_fact", `{"fact":"GPU provider is synthetic-fal","why":"synthetic: the owner said so in ask_synth-0"}`); err != nil {
		t.Fatal(err)
	}
	if r := f.created[1]; r.Proposal == nil || r.Proposal.Kind != c.ProposalFact || r.Proposal.Fact != "GPU provider is synthetic-fal" {
		t.Fatalf("fact proposal: %+v", r)
	}
	if _, err := callAskTool(t, f, "propose_fact", `{"fact":" "}`); err == nil {
		t.Error("empty fact accepted")
	}

	// list_grants defaults to the calling session.
	out, err := askTool(t, "list_grants").run(context.Background(), &Ops{API: f, Self: Identity{SessionID: "ses_synth-self"}}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if f.listedG[0].SessionID != "ses_synth-self" || !strings.Contains(string(out.(compactText)), `merge → "merge" · project prj_synth-1 · owner: "synthetic: merge when green" (approved ask_synth-p) · note: reviewed and synthetic CI green · expires`) ||
		!strings.Contains(string(out.(compactText)), "used 2×") {
		t.Fatalf("list: %+v\n%s", f.listedG[0], out)
	}
	if _, err := askTool(t, "list_grants").run(context.Background(), &Ops{API: f, Self: Identity{SessionID: "ses_synth-self"}}, json.RawMessage(`{"all":true}`)); err != nil || f.listedG[1].SessionID != "" {
		t.Fatalf("list all: %+v %v", f.listedG[1], err)
	}
	if !askTool(t, "list_grants").Annotations.ReadOnlyHint {
		t.Error("list_grants must be read-only")
	}

	text, err = callAskTool(t, f, "revoke_grant", `{"grant_id":"sgr_synth-1","reason":"owner took it back"}`)
	if err != nil || f.revokedG[0].Reason != "owner took it back" || !strings.Contains(text, "revoked") {
		t.Fatalf("revoke: %v %+v\n%s", err, f.revokedG, text)
	}
	if _, err := callAskTool(t, f, "revoke_grant", `{"grant_id":"ask_synth-1"}`); err == nil {
		t.Error("an ask ID accepted as a grant")
	}
	if _, err := askTool(t, "list_grants").run(context.Background(), &Ops{API: struct{ c.API }{}}, json.RawMessage(`{}`)); !c.IsCode(err, c.ErrUnsupported) {
		t.Errorf("unsupported: %v", err)
	}
	// A click-only approval is never rendered as the owner's words.
	g := synthGrant("sgr_synth-2")
	g.Quote = ""
	if line := grantLine(g, time.Now()); !strings.Contains(line, "owner approved proposal ask_synth-p (no words typed)") || strings.Contains(line, "owner:") {
		t.Errorf("click-only line: %s", line)
	}
}

func TestGrantCLIAndBatchedAskFlags(t *testing.T) {
	e := newEnv(t, nil)
	f := &fakeGrants{}
	r := run(t, e, &App{API: f}, "grant", "add", "--action", "merge", "--option", "merge", "--note", "reviewed and CI green", "--quote", "merge when reviewed and green", "--scope", "session", "--expires", "3")
	expectCode(t, r, 0)
	if g := f.createdG[0]; g.Action != "merge" || g.Option != "merge" || g.Scope != c.GrantScopeSession || g.Condition != "reviewed and CI green" ||
		g.ExpiresAt == nil || g.ExpiresAt.Sub(time.Now()) < 71*time.Hour || g.ExpiresAt.Sub(time.Now()) > 73*time.Hour {
		t.Fatalf("add: %+v", g)
	}
	expectCode(t, run(t, e, &App{API: f}, "grant", "add", "--action", "merge"), ExitUsage)
	expectCode(t, run(t, e, &App{API: f}, "grant", "add", "--action", "merge", "--option", "merge", "--expires", "40"), ExitUsage)
	a0 := synthAsk("ask_synth-p", c.AskOpen)
	f.create = c.CreateAskResponse{Status: c.AskCreated, Ask: &a0}
	expectCode(t, run(t, e, &App{API: f}, "propose", "grant", "--action", "merge", "--option", "merge", "--why", "synthetic reason"), 0)
	expectCode(t, run(t, e, &App{API: f}, "propose", "fact", "synthetic fact", "--why", "synthetic reason"), 0)
	if len(f.created) != 2 || f.created[0].Proposal.Grant.Option != "merge" || f.created[1].Proposal.Fact != "synthetic fact" {
		t.Fatalf("propose: %+v", f.created)
	}
	f.created = nil
	r = run(t, e, &App{API: f}, "grant", "ls", "--all")
	expectCode(t, r, 0)
	if l := f.listedG[0]; !l.IncludeRevoked || l.SessionID != "" || !strings.Contains(r.stdout, "sgr_synth-1") {
		t.Fatalf("ls --all: %+v\n%s", l, r)
	}
	r = run(t, e, &App{API: f}, "grant", "revoke", "sgr_synth-1", "--reason", "synthetic")
	expectCode(t, r, 0)
	if f.revokedG[0].GrantID != "sgr_synth-1" || f.revokedG[0].Reason != "synthetic" {
		t.Fatalf("revoke: %+v", f.revokedG)
	}

	// pyre ask new --action --item; pyre ask answer --item.
	a := synthAsk("ask_synth-9", c.AskOpen)
	f.create = c.CreateAskResponse{Status: c.AskCreated, Ask: &a}
	r = run(t, e, &App{API: f}, "ask", "new", "--topic", "merge-batch", "--title", "Merge these?", "--context", "Synthetic.",
		"--option", "a=Merge", "--option", "b=Hold", "--action", "merge",
		"--item", "pr-1=Synthetic PR 1|docs only|https://example.invalid/1", "--item", "pr-2=Synthetic PR 2")
	expectCode(t, r, 0)
	req := f.created[0]
	if req.Action != "merge" || len(req.Items) != 2 || req.Items[0] != (c.AskItem{ID: "pr-1", Label: "Synthetic PR 1", Detail: "docs only", URL: "https://example.invalid/1"}) ||
		req.Items[1].Label != "Synthetic PR 2" {
		t.Fatalf("new: %+v", req)
	}
	expectCode(t, run(t, e, &App{API: f}, "ask", "new", "--topic", "t", "--title", "t", "--context", "c", "--option", "a=x", "--option", "b=y", "--item", "nolabel"), ExitUsage)

	r = run(t, e, &App{API: f}, "ask", "answer", "ask_synth-1", "a", "--item", "pr-2=b", "--item", "pr-3 = b")
	expectCode(t, r, 0)
	if it := f.answered[0].Answers[0]; it.OptionID != "a" || it.Items["pr-2"] != "b" || it.Items["pr-3"] != "b" {
		t.Fatalf("answer items: %+v", it)
	}
	r = run(t, e, &App{API: f}, "ask", "answer", "ask_synth-1", "--item", "pr-1=a") // per item only
	expectCode(t, r, 0)
	expectCode(t, run(t, e, &App{API: f}, "ask", "answer", "ask_synth-1", "ask_synth-2", "--item", "pr-1=a"), ExitUsage)
	expectCode(t, run(t, e, &App{API: f}, "ask", "answer", "ask_synth-1", "--item", "pr-1"), ExitUsage)

	// A granted ask renders as granted.
	g := synthAsk("ask_synth-g", c.AskAnswered)
	g.GrantID = "sgr_synth-1"
	g.Answer = &c.AskAnswer{OptionID: "b", Text: `standing grant sgr_synth-1 (merge → option "b"; owner approved proposal ask_synth-p (no words typed))`, By: c.Principal{Kind: c.PrincipalSystem, ID: "grants"}, At: time.Now()}
	if text := askCreateText(c.CreateAskResponse{Status: c.AskGranted, Ask: &g}, time.Now()); !strings.Contains(text, "granted, the owner was not asked") ||
		!strings.Contains(text, `answered b "Add a column" by standing grant sgr_synth-1`) || !strings.Contains(text, "Go ahead") {
		t.Fatalf("granted text:\n%s", text)
	}
}
