package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/autotitle"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

func (x *harness) audit(req contracts.ListAuditRequest) []contracts.AuditEntry {
	x.t.Helper()
	p, err := x.h.ListAudit(as(owner), req)
	if err != nil {
		x.t.Fatalf("ListAudit: %v", err)
	}
	return p.Items
}

// findAudit returns the newest entry with action (fails if none).
func findAudit(t *testing.T, es []contracts.AuditEntry, action string) contracts.AuditEntry {
	t.Helper()
	for _, e := range es {
		if e.Action == action {
			return e
		}
	}
	var got []string
	for _, e := range es {
		got = append(got, e.Action)
	}
	t.Fatalf("no %s entry; have %v", action, got)
	return contracts.AuditEntry{}
}

func countAudit(es []contracts.AuditEntry, action string) int {
	n := 0
	for _, e := range es {
		if e.Action == action {
			n++
		}
	}
	return n
}

func wantSummary(t *testing.T, e contracts.AuditEntry, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(e.Summary, p) {
			t.Errorf("%s summary %q lacks %q", e.Action, e.Summary, p)
		}
	}
}

func TestAuditFleetConnectionsAndUpgrades(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	x.connect(nodeMac, 0) // same state again: one entry
	es := x.audit(contracts.ListAuditRequest{Kinds: []contracts.AuditKind{contracts.AuditFleet}})
	if n := countAudit(es, contracts.AuditNodeConnected); n != 1 {
		t.Fatalf("connected entries = %d, want 1: %+v", n, es)
	}
	c := findAudit(t, es, contracts.AuditNodeConnected)
	if c.NodeID != nodeMac || c.NodeName != "synth-mac" || c.To != "0.1.0-synth" || c.Kind != contracts.AuditFleet {
		t.Fatalf("connected entry: %+v", c)
	}
	wantSummary(t, c, "connected (pyred 0.1.0-synth)")
	x.disconnect(nodeMac)

	// A new pyred version on the next hello is an upgrade.
	x.ch.setConnected(nodeMac, true)
	hello := helloFor(nodeMac, 4)
	hello.PyredVersion = "0.2.0-synth"
	if _, err := x.h.HandleHello(nodeCtx(nodeMac), nodeMac, hello); err != nil {
		t.Fatal(err)
	}
	es = x.audit(contracts.ListAuditRequest{NodeID: nodeMac})
	up := findAudit(t, es, contracts.AuditNodeUpgraded)
	if up.From != "0.1.0-synth" || up.To != "0.2.0-synth" || !up.Automatic {
		t.Fatalf("upgrade: %+v", up)
	}
	wantSummary(t, up, "pyred upgraded 0.1.0-synth → 0.2.0-synth")
	if countAudit(es, contracts.AuditNodeDisconnected) != 1 || countAudit(es, contracts.AuditNodeConnected) != 2 {
		t.Fatalf("connection transitions: %+v", es)
	}
	// Newest first.
	for i := 1; i < len(es); i++ {
		if es[i].AuditID > es[i-1].AuditID {
			t.Fatalf("not newest first: %d after %d", es[i].AuditID, es[i-1].AuditID)
		}
	}

	// A failed update reported by the node.
	x.report(nodeMac, contracts.ReportEvent, contracts.Event{Kind: contracts.EvUpdateResult, Object: contracts.ObjectRef{Kind: contracts.KindNode, ID: nodeMac},
		Payload: json.RawMessage(`{"tool":"pyred","version":"0.3.0-synth","outcome":"failed_reverted","reverted_to":"0.2.0-synth","detail":"health check failed"}`)})
	f := findAudit(t, x.audit(contracts.ListAuditRequest{}), contracts.AuditNodeUpdateFailed)
	wantSummary(t, f, "update to pyred 0.3.0-synth did not complete", "back on 0.2.0-synth", "health check failed")
	if f.NodeID != nodeMac {
		t.Fatalf("update failure node: %+v", f)
	}
}

func TestAuditHubVersionChange(t *testing.T) {
	x := newHarness(t)
	if n := countAudit(x.audit(contracts.ListAuditRequest{}), contracts.AuditHubUpgraded); n != 0 {
		t.Fatalf("first start recorded an upgrade")
	}
	x.opts.HubVersion = "9.9.9-synth"
	x.restart()
	e := findAudit(t, x.audit(contracts.ListAuditRequest{}), contracts.AuditHubUpgraded)
	if e.To != "9.9.9-synth" || e.From == "" {
		t.Fatalf("hub upgrade: %+v", e)
	}
	x.restart()
	if n := countAudit(x.audit(contracts.ListAuditRequest{}), contracts.AuditHubUpgraded); n != 1 {
		t.Fatalf("same version recorded again: %d", n)
	}
}

func TestAuditSessionLifecycle(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_audit-start", nodeMac)
	sid := v.Session.SessionID

	// A child started by the session itself.
	child, err := x.h.StartSession(as(sesPrincipal(sid)), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_audit-child"},
		NodeID: nodeMac, Harness: contracts.HarnessClaude, Cwd: "/home/synth/child", Title: "synthetic child"})
	if err != nil {
		t.Fatal(err)
	}
	es := x.audit(contracts.ListAuditRequest{SessionID: child.Session.Session.SessionID})
	cs := findAudit(t, es, contracts.AuditSessionStarted)
	if cs.Actor.Kind != "session" || cs.Actor.ID != sid || cs.Actor.Name != v.Session.Title {
		t.Fatalf("child start actor: %+v", cs.Actor)
	}
	wantSummary(t, cs, "started 'synthetic child' (claude) — by session '"+v.Session.Title+"'")

	// Automatic title from the node, then the owner's rename and archive.
	x.report(nodeMac, contracts.ReportEvent, contracts.Event{Kind: autotitle.EventTitleSuggested, Object: contracts.ObjectRef{Kind: contracts.KindSession, ID: sid},
		Payload: json.RawMessage(`{"session_id":"` + sid + `","run_id":"` + v.Run.RunID + `","title":"Synthetic perf work"}`)})
	title := "Credit perf"
	if _, err := x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_audit-rename"}, SessionID: sid, Title: &title}); err != nil {
		t.Fatal(err)
	}
	// A retried rename is one entry.
	if _, err := x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_audit-rename"}, SessionID: sid, Title: &title}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.h.InterruptSession(as(owner), contracts.SessionCommandRequest{Mutation: contracts.Mutation{OperationID: "op_audit-int"}, SessionID: sid}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.h.StopSession(as(phone), contracts.SessionCommandRequest{Mutation: contracts.Mutation{OperationID: "op_audit-stop"}, SessionID: sid, Reason: "done for today"}); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_audit-archive"}, SessionID: sid, Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	es = x.audit(contracts.ListAuditRequest{SessionID: sid})
	st := findAudit(t, es, contracts.AuditSessionStarted)
	if st.Actor.Kind != "owner" || st.NodeID != nodeMac || st.RunID != v.Run.RunID {
		t.Fatalf("start: %+v", st)
	}
	wantSummary(t, st, "started 'Credit perf' (claude) — by owner (synthetic)")
	at := findAudit(t, es, contracts.AuditSessionAutoTitled)
	if !at.Automatic || at.From != "work" || at.To != "Synthetic perf work" {
		t.Fatalf("auto title: %+v", at)
	}
	if n := countAudit(es, contracts.AuditSessionRenamed); n != 1 {
		t.Fatalf("renamed entries = %d", n)
	}
	rn := findAudit(t, es, contracts.AuditSessionRenamed)
	wantSummary(t, rn, "renamed 'Synthetic perf work' to 'Credit perf' — by owner")
	findAudit(t, es, contracts.AuditSessionInterrupted)
	stop := findAudit(t, es, contracts.AuditSessionStopped)
	wantSummary(t, stop, "stopped 'Credit perf' — by owner (phone (synthetic))", "done for today")
	findAudit(t, es, contracts.AuditSessionArchived)
	for _, e := range es {
		if e.Kind != contracts.AuditSession || e.SessionID != sid {
			t.Fatalf("session filter leaked: %+v", e)
		}
	}

	// Kind filter.
	for _, e := range x.audit(contracts.ListAuditRequest{Kinds: []contracts.AuditKind{contracts.AuditAccount}}) {
		if e.Kind != contracts.AuditAccount {
			t.Fatalf("kind filter leaked: %+v", e)
		}
	}
}

func TestAuditLeaseTakeover(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_s", nodeMac)
	if _, err := x.h.AcquireLease(as(phone), contracts.LeaseRequest{Mutation: contracts.Mutation{OperationID: "op_l1"}, RunID: v.Run.RunID}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.h.AcquireLease(as(owner), contracts.LeaseRequest{Mutation: contracts.Mutation{OperationID: "op_l2"}, RunID: v.Run.RunID, Takeover: true}); err != nil {
		t.Fatal(err)
	}
	e := findAudit(t, x.audit(contracts.ListAuditRequest{SessionID: v.Session.SessionID}), contracts.AuditSessionLeaseTakenOver)
	if e.From != phone.Name || e.Actor.Kind != "owner" {
		t.Fatalf("takeover: %+v", e)
	}
}

// The owner's question: "when did an automatic account switch happen?"
func TestAuditAccountChoiceSwitchAndCooldown(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_s", nodeMac)
	sid := v.Session.SessionID
	title := "Credit perf"
	if _, err := x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_t"}, SessionID: sid, Title: &title}); err != nil {
		t.Fatal(err)
	}
	mk := func(label string) string {
		a, err := x.h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: contracts.Mutation{OperationID: "op_acct-" + label}, Provider: contracts.ProviderAnthropic, Label: label})
		if err != nil {
			t.Fatal(err)
		}
		return a.AccountID
	}
	code, me := mk("code"), mk("me")
	obs := func(acct string, el contracts.Eligibility, weekly float64) {
		x.report(nodeMac, contracts.ReportAccountObs, contracts.AccountObservation{AccountID: acct, Provider: contracts.ProviderAnthropic, ObservedAt: x.clock.Now(),
			Source: "synthetic", Eligibility: el, Windows: []contracts.UsageWindow{{Name: "five_hour", UsedPercent: 10}, {Name: "seven_day", UsedPercent: weekly}}})
		x.clock.Add(time.Second)
	}
	obs(code, contracts.EligibleYes, 80)
	obs(me, contracts.EligibleYes, 0)
	x.report(nodeMac, contracts.ReportGrant, contracts.DeviceGrant{GrantID: "grant_synth-code", AccountID: code, NodeID: nodeMac, Harness: contracts.HarnessClaude,
		Kind: contracts.CredRotaCheckout, Mechanism: "rota checkout", State: contracts.GrantValid, Generation: 1, ObservedAt: x.clock.Now()})
	asg := contracts.AccountAssignment{AssignmentID: "asg_synth-1", AccountID: code, GrantID: "grant_synth-code", RunID: v.Run.RunID, SessionID: sid,
		Reason: "machine affinity: this machine's account (5h 10%, weekly 80%)", AssignedAt: x.clock.Now()}
	x.report(nodeMac, contracts.ReportAssignment, asg)
	x.report(nodeMac, contracts.ReportAssignment, asg) // resent: still one entry

	// The limit: the node reports the account exhausted, then its plan.
	obs(code, contracts.EligibleExhausted, 96)
	x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: sid, FromRunID: v.Run.RunID, FromAccountID: code, ToAccountID: me,
		Outcome: contracts.FallbackHandoff, NewRunID: "run_synth-next", Reason: "usage_limit on " + code + "; " + me + " eligible; linked successor with handoff", PlannedAt: x.clock.Now()})

	es := x.audit(contracts.ListAuditRequest{Kinds: []contracts.AuditKind{contracts.AuditAccount}})
	ch := findAudit(t, es, contracts.AuditAccountChosen)
	if countAudit(es, contracts.AuditAccountChosen) != 1 || ch.AccountID != code || ch.SessionID != sid || ch.NodeName != "synth-mac" {
		t.Fatalf("chosen: %+v", ch)
	}
	wantSummary(t, ch, "chose code (weekly 80%) for 'Credit perf'", "machine affinity")
	sw := findAudit(t, es, contracts.AuditAccountSwitched)
	if !sw.Automatic || sw.FromAccountID != code || sw.ToAccountID != me || sw.From != "code" || sw.To != "me" {
		t.Fatalf("switch: %+v", sw)
	}
	if sw.Summary != "switched 'Credit perf' from code (weekly 96%) to me (5h 10%) — limit reached" {
		t.Fatalf("switch summary: %q", sw.Summary)
	}
	cd := findAudit(t, es, contracts.AuditAccountCooldown)
	wantSummary(t, cd, "code (weekly 96%) reached its limit")
	findAudit(t, es, contracts.AuditAccountLoginAdded)

	// Account filter finds the switch from either side.
	if n := countAudit(x.audit(contracts.ListAuditRequest{AccountID: me}), contracts.AuditAccountSwitched); n != 1 {
		t.Fatalf("switch by to-account: %d", n)
	}
	// Resent plan: one entry.
	x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: sid, FromRunID: v.Run.RunID, FromAccountID: code, ToAccountID: me, Outcome: contracts.FallbackHandoff})
	if n := countAudit(x.audit(contracts.ListAuditRequest{}), contracts.AuditAccountSwitched); n != 1 {
		t.Fatalf("switch entries after resend = %d", n)
	}

	// Usable again; the login expires and comes back.
	obs(code, contracts.EligibleYes, 5)
	x.report(nodeMac, contracts.ReportGrant, contracts.DeviceGrant{GrantID: "grant_synth-code", AccountID: code, NodeID: nodeMac, Harness: contracts.HarnessClaude,
		Kind: contracts.CredRotaCheckout, State: contracts.GrantExpired, Generation: 1, Detail: "refresh rejected (synthetic)", ObservedAt: x.clock.Now()})
	x.report(nodeMac, contracts.ReportGrant, contracts.DeviceGrant{GrantID: "grant_synth-code", AccountID: code, NodeID: nodeMac, Harness: contracts.HarnessClaude,
		Kind: contracts.CredRotaCheckout, State: contracts.GrantValid, Generation: 2, ObservedAt: x.clock.Now()})
	es = x.audit(contracts.ListAuditRequest{AccountID: code})
	findAudit(t, es, contracts.AuditAccountCooldownEnded)
	ra := findAudit(t, es, contracts.AuditAccountReauthNeeded)
	wantSummary(t, ra, "code needs re-auth — refresh rejected (synthetic)")
	findAudit(t, es, contracts.AuditAccountReauthed)

	// No eligible account: waiting.
	x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: sid, FromRunID: "run_synth-next", FromAccountID: me, Outcome: contracts.FallbackWaiting,
		Reason: "no eligible anthropic subscription"})
	w := findAudit(t, x.audit(contracts.ListAuditRequest{}), contracts.AuditAccountWaiting)
	wantSummary(t, w, "'Credit perf' hit a limit on me", "waiting for a reset", "no eligible anthropic subscription")

	// Owner edits: rename + disable/enable.
	nl, off, on := "work", true, false
	if _, err := x.h.UpdateAccount(as(owner), contracts.UpdateAccountRequest{Mutation: contracts.Mutation{OperationID: "op_u1"}, AccountID: code, Label: &nl, Disabled: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.h.UpdateAccount(as(owner), contracts.UpdateAccountRequest{Mutation: contracts.Mutation{OperationID: "op_u2"}, AccountID: code, Disabled: &on}); err != nil {
		t.Fatal(err)
	}
	es = x.audit(contracts.ListAuditRequest{AccountID: code})
	wantSummary(t, findAudit(t, es, contracts.AuditAccountRenamed), "renamed account code to work — by owner")
	findAudit(t, es, contracts.AuditAccountDisabled)
	findAudit(t, es, contracts.AuditAccountEnabled)
	// Labels are resolved when listed: the old switch now names "work".
	sw = findAudit(t, x.audit(contracts.ListAuditRequest{}), contracts.AuditAccountSwitched)
	if sw.From != "work" {
		t.Fatalf("switch from label after rename: %+v", sw)
	}
	added := findAudit(t, x.audit(contracts.ListAuditRequest{AccountID: me}), contracts.AuditAccountAdded)
	if added.Actor.Kind != "owner" {
		t.Fatalf("added: %+v", added)
	}
}

func TestAuditTokensNeverHoldSecrets(t *testing.T) {
	h, _ := newTokenHub(t)
	a, err := h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: tokOp(), Provider: contracts.ProviderAnthropic, Label: "synth-token"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := h.SetAccountToken(as(owner), contracts.SetAccountTokenRequest{Mutation: tokOp(), AccountID: a.AccountID, Token: synthHubToken}); err != nil {
			t.Fatal(err)
		}
	}
	node := contracts.NewID(contracts.PrefixNode)
	for i := 0; i < 3; i++ { // nodes re-fetch periodically: one entry per token version
		if _, err := h.FetchAccountToken(nodeCtx(node), contracts.FetchAccountTokenRequest{AccountID: a.AccountID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.RemoveAccountToken(as(owner), contracts.RemoveAccountTokenRequest{Mutation: tokOp(), AccountID: a.AccountID}); err != nil {
		t.Fatal(err)
	}
	p, err := h.ListAudit(as(owner), contracts.ListAuditRequest{AccountID: a.AccountID})
	if err != nil {
		t.Fatal(err)
	}
	for action, want := range map[string]int{contracts.AuditAccountTokenAdded: 1, contracts.AuditAccountTokenReplaced: 1,
		contracts.AuditAccountTokenSynced: 1, contracts.AuditAccountTokenRemoved: 1} {
		if n := countAudit(p.Items, action); n != want {
			t.Errorf("%s entries = %d, want %d", action, n, want)
		}
	}
	wantSummary(t, findAudit(t, p.Items, contracts.AuditAccountTokenAdded), "token added for synth-token — by owner")
	// The token never reaches the audit table or the listing.
	var dump string
	if err := h.db.QueryRow(`SELECT COALESCE(group_concat(COALESCE(reason,'')||COALESCE(from_val,'')||COALESCE(to_val,'')||COALESCE(dedupe,''),'|'),'') FROM audit_log`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	if strings.Contains(dump, "synthetic-hub-token") || strings.Contains(string(b), "synthetic-hub-token") {
		t.Fatal("audit log holds the token")
	}
}

func TestAuditNodeAutomationAndEventHook(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_s", nodeMac)
	auto := func(action string) {
		b, _ := json.Marshal(contracts.NodeAutomation{RunID: v.Run.RunID, SessionID: v.Session.SessionID, Harness: contracts.HarnessClaude, Action: action, Detail: "/home/synth/work"})
		x.report(nodeMac, contracts.ReportEvent, contracts.Event{Kind: contracts.EvNodeAutomation, Object: contracts.ObjectRef{Kind: contracts.KindRun, ID: v.Run.RunID}, Payload: b})
	}
	auto(contracts.AutomationTrustAccepted)
	auto(contracts.AutomationTrustAccepted) // reported twice: one entry
	auto(contracts.AutomationExitConfirmed)
	auto("something_else") // unknown: ignored
	// Another node cannot claim this run's automation.
	x.connect(nodeLin, 0)
	b, _ := json.Marshal(contracts.NodeAutomation{RunID: v.Run.RunID, Action: contracts.AutomationExitConfirmed})
	x.report(nodeLin, contracts.ReportEvent, contracts.Event{Kind: contracts.EvNodeAutomation, Payload: b})

	es := x.audit(contracts.ListAuditRequest{SessionID: v.Session.SessionID})
	if n := countAudit(es, contracts.AuditNodeTrustAccepted); n != 1 {
		t.Fatalf("trust entries = %d", n)
	}
	tr := findAudit(t, es, contracts.AuditNodeTrustAccepted)
	if !tr.Automatic || tr.NodeID != nodeMac || tr.Kind != contracts.AuditFleet {
		t.Fatalf("trust: %+v", tr)
	}
	wantSummary(t, tr, "answered the folder-trust prompt for 'work' (/home/synth/work, owner-trusted)")
	if n := countAudit(es, contracts.AuditNodeExitConfirmed); n != 1 {
		t.Fatalf("exit entries = %d", n)
	}

	// Events appended by other domains (fleet enrollment) reach the log
	// through the append hook, with the owner as actor.
	ctx := as(owner)
	err := sqlitex.Tx(context.Background(), x.db, func(tx *sql.Tx) error {
		_, err := x.h.EventLog().AppendTx(ctx, tx, contracts.Event{Kind: contracts.EvNodeEnrolled, Object: contracts.ObjectRef{Kind: contracts.KindNode, ID: "node_synth-new"}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	en := findAudit(t, x.audit(contracts.ListAuditRequest{NodeID: "node_synth-new"}), contracts.AuditNodeEnrolled)
	if en.Actor.Kind != "owner" {
		t.Fatalf("enrolled: %+v", en)
	}
}

func TestAuditPagingSinceAndRetention(t *testing.T) {
	x := newHarness(t)
	for i := 0; i < 7; i++ {
		x.connect(nodeMac, 0)
		x.disconnect(nodeMac)
		x.clock.Add(time.Hour)
	}
	all := x.audit(contracts.ListAuditRequest{Limit: 500})
	if len(all) != 14 {
		t.Fatalf("entries = %d, want 14", len(all))
	}
	// Paging: 5 per page, no gaps or repeats.
	var got []int64
	req := contracts.ListAuditRequest{PageRequest: contracts.PageRequest{Limit: 5}}
	for i := 0; i < 10; i++ {
		p, err := x.h.ListAudit(as(owner), req)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range p.Items {
			got = append(got, e.AuditID)
		}
		if p.NextCursor == "" {
			break
		}
		req.Cursor = p.NextCursor
	}
	if len(got) != 14 || got[0] != all[0].AuditID || got[13] != all[13].AuditID {
		t.Fatalf("paged ids %v", got)
	}
	since := x.clock.Now().Add(-150 * time.Minute)
	if n := len(x.audit(contracts.ListAuditRequest{Since: &since})); n != 4 {
		t.Fatalf("since: %d entries, want 4", n)
	}
	if _, err := x.h.ListAudit(as(owner), contracts.ListAuditRequest{Kinds: []contracts.AuditKind{"bogus"}}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("bad kind: %v", err)
	}
	if _, err := x.h.ListAudit(context.Background(), contracts.ListAuditRequest{}); !contracts.IsCode(err, contracts.ErrUnauthorized) {
		t.Fatalf("unauthenticated: %v", err)
	}

	// Retention: by age, then by count.
	x.h.opt.AuditRetention = &AuditRetention{MaxAge: 3 * time.Hour}
	if err := x.h.pruneAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(x.audit(contracts.ListAuditRequest{Limit: 500})); n != 6 {
		t.Fatalf("after age prune: %d, want 6", n)
	}
	x.h.opt.AuditRetention = &AuditRetention{MaxRows: 2}
	if err := x.h.pruneAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	left := x.audit(contracts.ListAuditRequest{Limit: 500})
	if len(left) != 2 || left[0].AuditID != all[0].AuditID {
		t.Fatalf("after row prune: %+v", left)
	}
}
