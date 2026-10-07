package hub

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestLeaseFixture05(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_1", nodeMac)
	runID := v.Run.RunID
	l1, err := x.h.AcquireLease(as(owner), contracts.LeaseRequest{Mutation: contracts.Mutation{OperationID: "op_synth-lease-1"}, RunID: runID})
	if err != nil || l1.Holder != owner || !l1.ExpiresAt.Equal(x.clock.Now().Add(120*time.Second)) {
		t.Fatalf("acquire: %v %+v", err, l1)
	}
	// Competing acquire: conflict naming the holder.
	_, err = x.h.AcquireLease(as(phone), contracts.LeaseRequest{Mutation: contracts.Mutation{OperationID: "op_synth-lease-2"}, RunID: runID})
	wantCode(t, err, contracts.ErrConflict)
	e := contracts.AsError(err)
	if e.Ref == nil || e.Ref.ID != l1.LeaseID || !strings.Contains(e.Message, "owner (synthetic)") || !strings.Contains(e.Message, "12:02Z") {
		t.Fatalf("conflict: %+v", e)
	}
	// Structured peer mail still goes out during the lease.
	m, err := x.h.SendMessage(as(sesPrincipal("ses_peer")), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_mail"}, Recipient: contracts.Address{Kind: contracts.AddrSession, ID: v.Session.SessionID}, Body: contracts.MessageBody{Text: "FYI build green"}})
	if err != nil || m.Status.Runtime != contracts.DeliveryQueued || len(x.ch.sentKinds(nodeMac, contracts.CmdDeliver)) != 1 {
		t.Fatalf("mail during lease: %v %+v", err, m.Status)
	}
	// Renewal by the holder keeps the lease ID.
	x.clock.Add(time.Minute)
	l1r, err := x.h.AcquireLease(as(owner), contracts.LeaseRequest{Mutation: contracts.Mutation{OperationID: "op_renew"}, RunID: runID})
	if err != nil || l1r.LeaseID != l1.LeaseID || !l1r.ExpiresAt.After(l1.ExpiresAt) {
		t.Fatalf("renew: %v %+v", err, l1r)
	}
	// Explicit takeover is recorded.
	l2, err := x.h.AcquireLease(as(phone), contracts.LeaseRequest{Mutation: contracts.Mutation{OperationID: "op_synth-lease-3"}, RunID: runID, Takeover: true})
	if err != nil || l2.TakeoverOf != l1.LeaseID || l2.Holder != phone {
		t.Fatalf("takeover: %v %+v", err, l2)
	}
	old, _ := x.h.getLease(t.Context(), x.db, l1.LeaseID)
	if old.ReleasedAt == nil {
		t.Fatal("taken-over lease not released")
	}
	view := x.session(v.Session.SessionID)
	if view.Run.Lease == nil || view.Run.Lease.LeaseID != l2.LeaseID {
		t.Fatalf("run view lease: %+v", view.Run.Lease)
	}
	if len(x.ch.sentKinds(nodeMac, contracts.CmdLeaseChanged)) < 3 {
		t.Fatal("lease changes not sent to node")
	}
	var payload string
	x.db.QueryRow(`SELECT payload FROM events WHERE kind='lease.changed' ORDER BY cursor DESC LIMIT 1`).Scan(&payload)
	var pl contracts.InputLease
	if json.Unmarshal([]byte(payload), &pl) != nil || pl.LeaseID != l2.LeaseID {
		t.Fatalf("lease.changed payload: %s", payload)
	}
	// Non-holder cannot release; expiry frees the run.
	_, err = x.h.ReleaseLease(as(sesPrincipal("ses_peer")), contracts.LeaseReleaseRequest{Mutation: contracts.Mutation{OperationID: "op_rel"}, LeaseID: l2.LeaseID})
	wantCode(t, err, contracts.ErrForbidden)
	x.clock.Add(3 * time.Minute)
	l3, err := x.h.AcquireLease(as(owner), contracts.LeaseRequest{Mutation: contracts.Mutation{OperationID: "op_after_expiry"}, RunID: runID})
	if err != nil || l3.TakeoverOf != "" {
		t.Fatalf("after expiry: %v %+v", err, l3)
	}
	rel, err := x.h.ReleaseLease(as(owner), contracts.LeaseReleaseRequest{Mutation: contracts.Mutation{OperationID: "op_rel2"}, LeaseID: l3.LeaseID})
	if err != nil || rel.ReleasedAt == nil {
		t.Fatalf("release: %v", err)
	}
}

func TestLeaseRaceExactlyOneHolder(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_1", nodeMac)
	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synth-" + string(rune('a'+i))}
			_, err := x.h.AcquireLease(as(p), contracts.LeaseRequest{Mutation: contracts.Mutation{OperationID: "op_race" + string(rune('a'+i))}, RunID: v.Run.RunID})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else if contracts.IsCode(err, contracts.ErrConflict) {
				conflicts++
			} else {
				t.Errorf("unexpected: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 || conflicts != n-1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	var active int
	x.db.QueryRow(`SELECT COUNT(*) FROM leases WHERE released_at IS NULL`).Scan(&active)
	if active != 1 {
		t.Fatalf("active leases %d", active)
	}
}

func approvalFor(run contracts.Run, id, req string, kind contracts.ApprovalKind, at time.Time) contracts.Approval {
	return contracts.Approval{ApprovalID: id, SessionID: run.SessionID, RunID: run.RunID, RequestID: req, Kind: kind, Title: "Run `make deploy-staging`?",
		Options: []contracts.ApprovalOption{{ID: "allow", Label: "Allow once", Kind: "allow"}, {ID: "deny", Label: "Deny", Kind: "deny"}}, CreatedAt: at, State: contracts.ApprovalPending}
}

func TestApprovalFixture09(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_1", nodeMac)
	run := *v.Run
	pend := approvalFor(run, "apr_synth-pending", "toolu_synth_01", contracts.ApprovalPermission, x.clock.Now())
	x.report(nodeMac, contracts.ReportApproval, pend)
	resolved := approvalFor(run, "apr_synth-resolved", "toolu_synth_00", contracts.ApprovalPermission, x.clock.Now())
	x.report(nodeMac, contracts.ReportApproval, resolved)
	resolved.State = contracts.ApprovalResolved
	resolved.Resolution = &contracts.ApprovalResolution{OptionID: "allow", Actor: contracts.Principal{Kind: contracts.PrincipalSystem, ID: "native-terminal"}, ResolvedAt: x.clock.Now(), Source: "native"}
	x.report(nodeMac, contracts.ReportApproval, resolved)

	page, _ := x.h.ListApprovals(as(phone), contracts.ListApprovalsRequest{})
	if len(page.Items) != 1 || page.Items[0].ApprovalID != pend.ApprovalID {
		t.Fatalf("pending list: %+v", page.Items)
	}
	if sv := x.session(v.Session.SessionID); len(sv.Run.PendingApprovalIDs) != 1 {
		t.Fatalf("pending ids: %+v", sv.Run.PendingApprovalIDs)
	}
	req := contracts.AnswerApprovalRequest{Mutation: contracts.Mutation{OperationID: "op_synth-apr-1"}, ApprovalID: pend.ApprovalID, RunID: run.RunID, OptionID: "allow"}
	a, err := x.h.AnswerApproval(as(phone), req)
	if err != nil || a.State != contracts.ApprovalResolved || a.Resolution.Actor != phone || a.Resolution.Source != "pyre" || a.Resolution.Effect != "submitted" {
		t.Fatalf("answer: %v %+v", err, a)
	}
	// Double tap: stored result, runtime answered once.
	a2, err := x.h.AnswerApproval(as(phone), req)
	if err != nil || a2.Resolution.ResolvedAt != a.Resolution.ResolvedAt {
		t.Fatalf("double tap: %v", err)
	}
	if n := len(x.ch.sentKinds(nodeMac, contracts.CmdAnswer)); n != 1 {
		t.Fatalf("answer commands: %d", n)
	}
	// Stale link to a natively resolved approval.
	_, err = x.h.AnswerApproval(as(phone), contracts.AnswerApprovalRequest{Mutation: contracts.Mutation{OperationID: "op_synth-apr-2"}, ApprovalID: resolved.ApprovalID, RunID: run.RunID, OptionID: "deny"})
	wantCode(t, err, contracts.ErrConflict)
	if !strings.Contains(err.Error(), "approval already resolved (allow, native terminal)") {
		t.Fatalf("message: %v", err)
	}
	// Wrong run.
	_, err = x.h.AnswerApproval(as(phone), contracts.AnswerApprovalRequest{Mutation: contracts.Mutation{OperationID: "op_x"}, ApprovalID: pend.ApprovalID, RunID: "run_other", OptionID: "allow"})
	wantCode(t, err, contracts.ErrConflict)

	// An approval for a run that gets replaced is withdrawn -> expired.
	old := approvalFor(run, "apr_synth-expired", "toolu_synth_old", contracts.ApprovalQuestion, x.clock.Now())
	old.AllowText, old.Options = true, nil
	x.report(nodeMac, contracts.ReportApproval, old)
	ex := run
	ex.State = contracts.RunExited
	x.report(nodeMac, contracts.ReportRun, ex)
	got, _ := x.h.GetApproval(as(phone), contracts.GetRequest{ID: old.ApprovalID})
	if got.State != contracts.ApprovalWithdrawn {
		t.Fatalf("withdrawn: %s", got.State)
	}
	if _, err := x.h.ResumeSession(as(owner), contracts.ResumeRequest{Mutation: contracts.Mutation{OperationID: "op_res"}, SessionID: v.Session.SessionID}); err != nil {
		t.Fatal(err)
	}
	_, err = x.h.AnswerApproval(as(phone), contracts.AnswerApprovalRequest{Mutation: contracts.Mutation{OperationID: "op_synth-apr-3"}, ApprovalID: old.ApprovalID, RunID: run.RunID, Text: "main"})
	wantCode(t, err, contracts.ErrExpired)
	if !strings.Contains(err.Error(), "approval withdrawn: its run was replaced") {
		t.Fatalf("message: %v", err)
	}
	var payload string
	x.db.QueryRow(`SELECT payload FROM events WHERE kind='approval.resolved' LIMIT 1`).Scan(&payload)
	var pa contracts.Approval
	if json.Unmarshal([]byte(payload), &pa) != nil || pa.ApprovalID == "" {
		t.Fatalf("approval payload: %s", payload)
	}
}

func TestApprovalConcurrentAnswersOneCommit(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_1", nodeMac)
	ap := approvalFor(*v.Run, "apr_synth-race", "toolu_r", contracts.ApprovalPermission, x.clock.Now())
	x.report(nodeMac, contracts.ReportApproval, ap)
	const n = 6
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, conflicts := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			opt := "allow"
			if i%2 == 1 {
				opt = "deny"
			}
			_, err := x.h.AnswerApproval(as(owner), contracts.AnswerApprovalRequest{Mutation: contracts.Mutation{OperationID: "op_ans" + string(rune('a'+i))}, ApprovalID: ap.ApprovalID, RunID: v.Run.RunID, OptionID: opt})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case contracts.IsCode(err, contracts.ErrConflict):
				conflicts++
			default:
				t.Errorf("unexpected %v", err)
			}
		}(i)
	}
	wg.Wait()
	if ok != 1 || conflicts != n-1 {
		t.Fatalf("ok=%d conflicts=%d", ok, conflicts)
	}
	var cmds int
	x.db.QueryRow(`SELECT COUNT(*) FROM commands WHERE kind='answer'`).Scan(&cmds)
	if cmds != 1 {
		t.Fatalf("answer commands %d", cmds)
	}
}

func TestApprovalEffectAndValidation(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_1", nodeMac)
	ap := approvalFor(*v.Run, "apr_synth-e", "toolu_e", contracts.ApprovalPermission, x.clock.Now())
	exp := x.clock.Now().Add(time.Minute)
	ap.ExpiresAt = &exp
	x.report(nodeMac, contracts.ReportApproval, ap)
	_, err := x.h.AnswerApproval(as(owner), contracts.AnswerApprovalRequest{Mutation: contracts.Mutation{OperationID: "op_bad"}, ApprovalID: ap.ApprovalID, RunID: v.Run.RunID, OptionID: "maybe"})
	wantCode(t, err, contracts.ErrInvalid)
	_, err = x.h.AnswerApproval(as(owner), contracts.AnswerApprovalRequest{Mutation: contracts.Mutation{OperationID: "op_txt"}, ApprovalID: ap.ApprovalID, RunID: v.Run.RunID, Text: "free"})
	wantCode(t, err, contracts.ErrInvalid)
	if _, err := x.h.AnswerApproval(as(owner), contracts.AnswerApprovalRequest{Mutation: contracts.Mutation{OperationID: "op_ok"}, ApprovalID: ap.ApprovalID, RunID: v.Run.RunID, OptionID: "allow"}); err != nil {
		t.Fatal(err)
	}
	// The TUI answered first: the native report is the truth.
	rep := ap
	rep.State = contracts.ApprovalResolved
	rep.Resolution = &contracts.ApprovalResolution{OptionID: "deny", Source: "native", Effect: "superseded", ResolvedAt: x.clock.Now()}
	x.report(nodeMac, contracts.ReportApproval, rep)
	got, _ := x.h.GetApproval(as(owner), contracts.GetRequest{ID: ap.ApprovalID})
	if got.Resolution.OptionID != "deny" || got.Resolution.Effect != "superseded" {
		t.Fatalf("superseded: %+v", got.Resolution)
	}
	// Time expiry of another approval.
	ap2 := approvalFor(*v.Run, "apr_synth-e2", "toolu_e2", contracts.ApprovalPermission, x.clock.Now())
	ap2.ExpiresAt = &exp
	x.report(nodeMac, contracts.ReportApproval, ap2)
	x.clock.Add(2 * time.Minute)
	_, err = x.h.AnswerApproval(as(owner), contracts.AnswerApprovalRequest{Mutation: contracts.Mutation{OperationID: "op_late"}, ApprovalID: ap2.ApprovalID, RunID: v.Run.RunID, OptionID: "allow"})
	wantCode(t, err, contracts.ErrExpired)
	if err := x.h.RunMaintenance(t.Context()); err != nil {
		t.Fatal(err)
	}
	g2, _ := x.h.GetApproval(as(owner), contracts.GetRequest{ID: ap2.ApprovalID})
	if g2.State != contracts.ApprovalExpired {
		t.Fatalf("swept: %s", g2.State)
	}
}

func TestMessageRoutingToRuns(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_1", nodeMac)
	sid := v.Session.SessionID
	to := contracts.Address{Kind: contracts.AddrSession, ID: sid}
	send := func(op string, mode contracts.MessageMode) contracts.MessageView {
		m, err := x.h.SendMessage(as(owner), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: op}, Recipient: to, Mode: mode, Body: contracts.MessageBody{Text: "synthetic " + op}})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	// Unknown recipient session.
	_, err := x.h.SendMessage(as(owner), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_nr"}, Recipient: contracts.Address{Kind: contracts.AddrSession, ID: "ses_missing"}, Body: contracts.MessageBody{Text: "x"}})
	wantCode(t, err, contracts.ErrNotFound)
	// Steer without input.steer: stored and rejected (fixture 03).
	st := send("op_steer", contracts.ModeSteer)
	if st.Status.Storage != contracts.StorageStored || st.Status.Runtime != contracts.DeliveryRejected || st.Status.RuntimeReason != "unsupported: adapter does not advertise input.steer" {
		t.Fatalf("steer: %+v", st.Status)
	}
	// Bound message on the current run.
	bound := send("op_bound", contracts.ModeQueue)
	x.report(nodeMac, contracts.ReportReceipt, contracts.DeliveryReceipt{MessageID: bound.Envelope.MessageID, RunID: v.Run.RunID, State: contracts.DeliveryIndeterminate, Evidence: contracts.EvidenceNone})
	// Run exits; a message waits for a run.
	ex := *v.Run
	ex.State = contracts.RunExited
	x.report(nodeMac, contracts.ReportRun, ex)
	waiting := send("op_wait", contracts.ModeQueue)
	if waiting.Status.Runtime != contracts.DeliveryQueued || !strings.Contains(waiting.Status.RuntimeReason, "no live run") {
		t.Fatalf("waiting: %+v", waiting.Status)
	}
	delivers := len(x.ch.sentKinds(nodeMac, contracts.CmdDeliver))
	// Resume: the unattempted message goes to the new run; the bound
	// indeterminate one never moves.
	resp, err := x.h.ResumeSession(as(owner), contracts.ResumeRequest{Mutation: contracts.Mutation{OperationID: "op_res"}, SessionID: sid})
	if err != nil {
		t.Fatal(err)
	}
	sent := x.ch.sentKinds(nodeMac, contracts.CmdDeliver)
	if len(sent) != delivers+1 {
		t.Fatalf("deliveries after resume: %d -> %d", delivers, len(sent))
	}
	var dc contracts.DeliverCommand
	json.Unmarshal(sent[len(sent)-1].Payload, &dc)
	if dc.Message.MessageID != waiting.Envelope.MessageID {
		t.Fatalf("wrong message routed: %s", dc.Message.MessageID)
	}
	b := x.msg(bound.Envelope.MessageID)
	if b.Status.Runtime != contracts.DeliveryIndeterminate || b.Status.BoundRunID != v.Run.RunID {
		t.Fatalf("bound moved: %+v", b.Status)
	}
	// A receipt for the new run on the bound message is refused (rejected report).
	x.report(nodeMac, contracts.ReportReceipt, contracts.DeliveryReceipt{MessageID: bound.Envelope.MessageID, RunID: resp.Session.Run.RunID, State: contracts.DeliveryAccepted, Evidence: contracts.EvidenceClaudePromptHook})
	if got := x.msg(bound.Envelope.MessageID); got.Status.Runtime != contracts.DeliveryIndeterminate {
		t.Fatal("rebinding accepted")
	}
	// Cancelling a sent-but-unacked delivery withdraws the command and tells the node.
	c := send("op_cancel", contracts.ModeQueue)
	if _, err := x.h.CancelMessage(as(owner), contracts.CancelMessageRequest{Mutation: contracts.Mutation{OperationID: "op_c"}, MessageID: c.Envelope.MessageID}); err != nil {
		t.Fatal(err)
	}
	if len(x.ch.sentKinds(nodeMac, contracts.CmdCancelDelivery)) != 1 {
		t.Fatal("cancel_delivery not sent")
	}
	// Consumption evidence after acceptance sets consumed_at only.
	acc := send("op_acc", contracts.ModeQueue)
	newRun := resp.Session.Run.RunID
	x.report(nodeMac, contracts.ReportReceipt, contracts.DeliveryReceipt{MessageID: acc.Envelope.MessageID, RunID: newRun, State: contracts.DeliveryAccepted, Evidence: contracts.EvidenceClaudeQueue})
	if got := x.msg(acc.Envelope.MessageID); got.Status.Runtime != contracts.DeliveryAccepted || got.Status.ConsumedAt != nil {
		t.Fatalf("queued acceptance: %+v", got.Status)
	}
	x.report(nodeMac, contracts.ReportReceipt, contracts.DeliveryReceipt{MessageID: acc.Envelope.MessageID, RunID: newRun, State: contracts.DeliveryAccepted, Evidence: contracts.EvidenceClaudeUserRecord})
	if got := x.msg(acc.Envelope.MessageID); got.Status.Runtime != contracts.DeliveryAccepted || got.Status.ConsumedAt == nil {
		t.Fatalf("consumed: %+v", got.Status)
	}
	var payload string
	x.db.QueryRow(`SELECT payload FROM events WHERE kind='message.delivery' ORDER BY cursor DESC LIMIT 1`).Scan(&payload)
	var mv contracts.MessageView
	if json.Unmarshal([]byte(payload), &mv) != nil || mv.Envelope.MessageID != acc.Envelope.MessageID || mv.Status.ConsumedAt == nil {
		t.Fatalf("message.delivery payload: %s", payload)
	}
}

func TestAccountsIngestion(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	a, err := x.h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: contracts.Mutation{OperationID: "op_a"}, Provider: contracts.ProviderAnthropic, Label: "claude-personal-a"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = x.h.UpsertAccount(as(sesPrincipal("ses_x")), contracts.UpsertAccountRequest{Mutation: contracts.Mutation{OperationID: "op_b"}, Provider: contracts.ProviderAnthropic, Label: "x"})
	wantCode(t, err, contracts.ErrForbidden)
	now := x.clock.Now()
	x.report(nodeMac, contracts.ReportAccountObs, contracts.AccountObservation{AccountID: a.AccountID, Provider: a.Provider, ObservedAt: now, Source: "rota:usage-endpoint", Confidence: "high", Eligibility: contracts.EligibleYes})
	// An older observation does not replace the newer one.
	x.report(nodeMac, contracts.ReportAccountObs, contracts.AccountObservation{AccountID: a.AccountID, Provider: a.Provider, ObservedAt: now.Add(-time.Hour), Eligibility: contracts.EligibleExhausted})
	x.report(nodeMac, contracts.ReportGrant, contracts.DeviceGrant{GrantID: "grant_synth-a-mac", AccountID: a.AccountID, Harness: contracts.HarnessClaude, Kind: contracts.CredClaudeProfileStore, State: contracts.GrantValid, Generation: 2, ObservedAt: now})
	x.report(nodeMac, contracts.ReportGrant, contracts.DeviceGrant{GrantID: "grant_synth-a-mac", AccountID: a.AccountID, Harness: contracts.HarnessClaude, State: contracts.GrantValid, Generation: 1, ObservedAt: now})
	v := x.start(owner, "op_s", nodeMac)
	x.report(nodeMac, contracts.ReportAssignment, contracts.AccountAssignment{AssignmentID: "asg_synth-1", AccountID: a.AccountID, GrantID: "grant_synth-a-mac", RunID: v.Run.RunID, Reason: "most headroom", AssignedAt: now})
	page, err := x.h.ListAccounts(as(owner), contracts.ListAccountsRequest{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("accounts: %v", err)
	}
	av := page.Items[0]
	if av.Latest == nil || av.Latest.Eligibility != contracts.EligibleYes || len(av.Grants) != 1 || av.Grants[0].Generation != 2 || len(av.Assignments) != 1 {
		t.Fatalf("view: %+v", av)
	}
	rej, _ := x.h.NodeReportRejects(t.Context(), nodeMac)
	if len(rej) != 1 || !strings.Contains(rej[0], "stale grant generation") {
		t.Fatalf("rejects: %v", rej)
	}
	// Stale data is never presented as eligible.
	x.clock.Add(2 * time.Hour)
	page, _ = x.h.ListAccounts(as(owner), contracts.ListAccountsRequest{})
	if l := page.Items[0].Latest; !l.Stale || l.Eligibility != contracts.EligibleUnknown {
		t.Fatalf("stale: %+v", l)
	}
	// Fallback: no eligible account -> fallback + exhausted events (FallbackPlan payload).
	x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: v.Session.SessionID, FromRunID: v.Run.RunID, FromAccountID: a.AccountID, Outcome: contracts.FallbackWaiting, Reason: "no eligible anthropic subscription"})
	fb, _ := x.h.ListFallbacks(as(owner), contracts.ListFallbacksRequest{SessionID: v.Session.SessionID})
	if len(fb.Items) != 1 || fb.Items[0].Provider != contracts.ProviderAnthropic {
		t.Fatalf("fallbacks: %+v", fb.Items)
	}
	if x.countEvents(contracts.EvAccountExhausted) != 1 || x.countEvents(contracts.EvFallbackPlanned) != 1 {
		t.Fatal("fallback events")
	}
	var payload string
	x.db.QueryRow(`SELECT payload FROM events WHERE kind='account.exhausted'`).Scan(&payload)
	var fp contracts.FallbackPlan
	if json.Unmarshal([]byte(payload), &fp) != nil || fp.SessionID != v.Session.SessionID {
		t.Fatalf("exhausted payload %s", payload)
	}
	// Cross-provider fallback is refused.
	o, _ := x.h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: contracts.Mutation{OperationID: "op_o"}, Provider: contracts.ProviderOpenAI, Label: "codex-a"})
	x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: v.Session.SessionID, FromRunID: v.Run.RunID, FromAccountID: a.AccountID, ToAccountID: o.AccountID, Outcome: contracts.FallbackNativeResume})
	fb, _ = x.h.ListFallbacks(as(owner), contracts.ListFallbacksRequest{})
	if len(fb.Items) != 1 {
		t.Fatal("cross-provider fallback stored")
	}
	// Explicit policy with the wrong provider is invalid at launch.
	_, err = x.h.StartSession(as(owner), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_s2"}, NodeID: nodeMac, Harness: contracts.HarnessClaude, AccountPolicy: contracts.AccountPolicy{Mode: contracts.AccountExplicit, AccountID: o.AccountID}})
	wantCode(t, err, contracts.ErrInvalid)
}

func TestEventsThroughHub(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	x.start(owner, "op_1", nodeMac)
	resp, err := x.h.ListEvents(as(owner), contracts.ListEventsRequest{})
	if err != nil || len(resp.Events) == 0 {
		t.Fatalf("events: %v", err)
	}
	head := resp.HeadCursor
	done := make(chan contracts.ListEventsResponse, 1)
	go func() {
		r, _ := x.h.ListEvents(as(owner), contracts.ListEventsRequest{AfterCursor: head, WaitSeconds: 5, Kinds: []contracts.EventKind{contracts.EvMessageStored}})
		done <- r
	}()
	time.Sleep(50 * time.Millisecond)
	x.h.SendMessage(as(owner), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_m"}, Recipient: contracts.Address{Kind: contracts.AddrOwner}, Body: contracts.MessageBody{Text: "note to self"}})
	select {
	case r := <-done:
		if len(r.Events) != 1 {
			t.Fatalf("long poll: %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("long poll did not wake")
	}
	_, err = x.h.ListEvents(t.Context(), contracts.ListEventsRequest{})
	wantCode(t, err, contracts.ErrUnauthorized)
}

func TestAccountChangedEvents(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	a, err := x.h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: contracts.Mutation{OperationID: "op_a"}, Provider: contracts.ProviderAnthropic, Label: "claude-personal-a"})
	if err != nil {
		t.Fatal(err)
	}
	if n := x.countEvents(contracts.EvAccountChanged); n != 1 {
		t.Fatalf("account.changed after upsert = %d", n)
	}
	var payload string
	x.db.QueryRow(`SELECT payload FROM events WHERE kind='account.changed' ORDER BY cursor DESC LIMIT 1`).Scan(&payload)
	var got contracts.Account
	if json.Unmarshal([]byte(payload), &got) != nil || got.AccountID != a.AccountID || got.Label != "claude-personal-a" {
		t.Fatalf("account.changed payload: %s", payload)
	}
	// A replay of the same operation emits nothing new.
	if _, err := x.h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: contracts.Mutation{OperationID: "op_a"}, Provider: contracts.ProviderAnthropic, Label: "claude-personal-a"}); err != nil {
		t.Fatal(err)
	}
	if n := x.countEvents(contracts.EvAccountChanged); n != 1 {
		t.Fatalf("account.changed after replay = %d", n)
	}
	// An account first seen in a node report is announced once.
	newID := contracts.NewID(contracts.PrefixAccount)
	now := x.clock.Now()
	x.report(nodeMac, contracts.ReportAccountObs, contracts.AccountObservation{AccountID: newID, Provider: contracts.ProviderOpenAI, ObservedAt: now, Eligibility: contracts.EligibleYes})
	x.report(nodeMac, contracts.ReportAccountObs, contracts.AccountObservation{AccountID: newID, Provider: contracts.ProviderOpenAI, ObservedAt: now.Add(time.Minute), Eligibility: contracts.EligibleYes})
	if n := x.countEvents(contracts.EvAccountChanged); n != 2 {
		t.Fatalf("account.changed after report ingestion = %d", n)
	}
}

// TestAccountViewPerNode: one account with grants (and independent logins)
// on two nodes. The view keeps each node's own newest observation, so a
// node reporting "unknown" (expired token at rest) no longer hides another
// node's "eligible" (seen live: laptop's report replaced server's).
func TestAccountViewPerNode(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	x.connect(nodeLin, 0)
	a, err := x.h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: contracts.Mutation{OperationID: "op_pn"}, Provider: contracts.ProviderAnthropic, Label: "synthetic-two-nodes"})
	if err != nil {
		t.Fatal(err)
	}
	now := x.clock.Now()
	x.report(nodeLin, contracts.ReportAccountObs, contracts.AccountObservation{AccountID: a.AccountID, Provider: a.Provider, ObservedAt: now.Add(-time.Minute),
		Source: "rota:usage-endpoint", Confidence: "high", Eligibility: contracts.EligibleYes, Windows: []contracts.UsageWindow{{Name: "seven_day", UsedPercent: 87}}})
	x.report(nodeMac, contracts.ReportAccountObs, contracts.AccountObservation{AccountID: a.AccountID, Provider: a.Provider, ObservedAt: now,
		Source: "rota-snapshot", Confidence: "low", Eligibility: contracts.EligibleUnknown, Detail: "access token expired"})
	page, err := x.h.ListAccounts(as(owner), contracts.ListAccountsRequest{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("accounts: %v", err)
	}
	av := page.Items[0]
	if av.Latest == nil || av.Latest.ReportingNode != nodeMac {
		t.Fatalf("latest: %+v", av.Latest)
	}
	if len(av.ByNode) != 2 {
		t.Fatalf("by_node: %+v", av.ByNode)
	}
	got := map[string]contracts.Eligibility{}
	for _, o := range av.ByNode {
		got[o.ReportingNode] = o.Eligibility
	}
	if got[nodeLin] != contracts.EligibleYes || got[nodeMac] != contracts.EligibleUnknown {
		t.Fatalf("per-node eligibility: %v", got)
	}
	// Each node's entry ages independently (stale data is never eligible).
	x.clock.Add(2 * time.Hour)
	page, _ = x.h.ListAccounts(as(owner), contracts.ListAccountsRequest{})
	for _, o := range page.Items[0].ByNode {
		if !o.Stale || o.Eligibility != contracts.EligibleUnknown {
			t.Fatalf("stale per-node entry presented as fresh: %+v", o)
		}
	}
}
