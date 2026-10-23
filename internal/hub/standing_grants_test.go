package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// ---- standing grants, proposals and batched asks (W-grants) ----

const synthProject = "prj_synth-grants"

// startUnder starts a session as parentID's child.
func (f *childFixture) startUnder(parentID, op string) contracts.SessionView {
	x := f.x
	x.t.Helper()
	resp, err := x.h.StartSession(as(sesPrincipal(parentID)), contracts.StartSessionRequest{
		Mutation: contracts.Mutation{OperationID: op}, NodeID: nodeMac, Harness: contracts.HarnessClaude, Cwd: "/home/synth/grandchild",
		Title: "synthetic grandchild", InitialPrompt: "run the synthetic checks"})
	if err != nil {
		x.t.Fatalf("start under %s: %v", parentID, err)
	}
	cmds := x.ch.sentKinds(nodeMac, contracts.CmdStartRun)
	cmd := cmds[len(cmds)-1]
	x.ack(nodeMac, cmd)
	run := *resp.Session.Run
	run.State = contracts.RunWaitingInput
	x.result(nodeMac, cmd, contracts.OutcomeSucceeded, run)
	return x.session(resp.Session.Session.SessionID)
}

func (f *childFixture) grant(req contracts.CreateGrantRequest) contracts.StandingGrant {
	f.x.t.Helper()
	g, err := f.x.h.CreateGrant(as(owner), req)
	if err != nil {
		f.x.t.Fatalf("create grant %s: %v", req.OperationID, err)
	}
	return g
}

// treeGrant is the owner's grant for sessionID's tree: action merge,
// option a.
func treeGrant(op, sessionID string) contracts.CreateGrantRequest {
	return contracts.CreateGrantRequest{Mutation: contracts.Mutation{OperationID: op}, Scope: contracts.GrantScopeSession, SessionID: sessionID,
		Action: "merge", Option: "a", Condition: "reviewed and synthetic CI green", Quote: "synthetic owner: merge when reviewed and green"}
}

func projectGrant(op string) contracts.CreateGrantRequest {
	pr := synthProject
	return contracts.CreateGrantRequest{Mutation: contracts.Mutation{OperationID: op}, ProjectID: &pr, Action: "merge", Option: "a"}
}

// mergeAsk is an ask naming action merge with a default.
func mergeAsk(op, topic, title string) contracts.CreateAskRequest {
	req := synthAsk(op, topic, title, "b")
	req.Action, req.ConfirmNew = "merge", true
	return req
}

func (f *childFixture) held(sessionID string, project *string) []string {
	f.x.t.Helper()
	gs, err := f.x.h.grantsHeldBy(context.Background(), f.x.db, sessionID, project)
	if err != nil {
		f.x.t.Fatal(err)
	}
	var out []string
	for _, g := range gs {
		out = append(out, g.Action)
	}
	return out
}

func (f *childFixture) inProject(project string, ids ...string) {
	f.x.t.Helper()
	for _, id := range ids {
		if _, err := f.x.db.Exec(`UPDATE sessions SET project_id=? WHERE session_id=?`, project, id); err != nil {
			f.x.t.Fatal(err)
		}
	}
}

func (f *childFixture) answer(op, askID, option, text string) contracts.Ask {
	f.x.t.Helper()
	res, err := f.x.h.AnswerAsks(as(owner), contracts.AnswerAsksRequest{Mutation: contracts.Mutation{OperationID: op},
		Answers: []contracts.AnswerAskItem{{AskID: askID, OptionID: option, Text: text}}})
	if err != nil || len(res.Results) != 1 || res.Results[0].Error != nil {
		f.x.t.Fatalf("answer %s: %+v %v", op, res, err)
	}
	return *res.Results[0].Ask
}

func (f *childFixture) grants(where string, args ...any) []contracts.StandingGrant {
	f.x.t.Helper()
	gs, err := f.x.h.queryGrants(context.Background(), f.x.db, where, args...)
	if err != nil {
		f.x.t.Fatal(err)
	}
	return gs
}

// Only the owner creates grants (setup review PR #12, decision A); the
// deny-list is never grantable; expiry defaults to 7 days, at most 30.
func TestGrantCreateOwnerOnlyDenyListAndExpiry(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid, cid := f.parent.Session.SessionID, f.child.Session.SessionID
	f.inProject(synthProject, pid, cid)

	for _, p := range []contracts.Principal{sesPrincipal(pid), sesPrincipal(cid)} {
		if _, err := x.h.CreateGrant(as(p), projectGrant("op_synth-c-"+p.ID)); !contracts.IsCode(err, contracts.ErrForbidden) || !strings.Contains(err.Error(), "propose") {
			t.Fatalf("session %s created a grant: %v", p.ID, err)
		}
		if _, err := x.h.CreateGrant(as(p), treeGrant("op_synth-t-"+p.ID, p.ID)); !contracts.IsCode(err, contracts.ErrForbidden) {
			t.Fatalf("session %s created a tree grant: %v", p.ID, err)
		}
	}
	if n := len(f.grants(`1=1`)); n != 0 {
		t.Fatalf("grants stored: %d", n)
	}

	g := f.grant(projectGrant("op_synth-c1"))
	if g.Scope != contracts.GrantScopeProject || *g.ProjectID != synthProject || g.Option != "a" || g.RecordedBy.Kind != contracts.PrincipalOwner ||
		g.AskID != "" || g.ProposedBy != nil || !g.ExpiresAt.Equal(x.clock.Now().Add(7*24*time.Hour)) {
		t.Fatalf("owner grant: %+v", g)
	}
	if again := f.grant(projectGrant("op_synth-c1")); again.GrantID != g.GrantID {
		t.Fatalf("retry made a new grant")
	}
	tg := f.grant(treeGrant("op_synth-c2", cid))
	if tg.Scope != contracts.GrantScopeSession || tg.SessionID != cid || *tg.ProjectID != synthProject {
		t.Fatalf("tree grant: %+v", tg)
	}

	bad := func(op string, mod func(*contracts.CreateGrantRequest), code contracts.ErrorCode) {
		t.Helper()
		req := projectGrant(op)
		mod(&req)
		if _, err := x.h.CreateGrant(as(owner), req); !contracts.IsCode(err, code) {
			t.Errorf("%s: %v, want %s", op, err, code)
		}
	}
	bad("op_synth-b1", func(r *contracts.CreateGrantRequest) { r.Option = " " }, contracts.ErrInvalid)
	bad("op_synth-b2", func(r *contracts.CreateGrantRequest) { r.Action = " " }, contracts.ErrInvalid)
	bad("op_synth-b3", func(r *contracts.CreateGrantRequest) { at := x.clock.Now().Add(31 * 24 * time.Hour); r.ExpiresAt = &at }, contracts.ErrInvalid)
	bad("op_synth-b4", func(r *contracts.CreateGrantRequest) { at := x.clock.Now().Add(-time.Minute); r.ExpiresAt = &at }, contracts.ErrInvalid)
	bad("op_synth-b5", func(r *contracts.CreateGrantRequest) { r.ProjectID = nil }, contracts.ErrInvalid)
	for i, action := range []string{"deploy-prod", "prod-deploy", "deploy to production", "release", "publish-npm", "drop-table", "delete-data",
		"force-push", "rewrite-history", "pay-invoice", "purchase", "rotate-credentials", "change-account", "deployprod"} {
		bad("op_synth-d"+string(rune('a'+i)), func(r *contracts.CreateGrantRequest) { r.Action = action }, contracts.ErrForbidden)
	}
	bad("op_synth-d-opt", func(r *contracts.CreateGrantRequest) { r.Option = "force push" }, contracts.ErrForbidden)
	at := x.clock.Now().Add(30 * 24 * time.Hour)
	req := projectGrant("op_synth-c3")
	req.ExpiresAt = &at
	if g := f.grant(req); !g.ExpiresAt.Equal(at) {
		t.Fatalf("30-day grant: %+v", g)
	}
}

// Grants answer only an exact match (decisions B–E): explicit action, the
// grant's option, a default, not irreversible, not deny-listed, not a
// reopen or a recently decided/open topic, unexpired, the asker's own
// project; a child never broadens one.
func TestGrantAnswersOnlyExactMatches(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid, cid := f.parent.Session.SessionID, f.child.Session.SessionID
	gc := f.startUnder(cid, "op_synth-gc").Session.SessionID
	parentP := sesPrincipal(pid)
	g := f.grant(treeGrant("op_synth-g0", pid))

	r := f.ask(parentP, mergeAsk("op_synth-a1", "merge-pr-12", "Merge synthetic PR 12?"))
	if r.Status != contracts.AskGranted || r.Ask.GrantID != g.GrantID || r.Ask.Answer.OptionID != "a" || r.Ask.Answer.By.ID != "grants" || r.Ask.MessageID != "" {
		t.Fatalf("exact match: %+v", r)
	}
	if !strings.Contains(r.Ask.Answer.Text, `set by the owner: "synthetic owner: merge when reviewed and green"`) || strings.Contains(r.Ask.Answer.Text, "owner said") {
		t.Fatalf("answer text: %s", r.Ask.Answer.Text)
	}
	if f.ownerInbox() != 0 {
		t.Fatalf("granted ask mailed the owner")
	}
	dec := x.envelopes(`recipient_kind='session' AND recipient_id=? AND sender_kind='system'`, pid)
	if len(dec) != 1 || !strings.Contains(dec[0].Body.Text, "[pyre ask granted] Merge synthetic PR 12?") || !strings.Contains(dec[0].Body.Text, "go ahead with a — Ship") {
		t.Fatalf("decision: %+v", dec)
	}
	if gs := f.grants(`grant_id=?`, g.GrantID); gs[0].UsedCount != 1 || gs[0].LastUsedAt == nil {
		t.Fatalf("use not counted: %+v", gs[0])
	}
	// A retry is the same answer.
	if rr := f.ask(parentP, mergeAsk("op_synth-a1", "merge-pr-12", "Merge synthetic PR 12?")); rr.Ask.AskID != r.Ask.AskID {
		t.Fatalf("retry: %+v", rr)
	}

	// Not answered: each case reaches the owner as an ordinary ask.
	cases := []struct {
		name string
		mod  func(*contracts.CreateAskRequest)
	}{
		{"no action (no topic-key guess)", func(r *contracts.CreateAskRequest) { r.Action = "" }},
		{"another action", func(r *contracts.CreateAskRequest) { r.Action = "rebase" }},
		{"no default", func(r *contracts.CreateAskRequest) { r.DefaultOption = "" }},
		{"irreversible", func(r *contracts.CreateAskRequest) { r.Irreversible = true }},
		{"option missing", func(r *contracts.CreateAskRequest) {
			r.Options = []contracts.AskOption{{ID: "x", Label: "Squash it", Detail: "synthetic"}, {ID: "b", Label: "Wait for the synthetic soak", Detail: "one more day"}}
		}},
		{"deny-listed title", func(r *contracts.CreateAskRequest) { r.Title = "Merge and deploy to production now?" }},
		{"deny-listed topic", func(r *contracts.CreateAskRequest) { r.TopicKey += "-drop-table" }},
		{"reopen", func(r *contracts.CreateAskRequest) { r.Reopen = true }},
		{"another project", func(r *contracts.CreateAskRequest) { other := "prj_synth-other"; r.ProjectID = &other }},
	}
	for i, c := range cases {
		req := mergeAsk("op_synth-n"+string(rune('a'+i)), "merge-case-"+string(rune('a'+i)), "Merge synthetic case "+string(rune('A'+i))+"?")
		c.mod(&req)
		if r := f.ask(parentP, req); r.Status != contracts.AskCreated || r.Ask.GrantID != "" {
			t.Errorf("%s: %s %+v", c.name, r.Status, r.Ask)
		}
	}
	// The option is matched by label too.
	byLabel := f.grant(contracts.CreateGrantRequest{Mutation: contracts.Mutation{OperationID: "op_synth-g1"}, Scope: contracts.GrantScopeSession, SessionID: pid,
		Action: "land", Option: "wait for the synthetic SOAK"})
	req := mergeAsk("op_synth-l1", "land-pr-3", "Land synthetic PR 3?")
	req.Action = "land"
	if r := f.ask(parentP, req); r.Status != contracts.AskGranted || r.Ask.GrantID != byLabel.GrantID || r.Ask.Answer.OptionID != "b" {
		t.Fatalf("by label: %+v", r)
	}

	// Decided or open topic first (decision C): the owner's decision stands.
	open := f.ask(parentP, func() contracts.CreateAskRequest {
		r := mergeAsk("op_synth-o1", "merge-pr-20", "Merge synthetic PR 20?")
		r.Action = ""
		return r
	}())
	if r := f.ask(parentP, mergeAsk("op_synth-o2", "merge-pr-20", "Merge synthetic PR 20?")); r.Status != contracts.AskMerged || r.Ask.AskID != open.Ask.AskID {
		t.Fatalf("open topic: %+v", r)
	}
	f.answer("op_synth-o3", open.Ask.AskID, "b", "synthetic: not this one")
	if r := f.ask(parentP, mergeAsk("op_synth-o4", "merge-pr-20", "Merge synthetic PR 20?")); r.Status != contracts.AskAlreadyDecided || r.Ask.GrantID != "" {
		t.Fatalf("decided topic: %+v", r)
	}
	if r := f.ask(sesPrincipal(cid), mergeAsk("op_synth-o5", "merge-pr-20", "Merge synthetic PR 20?")); r.Status != contracts.AskRedirected {
		t.Fatalf("child on a decided topic: %+v", r)
	}

	// Children inherit the parent's tree grant; a child's own grant does
	// not cover its parent (no broadening).
	if r := f.ask(sesPrincipal(gc), mergeAsk("op_synth-c1", "merge-pr-30", "Merge synthetic PR 30?")); r.Status != contracts.AskGranted || r.Ask.SessionID != gc {
		t.Fatalf("grandchild inherits: %+v", r)
	}
	child := f.grant(contracts.CreateGrantRequest{Mutation: contracts.Mutation{OperationID: "op_synth-g2"}, Scope: contracts.GrantScopeSession, SessionID: cid, Action: "tag", Option: "a"})
	tagAsk := mergeAsk("op_synth-c2", "tag-v1", "Tag synthetic v1?")
	tagAsk.Action = "tag"
	if r := f.ask(parentP, tagAsk); r.Status != contracts.AskCreated {
		t.Fatalf("a child's grant answered its parent: %+v", r)
	}
	tagAsk.OperationID, tagAsk.TopicKey = "op_synth-c3", "tag-v2"
	if r := f.ask(sesPrincipal(gc), tagAsk); r.Status != contracts.AskGranted || r.Ask.GrantID != child.GrantID {
		t.Fatalf("grandchild under the child's grant: %+v", r)
	}
	if _, err := x.h.CreateGrant(as(sesPrincipal(cid)), treeGrant("op_synth-c4", pid)); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("child broadened a grant: %v", err)
	}

	// Expired (decision E): asked as usual.
	x.clock.Add(7*24*time.Hour + time.Minute)
	if got := f.held(pid, nil); len(got) != 0 {
		t.Fatalf("expired grants still held: %v", got)
	}
	if r := f.ask(parentP, mergeAsk("op_synth-e1", "merge-pr-40", "Merge synthetic PR 40?")); r.Status != contracts.AskCreated {
		t.Fatalf("expired grant answered: %+v", r)
	}
}

// A caller-supplied project_id never selects another project's grants
// (decision D).
func TestGrantMatchingUsesTheAskersOwnProject(t *testing.T) {
	f := newChildFixture(t, nil)
	pid := f.parent.Session.SessionID
	other := f.x.start(owner, "op_synth-other", nodeMac).Session.SessionID
	f.inProject(synthProject, pid)
	f.inProject("prj_synth-other", other)
	f.grant(projectGrant("op_synth-p0"))

	req := mergeAsk("op_synth-p1", "merge-x", "Merge synthetic X?")
	pr := synthProject
	req.ProjectID = &pr // claims the granted project
	if r := f.ask(sesPrincipal(other), req); r.Status == contracts.AskGranted {
		t.Fatalf("another project's session borrowed the grant: %+v", r)
	}
	if r := f.ask(sesPrincipal(pid), mergeAsk("op_synth-p2", "merge-y", "Merge synthetic Y?")); r.Status != contracts.AskGranted {
		t.Fatalf("the project's own session: %+v", r)
	}
}

// grants.list is owner-or-same-project/tree (fix 11); revoke is the owner
// or a session the grant holds for.
func TestListAndRevokeGrantsAuth(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid, cid := f.parent.Session.SessionID, f.child.Session.SessionID
	stranger := x.start(owner, "op_synth-stranger", nodeMac).Session.SessionID
	f.inProject(synthProject, pid, cid)
	f.inProject("prj_synth-other", stranger)
	pg := f.grant(projectGrant("op_synth-l0"))
	tg := f.grant(treeGrant("op_synth-l1", cid))
	pr, other := synthProject, "prj_synth-other"

	list := func(p contracts.Principal, req contracts.ListGrantsRequest) ([]contracts.StandingGrant, error) {
		r, err := x.h.ListGrants(as(p), req)
		return r.Grants, err
	}
	if gs, err := list(owner, contracts.ListGrantsRequest{}); err != nil || len(gs) != 2 {
		t.Fatalf("owner: %v %v", gs, err)
	}
	if gs, err := list(sesPrincipal(pid), contracts.ListGrantsRequest{}); err != nil || len(gs) != 1 || gs[0].GrantID != pg.GrantID {
		t.Fatalf("parent default (held by itself): %v %v", gs, err)
	}
	if gs, err := list(sesPrincipal(pid), contracts.ListGrantsRequest{ProjectID: &pr}); err != nil || len(gs) != 2 {
		t.Fatalf("parent, its project: %v %v", gs, err)
	}
	if gs, err := list(sesPrincipal(pid), contracts.ListGrantsRequest{SessionID: cid}); err != nil || len(gs) != 2 {
		t.Fatalf("parent, its child: %v %v", gs, err)
	}
	for name, req := range map[string]contracts.ListGrantsRequest{"other project": {ProjectID: &pr}, "other session": {SessionID: pid}} {
		if _, err := list(sesPrincipal(stranger), req); !contracts.IsCode(err, contracts.ErrForbidden) {
			t.Errorf("stranger, %s: %v", name, err)
		}
	}
	if _, err := list(sesPrincipal(pid), contracts.ListGrantsRequest{ProjectID: &other}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Errorf("parent listed another project: %v", err)
	}
	if gs, err := list(sesPrincipal(stranger), contracts.ListGrantsRequest{}); err != nil || len(gs) != 0 {
		t.Fatalf("stranger, own: %v %v", gs, err)
	}

	revoke := func(p contracts.Principal, op, id string) error {
		_, err := x.h.RevokeGrant(as(p), contracts.RevokeGrantRequest{Mutation: contracts.Mutation{OperationID: op}, GrantID: id, Reason: "synthetic"})
		return err
	}
	if err := revoke(sesPrincipal(stranger), "op_synth-r1", pg.GrantID); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("stranger revoked: %v", err)
	}
	if err := revoke(sesPrincipal(pid), "op_synth-r2", tg.GrantID); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("parent revoked its child's tree grant (it does not hold for the parent): %v", err)
	}
	if err := revoke(sesPrincipal(cid), "op_synth-r3", pg.GrantID); err != nil {
		t.Fatalf("child revoked its project's grant: %v", err)
	}
	if err := revoke(owner, "op_synth-r4", pg.GrantID); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("second revoke: %v", err)
	}
	if err := revoke(owner, "op_synth-r5", tg.GrantID); err != nil {
		t.Fatal(err)
	}
	if gs, _ := list(owner, contracts.ListGrantsRequest{}); len(gs) != 0 {
		t.Fatalf("active after revokes: %v", gs)
	}
	if gs, _ := list(owner, contracts.ListGrantsRequest{IncludeRevoked: true}); len(gs) != 2 {
		t.Fatalf("history: %v", gs)
	}
	page, err := x.h.ListEvents(as(owner), contracts.ListEventsRequest{Kinds: []contracts.EventKind{contracts.EvStandingGrantChanged}, Limit: 100})
	if err != nil || len(page.Events) != 4 {
		t.Fatalf("grant events: %d %v", len(page.Events), err)
	}
}

func proposeGrant(op string, g contracts.GrantProposal) contracts.CreateAskRequest {
	return contracts.CreateAskRequest{Mutation: contracts.Mutation{OperationID: op}, Context: "Synthetic reason: the owner merged the last five reviewed PRs the same way.",
		Proposal: &contracts.AskProposal{Kind: contracts.ProposalGrant, Grant: &g}}
}

// A session proposes; the owner's approve writes the grant with his own
// answer text and the ask ID; reject or no answer writes nothing
// (decision A).
func TestGrantProposalApproveRejectExpire(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid, cid := f.parent.Session.SessionID, f.child.Session.SessionID
	f.inProject(synthProject, pid, cid)
	parentP := sesPrincipal(pid)

	p := f.ask(parentP, proposeGrant("op_synth-p1", contracts.GrantProposal{Action: "Merge", Option: "a", Condition: "reviewed and synthetic CI green"}))
	a := p.Ask
	if p.Status != contracts.AskCreated || a.Proposal == nil || a.Proposal.State != contracts.AskProposalPending || a.DefaultOption != "" ||
		len(a.Options) != 2 || a.Options[0].ID != contracts.ProposalApprove || a.Options[1].ID != contracts.ProposalReject || a.Action != "" {
		t.Fatalf("proposal ask: %+v", p)
	}
	for _, want := range []string{"Proposed standing go-ahead", "Action: `merge`", `Picks option: "a"`, "every session in project " + synthProject,
		"Note (the session's words; Pyre never checks it): reviewed and synthetic CI green", "Why the session proposes it** (its words):\n\nSynthetic reason"} {
		if !strings.Contains(a.Context, want) {
			t.Errorf("context lacks %q:\n%s", want, a.Context)
		}
	}
	if a.Title != "Remember this go-ahead? merge → a" || f.ownerInbox() != 1 {
		t.Fatalf("title %q, inbox %d", a.Title, f.ownerInbox())
	}
	if got, _ := x.h.GetAsk(as(owner), contracts.GetRequest{ID: a.AskID}); got.Proposal == nil || got.Proposal.Grant.Action != "merge" {
		t.Fatalf("stored proposal: %+v", got.Proposal)
	}
	if n := len(f.grants(`1=1`)); n != 0 {
		t.Fatalf("a proposal recorded a grant: %d", n)
	}
	// A proposal never auto-approves and is never grant-answered.
	bad := proposeGrant("op_synth-p2", contracts.GrantProposal{Action: "tag", Option: "a"})
	bad.DefaultOption = contracts.ProposalApprove
	if _, err := x.h.CreateAsk(as(parentP), bad); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("approve default: %v", err)
	}
	if _, err := x.h.CreateAsk(as(parentP), proposeGrant("op_synth-p3", contracts.GrantProposal{Action: "deploy-prod", Option: "a"})); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("deny-listed proposal: %v", err)
	}
	if _, err := x.h.CreateAsk(as(parentP), proposeGrant("op_synth-p4", contracts.GrantProposal{Action: "tag", Option: "a", ExpiresInDays: 31})); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("31 days: %v", err)
	}
	if _, err := x.h.CreateAsk(as(owner), proposeGrant("op_synth-p5", contracts.GrantProposal{Action: "tag", Option: "a"})); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("owner proposal: %v", err)
	}
	// A child's proposal goes to its parent like any ask.
	if r := f.ask(sesPrincipal(cid), proposeGrant("op_synth-p6", contracts.GrantProposal{Action: "tag", Option: "a", Scope: contracts.GrantScopeSession})); r.Status != contracts.AskRedirected || f.ownerInbox() != 1 {
		t.Fatalf("child proposal: %+v (inbox %d)", r, f.ownerInbox())
	}
	if m := x.envelopes(`recipient_kind='session' AND recipient_id=? AND sender_id=?`, pid, cid); len(m) != 1 || !strings.Contains(m[0].Body.Text, "Remember this go-ahead? tag → a") {
		t.Fatalf("redirected proposal: %+v", m)
	}

	// Approve with words: the grant quotes them and cites the ask.
	done := f.answer("op_synth-p7", a.AskID, contracts.ProposalApprove, "yes — synthetic: merge them once green")
	if done.Proposal == nil || done.Proposal.State != contracts.AskProposalApplied || !contracts.ValidID(done.Proposal.ResultID, contracts.PrefixStandingGrant) {
		t.Fatalf("approved: %+v", done.Proposal)
	}
	gs := f.grants(`ask_id=?`, a.AskID)
	if len(gs) != 1 {
		t.Fatalf("grants for the proposal: %+v", gs)
	}
	g := gs[0]
	if g.GrantID != done.Proposal.ResultID || g.Quote != "yes — synthetic: merge them once green" || g.Action != "merge" || g.Option != "a" ||
		g.Scope != contracts.GrantScopeProject || *g.ProjectID != synthProject || g.RecordedBy.Kind != contracts.PrincipalOwner ||
		g.ProposedBy == nil || g.ProposedBy.ID != pid || g.Condition != "reviewed and synthetic CI green" || !g.ExpiresAt.Equal(x.clock.Now().Add(7*24*time.Hour)) {
		t.Fatalf("grant: %+v", g)
	}
	if strings.Contains(g.Quote, "Synthetic reason") {
		t.Fatalf("the session's text became the owner's quote: %q", g.Quote)
	}
	dec := x.envelopes(`recipient_kind='session' AND recipient_id=? AND sender_kind='owner'`, pid)
	if len(dec) == 0 || !strings.Contains(dec[len(dec)-1].Body.Text, "Pyre recorded standing grant "+g.GrantID) {
		t.Fatalf("decision message: %+v", dec)
	}
	// It now answers: the provenance is his words and the ask.
	r := f.ask(parentP, mergeAsk("op_synth-p8", "merge-pr-50", "Merge synthetic PR 50?"))
	if r.Status != contracts.AskGranted || !strings.Contains(r.Ask.Answer.Text, `owner: "yes — synthetic: merge them once green" (approved `+a.AskID+")") {
		t.Fatalf("granted by the approved grant: %+v", r.Ask)
	}

	// Approve without words: no quote; labeled as an approval.
	p2 := f.ask(parentP, proposeGrant("op_synth-p9", contracts.GrantProposal{Action: "tag", Option: "a", ExpiresInDays: 2}))
	f.answer("op_synth-p10", p2.Ask.AskID, contracts.ProposalApprove, "")
	g2 := f.grants(`ask_id=?`, p2.Ask.AskID)
	if len(g2) != 1 || g2[0].Quote != "" || grantProvenance(g2[0]) != "owner approved proposal "+p2.Ask.AskID+" (no words typed)" ||
		!g2[0].ExpiresAt.Equal(x.clock.Now().Add(2*24*time.Hour)) {
		t.Fatalf("click-only grant: %+v", g2)
	}

	// Reject: nothing.
	p3 := f.ask(parentP, proposeGrant("op_synth-p11", contracts.GrantProposal{Action: "rebase", Option: "a"}))
	if d := f.answer("op_synth-p12", p3.Ask.AskID, contracts.ProposalReject, "no"); d.Proposal.State != contracts.AskProposalRejected {
		t.Fatalf("rejected: %+v", d.Proposal)
	}
	// Free text only (no option) is not an approval either.
	p4 := f.ask(parentP, proposeGrant("op_synth-p13", contracts.GrantProposal{Action: "squash", Option: "a"}))
	f.answer("op_synth-p14", p4.Ask.AskID, "", "approve, I guess")
	// No answer by the deadline: expires, nothing.
	p5 := f.ask(parentP, proposeGrant("op_synth-p15", contracts.GrantProposal{Action: "cherry-pick", Option: "a"}))
	x.clock.Add(36 * time.Hour) // past the deadline (moved to the owner's morning)
	if err := x.h.RunMaintenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.h.GetAsk(as(owner), contracts.GetRequest{ID: p5.Ask.AskID}); got.State != contracts.AskExpired {
		t.Fatalf("proposal at deadline: %+v", got)
	}
	for _, id := range []string{p3.Ask.AskID, p4.Ask.AskID, p5.Ask.AskID} {
		if n := len(f.grants(`ask_id=?`, id)); n != 0 {
			t.Fatalf("grant written for %s", id)
		}
	}
}

// proposalDocs is a synthetic fact sheet store that records who writes.
type proposalDocs struct {
	contracts.DocumentAPI
	content  string
	exists   bool
	writers  []contracts.Principal
	failNext error
}

func (d *proposalDocs) GetDocument(ctx context.Context, req contracts.GetDocumentRequest) (contracts.DocumentWithRevision, error) {
	if !d.exists || req.ProjectID != synthProject || req.Slug != contracts.FactSheetSlug {
		return contracts.DocumentWithRevision{}, contracts.Errorf(contracts.ErrNotFound, "no such document")
	}
	return contracts.DocumentWithRevision{Document: contracts.Document{DocumentID: "doc_synthfacts"},
		Revision: contracts.DocumentRevision{RevisionID: "rev_synth-" + string(rune('0'+len(d.writers))), Content: d.content}}, nil
}

func (d *proposalDocs) CreateDocument(ctx context.Context, req contracts.CreateDocumentRequest) (contracts.DocumentWithRevision, error) {
	p, _ := contracts.PrincipalFrom(ctx)
	if d.failNext != nil {
		err := d.failNext
		d.failNext = nil
		return contracts.DocumentWithRevision{}, err
	}
	d.writers = append(d.writers, p)
	d.exists, d.content = true, req.Content
	return contracts.DocumentWithRevision{Revision: contracts.DocumentRevision{RevisionID: "rev_synth-created"}}, nil
}

func (d *proposalDocs) ProposeRevision(ctx context.Context, req contracts.ProposeRevisionRequest) (contracts.DocumentWithRevision, error) {
	p, _ := contracts.PrincipalFrom(ctx)
	d.writers = append(d.writers, p)
	d.content = req.Content
	return contracts.DocumentWithRevision{Revision: contracts.DocumentRevision{RevisionID: "rev_synth-appended"}}, nil
}

// An approved fact is appended to the fact sheet by the facts principal,
// quoting the owner only with his words; reject writes nothing; a failed
// write is retried by maintenance.
func TestFactProposalApproveAppendsToFactSheet(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid := f.parent.Session.SessionID
	f.inProject(synthProject, pid)
	docs := &proposalDocs{failNext: contracts.Errorf(contracts.ErrUnavailable, "synthetic outage")}
	x.h.cfg.Documents = docs
	propose := func(op, fact string) contracts.Ask {
		t.Helper()
		return *f.ask(sesPrincipal(pid), contracts.CreateAskRequest{Mutation: contracts.Mutation{OperationID: op}, Context: "Synthetic: the owner settled it in chat.",
			Proposal: &contracts.AskProposal{Kind: contracts.ProposalFact, Fact: fact}}).Ask
	}
	a := propose("op_synth-f1", "GPU provider is synthetic-fal")
	if a.Proposal.Fact != "GPU provider is synthetic-fal" || !strings.Contains(a.Context, "> GPU provider is synthetic-fal") || a.DefaultOption != "" {
		t.Fatalf("fact proposal: %+v", a)
	}
	// Approved during an outage: pending until maintenance writes it.
	if d := f.answer("op_synth-f2", a.AskID, contracts.ProposalApprove, "yes, synthetic-fal like I said"); d.Proposal.State != contracts.AskProposalApproved || docs.exists {
		t.Fatalf("approved in an outage: %+v", d.Proposal)
	}
	if err := x.h.RunMaintenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := x.h.GetAsk(as(owner), contracts.GetRequest{ID: a.AskID})
	if got.Proposal.State != contracts.AskProposalApplied || got.Proposal.ResultID != "rev_synth-created" {
		t.Fatalf("after maintenance: %+v", got.Proposal)
	}
	if !strings.HasPrefix(docs.content, "# "+contracts.FactSheetTitle+"\n") ||
		!strings.Contains(docs.content, `- GPU provider is synthetic-fal — owner: "yes, synthetic-fal like I said" (approved `+a.AskID+", ") {
		t.Fatalf("sheet:\n%s", docs.content)
	}
	// Click-only approval: the approved text, labeled as such.
	b := propose("op_synth-f3", "Deploys go through synthetic-ci only")
	f.answer("op_synth-f4", b.AskID, contracts.ProposalApprove, "")
	if !strings.Contains(docs.content, `- Deploys go through synthetic-ci only — owner approved proposal: "Deploys go through synthetic-ci only" (approved `+b.AskID+", ") {
		t.Fatalf("click-only line:\n%s", docs.content)
	}
	// Rejected: nothing written.
	c := propose("op_synth-f5", "The synthetic cache is optional")
	f.answer("op_synth-f6", c.AskID, contracts.ProposalReject, "")
	if strings.Contains(docs.content, "synthetic cache") || len(docs.writers) != 2 {
		t.Fatalf("rejected fact written:\n%s (%d writes)", docs.content, len(docs.writers))
	}
	for _, w := range docs.writers {
		if w != factsPrincipal {
			t.Fatalf("fact written as %+v, want the facts system principal", w)
		}
	}
	// Re-applying is a no-op (the sheet already cites the ask).
	if err := x.h.setProposalTx(context.Background(), x.db, b.AskID, contracts.AskProposalApproved, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := x.h.applyFact(context.Background(), b.AskID); err != nil || len(docs.writers) != 2 {
		t.Fatalf("re-apply: %v (%d writes)", err, len(docs.writers))
	}
}

func batchAsk(op, topic, def string) contracts.CreateAskRequest {
	req := synthAsk(op, topic, "Approve the synthetic merges?", def)
	req.Items = []contracts.AskItem{
		{ID: "pr-1", Label: "Synthetic PR 1", Detail: "docs only, CI green"},
		{ID: "pr-2", Label: "Synthetic PR 2", Detail: "schema change", URL: "https://example.invalid/pr/2"},
		{ID: "pr-3", Label: "Synthetic PR 3"},
	}
	req.ConfirmNew = true
	return req
}

func TestBatchedAskPerItemAnswersAndDefault(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid := f.parent.Session.SessionID
	parentP := sesPrincipal(pid)

	// Validation.
	bad := batchAsk("op_synth-v1", "synthetic-batch", "")
	bad.Items[2].ID = "PR-1"
	if _, err := x.h.CreateAsk(as(parentP), bad); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("duplicate item id: %v", err)
	}
	bad = batchAsk("op_synth-v2", "synthetic-batch", "")
	bad.Items[0].Label = ""
	if _, err := x.h.CreateAsk(as(parentP), bad); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("no label: %v", err)
	}
	bad = batchAsk("op_synth-v3", "synthetic-batch", "")
	for len(bad.Items) <= contracts.AskMaxItems {
		bad.Items = append(bad.Items, contracts.AskItem{ID: "x" + string(rune('a'+len(bad.Items)%26)) + strings.Repeat("y", len(bad.Items)/26), Label: "synthetic"})
	}
	if _, err := x.h.CreateAsk(as(parentP), bad); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("too many items: %v", err)
	}

	a := f.ask(parentP, batchAsk("op_synth-b1", "synthetic-batch", "")).Ask
	if len(a.Items) != 3 || a.Items[1].URL != "https://example.invalid/pr/2" {
		t.Fatalf("stored items: %+v", a.Items)
	}
	om := x.envelopes(`message_id=?`, a.MessageID)
	for _, want := range []string{"Items (3;", "- [pr-1] Synthetic PR 1 — docs only, CI green", "- [pr-2] Synthetic PR 2 — schema change <https://example.invalid/pr/2>", "- [pr-3] Synthetic PR 3"} {
		if len(om) != 1 || !strings.Contains(om[0].Body.Text, want) {
			t.Fatalf("owner message lacks %q: %+v", want, om)
		}
	}
	answer := func(op string, it contracts.AnswerAskItem) contracts.AnswerAskResult {
		t.Helper()
		res, err := x.h.AnswerAsks(as(owner), contracts.AnswerAsksRequest{Mutation: contracts.Mutation{OperationID: op}, Answers: []contracts.AnswerAskItem{it}})
		if err != nil || len(res.Results) != 1 {
			t.Fatalf("answer %s: %+v %v", op, res, err)
		}
		return res.Results[0]
	}
	// Every item must be answered when no option covers the rest.
	if r := answer("op_synth-b2", contracts.AnswerAskItem{AskID: a.AskID, Items: map[string]string{"pr-1": "a"}}); r.Error == nil || r.Error.Code != contracts.ErrInvalid || !strings.Contains(r.Error.Message, "pr-2, pr-3") {
		t.Fatalf("partial: %+v", r)
	}
	if r := answer("op_synth-b3", contracts.AnswerAskItem{AskID: a.AskID, OptionID: "a", Items: map[string]string{"pr-9": "a"}}); r.Error == nil || r.Error.Code != contracts.ErrInvalid {
		t.Fatalf("unknown item: %+v", r)
	}
	if r := answer("op_synth-b4", contracts.AnswerAskItem{AskID: a.AskID, OptionID: "a", Items: map[string]string{"pr-1": "zz"}}); r.Error == nil || r.Error.Code != contracts.ErrInvalid {
		t.Fatalf("unknown option: %+v", r)
	}
	r := answer("op_synth-b5", contracts.AnswerAskItem{AskID: a.AskID, OptionID: "a", Items: map[string]string{"PR-2": "b"}})
	if r.Error != nil || r.Ask.Answer.OptionID != "a" || len(r.Ask.Answer.Items) != 1 || r.Ask.Answer.Items["pr-2"] != "b" {
		t.Fatalf("per item: %+v", r)
	}
	dec := x.envelopes(`recipient_kind='session' AND recipient_id=? AND in_reply_to=?`, pid, a.MessageID)
	for _, want := range []string{"owner chose a — Ship", "- [pr-1] Synthetic PR 1: a — Ship", "- [pr-2] Synthetic PR 2: b — Wait", "- [pr-3] Synthetic PR 3: a — Ship"} {
		if len(dec) != 1 || !strings.Contains(dec[0].Body.Text, want) {
			t.Fatalf("decision lacks %q: %+v", want, dec)
		}
	}
	// A plain ask takes no items.
	plain := f.ask(parentP, synthAsk("op_synth-b6", "synthetic-plain", "Rotate the synthetic key?", "")).Ask
	if r := answer("op_synth-b7", contracts.AnswerAskItem{AskID: plain.AskID, OptionID: "a", Items: map[string]string{"x": "a"}}); r.Error == nil || r.Error.Code != contracts.ErrInvalid {
		t.Fatalf("items on a plain ask: %+v", r)
	}

	// Held per-item answers carry their items through the flush.
	b := f.ask(parentP, batchAsk("op_synth-h1", "synthetic-batch-2", "b")).Ask
	held, err := x.h.HoldAnswer(as(owner), contracts.HoldAnswerRequest{Mutation: contracts.Mutation{OperationID: "op_synth-h2"},
		AnswerAskItem: contracts.AnswerAskItem{AskID: b.AskID, Items: map[string]string{"pr-1": "a", "pr-2": "b", "pr-3": "a"}}})
	if err != nil || len(held.Items) != 1 || held.Items[0].Items["pr-2"] != "b" {
		t.Fatalf("hold: %+v %v", held, err)
	}
	fl, err := x.h.FlushHeldAnswers(as(owner), contracts.FlushHeldAnswersRequest{Mutation: contracts.Mutation{OperationID: "op_synth-h3"}})
	if err != nil || len(fl.Results) != 1 || fl.Results[0].Error != nil || fl.Results[0].Ask.Answer.Items["pr-1"] != "a" || fl.Results[0].Ask.Answer.OptionID != "" {
		t.Fatalf("flush: %+v %v", fl, err)
	}

	// The default at the deadline applies to every item.
	c := f.ask(parentP, batchAsk("op_synth-d1", "synthetic-batch-3", "b")).Ask
	x.clock.Add(5 * time.Hour)
	if err := x.h.RunMaintenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := x.h.GetAsk(as(owner), contracts.GetRequest{ID: c.AskID})
	if got.State != contracts.AskDefaulted || got.Answer.OptionID != "b" {
		t.Fatalf("defaulted: %+v", got)
	}
	msgs := x.envelopes(`recipient_kind='session' AND recipient_id=? AND sender_kind='system'`, pid)
	if body := msgs[len(msgs)-1].Body.Text; !strings.Contains(body, "- [pr-1] Synthetic PR 1: b — Wait") || !strings.Contains(body, "- [pr-3] Synthetic PR 3: b — Wait") {
		t.Fatalf("default decision:\n%s", body)
	}
}

func TestGrantsFeatureAdvertised(t *testing.T) {
	x := newHarness(t)
	hr, err := x.h.Hello(as(owner), contracts.HelloRequest{Protocol: contracts.ProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range hr.Features {
		if f == contracts.FeatureGrants {
			return
		}
	}
	t.Fatalf("features: %v", hr.Features)
}

// A topic a grant answered is not replayed as "already decided": a repeat
// is re-checked against live grants, so after revoke it reaches the owner.
func TestRevokedGrantIsNotReplayedAsDecided(t *testing.T) {
	f := newChildFixture(t, nil)
	pid := f.parent.Session.SessionID
	parentP := sesPrincipal(pid)
	g := f.grant(treeGrant("op_synth-rv-g", pid))
	if r := f.ask(parentP, mergeAsk("op_synth-rv-1", "merge-pr-40", "Merge synthetic PR 40?")); r.Status != contracts.AskGranted {
		t.Fatalf("first: %+v", r)
	}
	// Still held: the same topic is granted again (not blocked by itself).
	if r := f.ask(parentP, mergeAsk("op_synth-rv-2", "merge-pr-40", "Merge synthetic PR 40?")); r.Status != contracts.AskGranted {
		t.Fatalf("repeat while held: %+v", r)
	}
	if _, err := f.x.h.RevokeGrant(as(owner), contracts.RevokeGrantRequest{Mutation: contracts.Mutation{OperationID: "op_synth-rv-revoke"}, GrantID: g.GrantID}); err != nil {
		t.Fatal(err)
	}
	r := f.ask(parentP, mergeAsk("op_synth-rv-3", "merge-pr-40", "Merge synthetic PR 40?"))
	if r.Status != contracts.AskCreated || f.ownerInbox() != 1 {
		t.Fatalf("after revoke the ask must reach the owner, got %s: %+v", r.Status, r.Ask)
	}
}

// Proposals that differ in scope, holder or expiry are separate asks; the
// same proposal again merges.
func TestGrantProposalsDifferingInScopeOrExpiryDoNotMerge(t *testing.T) {
	f := newChildFixture(t, nil)
	pid, cid := f.parent.Session.SessionID, f.child.Session.SessionID
	f.inProject(synthProject, pid, cid)
	parentP := sesPrincipal(pid)
	base := contracts.GrantProposal{Action: "merge", Option: "a"}
	week := f.ask(parentP, proposeGrant("op_synth-pm1", base))
	longer := base
	longer.ExpiresInDays = 30
	month := f.ask(parentP, proposeGrant("op_synth-pm2", longer))
	tree := base
	tree.Scope, tree.SessionID = contracts.GrantScopeSession, cid
	sub := f.ask(parentP, proposeGrant("op_synth-pm3", tree))
	again := f.ask(parentP, proposeGrant("op_synth-pm4", base))
	if week.Status != contracts.AskCreated || month.Status != contracts.AskCreated || sub.Status != contracts.AskCreated ||
		week.Ask.AskID == month.Ask.AskID || week.Ask.AskID == sub.Ask.AskID || again.Status != contracts.AskMerged || again.Ask.AskID != week.Ask.AskID {
		t.Fatalf("week %s %s, month %s %s, tree %s %s, again %s", week.Status, week.Ask.AskID, month.Status, month.Ask.AskID, sub.Status, sub.Ask.AskID, again.Status)
	}
}

// A child's proposal reaches its parent as a message saying only the owner
// can approve it and how to re-propose.
func TestChildProposalRedirectSaysReProposeToOwner(t *testing.T) {
	f := newChildFixture(t, nil)
	r := f.ask(sesPrincipal(f.child.Session.SessionID), proposeGrant("op_synth-cp", contracts.GrantProposal{Action: "merge", Option: "a"}))
	if r.Status != contracts.AskRedirected {
		t.Fatalf("status %s", r.Status)
	}
	m := f.x.msg(r.MessageID)
	if !strings.Contains(m.Envelope.Body.Text, "only the owner can approve it") || !strings.Contains(m.Envelope.Body.Text, "propose_grant") {
		t.Fatalf("redirect:\n%s", m.Envelope.Body.Text)
	}
}
