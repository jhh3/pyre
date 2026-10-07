package attention

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

const secret = "SYNTH-SECRET-BODY-TEXT"

type fakeLookup map[string]contracts.Provider

func (f fakeLookup) AccountProvider(_ context.Context, id string) (contracts.Provider, error) {
	if p, ok := f[id]; ok {
		return p, nil
	}
	return "", contracts.Errorf(contracts.ErrNotFound, "no account")
}

func mkEvent(kind contracts.EventKind, obj contracts.ObjectRef, session string, v any) contracts.Event {
	b, _ := json.Marshal(v)
	return contracts.Event{EventID: contracts.NewID(contracts.PrefixEvent), Cursor: 1, Origin: "hub", Kind: kind, Object: obj, SessionID: session, Payload: b}
}

func ptr[T any](v T) *T { return &v }

func TestDeriveTable(t *testing.T) {
	prj := "prj_synth-1"
	approval := contracts.Approval{ApprovalID: "apr_synth-1", SessionID: "ses_synth-a", RunID: "run_synth-a", Kind: contracts.ApprovalPermission,
		Title: "make deploy-staging", Body: secret, State: contracts.ApprovalPending}
	question := approval
	question.ApprovalID, question.Kind, question.Title = "apr_synth-q", contracts.ApprovalQuestion, "Which branch?"
	resolvedNative := approval
	resolvedNative.State = contracts.ApprovalResolved
	resolvedNative.Resolution = &contracts.ApprovalResolution{Source: "native", ResolvedAt: time.Now()}
	expired := approval
	expired.State = contracts.ApprovalExpired

	ownerMsg := contracts.MessageView{Envelope: contracts.MessageEnvelope{MessageID: "msg_synth-1", Sender: contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_synth-a", Name: "worker-a"},
		Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}, ThreadID: "thr_synth-1", Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: secret}}}
	infoMsg := ownerMsg
	infoMsg.Envelope.Mode, infoMsg.Envelope.ThreadID = contracts.ModeInfo, ""
	peerMsg := ownerMsg
	peerMsg.Envelope.Recipient = contracts.Address{Kind: contracts.AddrSession, ID: "ses_synth-b"}
	ownerReply := contracts.MessageView{Envelope: contracts.MessageEnvelope{MessageID: "msg_synth-2", Sender: contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synth"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: "ses_synth-a"}, ThreadID: "thr_synth-1"}}
	selfNote := ownerReply
	selfNote.Envelope.Recipient = contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}

	indet := contracts.MessageView{Envelope: contracts.MessageEnvelope{MessageID: "msg_synth-i", Recipient: contracts.Address{Kind: contracts.AddrSession, ID: "ses_synth-b"}},
		Status: contracts.DeliveryStatus{Runtime: contracts.DeliveryIndeterminate}}
	accepted := indet
	accepted.Status.Runtime = contracts.DeliveryAccepted
	receipt := contracts.DeliveryReceipt{MessageID: "msg_synth-r", SessionID: "ses_synth-b", State: contracts.DeliveryIndeterminate}
	queued := indet
	queued.Status.Runtime = contracts.DeliveryQueued

	failed := contracts.Run{RunID: "run_synth-f", SessionID: "ses_synth-a", State: contracts.RunFailed, StateReason: secret, ExitCode: ptr(2)}
	unknown := failed
	unknown.State = contracts.RunUnknown
	exited := failed
	exited.State = contracts.RunExited

	pub := map[string]any{"artifact": contracts.Artifact{ArtifactID: "art_synth-r", Title: "Synthetic report", ProjectID: &prj},
		"version":          contracts.ArtifactVersion{VersionID: "ver_synth-2", ArtifactID: "art_synth-r", Seq: 2, Provenance: contracts.Provenance{SessionID: "ses_synth-a"}},
		"review_requested": true}
	pubNoReview := map[string]any{"artifact": contracts.Artifact{ArtifactID: "art_synth-r", Title: "Synthetic report"},
		"version": contracts.ArtifactVersion{VersionID: "ver_synth-3", ArtifactID: "art_synth-r", Seq: 3}}

	agentComment := contracts.ReviewComment{CommentID: "cmt_synth-1", Target: contracts.ObjectRef{Kind: contracts.KindArtifactVersion, ID: "ver_synth-2"},
		Author: contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_synth-a"}, Body: secret, Mentions: []string{"@owner"}}
	ownerComment := agentComment
	ownerComment.Author = contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synth"}
	noMention := agentComment
	noMention.Mentions = []string{"ses_synth-b"}

	conflict := contracts.DocumentProposal{ProposalID: "prp_synth-1", DocumentID: "doc_synth-1", Content: secret, Author: contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_synth-a"}}
	ownerConflict := conflict
	ownerConflict.Author = contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synth"}

	updFail := map[string]any{"tool": "claude", "version": "2.1.300", "state": "failed", "health": "failed: " + secret, "rolled_back": true}
	updOK := map[string]any{"tool": "claude", "version": "2.1.301", "state": "active", "health": "ok"}

	ownerAck := ownerMsg
	ownerAck.Status.Ack = contracts.AckAcknowledged
	ownerAckNoThread := infoMsg
	ownerAckNoThread.Status.Ack = contracts.AckReplied
	ackNone := ownerMsg
	ackNone.Status.Ack = contracts.AckNone
	peerAck := peerMsg
	peerAck.Status.Ack = contracts.AckAcknowledged

	grantDead := contracts.DeviceGrant{GrantID: "grt_synth-1", AccountID: "acct_synth-1", NodeID: "node_synth-mac", State: contracts.GrantDead, Detail: secret}
	grantExpired := grantDead
	grantExpired.State = contracts.GrantExpired
	grantValid := grantDead
	grantValid.State = contracts.GrantValid
	grantRevoked := grantDead
	grantRevoked.State = contracts.GrantRevoked
	grantExpiring := grantDead
	grantExpiring.State = contracts.GrantExpiring
	grantRef := contracts.ObjectRef{Kind: contracts.KindGrant, ID: "grt_synth-1"}

	nodeRef := contracts.ObjectRef{Kind: contracts.KindNode, ID: "node_synth-mac"}
	aprRef := contracts.ObjectRef{Kind: contracts.KindApproval, ID: "apr_synth-1"}

	type want struct {
		op       opKind
		kind     contracts.AttentionKind
		group    string
		severity string
		object   contracts.ObjectKind
	}
	cases := []struct {
		name string
		ev   contracts.Event
		want []want
	}{
		{"approval requested", mkEvent(contracts.EvApprovalRequested, aprRef, "ses_synth-a", approval),
			[]want{{opRaise, contracts.AttnApproval, "approval:apr_synth-1", "urgent", contracts.KindApproval}}},
		{"question requested", mkEvent(contracts.EvApprovalRequested, contracts.ObjectRef{Kind: contracts.KindApproval, ID: "apr_synth-q"}, "", question),
			[]want{{opRaise, contracts.AttnQuestion, "approval:apr_synth-q", "urgent", contracts.KindApproval}}},
		{"approval resolved natively", mkEvent(contracts.EvApprovalResolved, aprRef, "", resolvedNative),
			[]want{{op: opWithdraw, group: "approval:apr_synth-1"}, {op: opWithdraw, group: "terminal:apr_synth-1"}}},
		{"approval withdrawn, payload-less", contracts.Event{Kind: contracts.EvApprovalWithdrawn, Object: aprRef},
			[]want{{op: opWithdraw, group: "approval:apr_synth-1"}, {op: opWithdraw, group: "terminal:apr_synth-1"}}},
		// The remote answer window closed with the native prompt still up:
		// nothing to answer through Pyre, but the run still needs the owner.
		{"approval expired, prompt in the terminal", mkEvent(contracts.EvApprovalWithdrawn, aprRef, "ses_synth-a", expired),
			[]want{{op: opWithdraw, group: "approval:apr_synth-1"}, {opRaise, contracts.AttnApproval, "terminal:apr_synth-1", "urgent", contracts.KindSession}}},
		{"message to owner", mkEvent(contracts.EvMessageStored, contracts.ObjectRef{Kind: contracts.KindMessage, ID: "msg_synth-1"}, "", ownerMsg),
			[]want{{opRaise, contracts.AttnMessage, "message:thr_synth-1", "normal", contracts.KindThread}}},
		{"info message to owner without thread", mkEvent(contracts.EvMessageStored, contracts.ObjectRef{}, "", infoMsg),
			[]want{{opRaise, contracts.AttnMessage, "message:from:session:ses_synth-a", "info", contracts.KindMessage}}},
		{"bare envelope payload", mkEvent(contracts.EvMessageStored, contracts.ObjectRef{}, "", ownerMsg.Envelope),
			[]want{{opRaise, contracts.AttnMessage, "message:thr_synth-1", "normal", contracts.KindThread}}},
		{"agent-to-agent progress message", mkEvent(contracts.EvMessageStored, contracts.ObjectRef{}, "", peerMsg), nil},
		{"owner reply handles thread", mkEvent(contracts.EvMessageStored, contracts.ObjectRef{}, "", ownerReply),
			// ... and, sent to a session, engages with it (its messages and
			// own flag; group "" = by session and kind).
			[]want{{op: opHandle, group: "message:thr_synth-1"}, {op: opHandle, group: "message:from:session:ses_synth-a"}, {op: opHandle, group: ""}}},
		{"owner note to self", mkEvent(contracts.EvMessageStored, contracts.ObjectRef{}, "", selfNote), nil},
		{"owner ack handles thread item", mkEvent(contracts.EvMessageAck, contracts.ObjectRef{Kind: contracts.KindMessage, ID: "msg_synth-1"}, "", ownerAck),
			[]want{{op: opHandle, group: "message:thr_synth-1"}}},
		{"reply-ack handles sender item", mkEvent(contracts.EvMessageAck, contracts.ObjectRef{}, "", ownerAckNoThread),
			[]want{{op: opHandle, group: "message:from:session:ses_synth-a"}}},
		{"ack none is not handling", mkEvent(contracts.EvMessageAck, contracts.ObjectRef{}, "", ackNone), nil},
		{"agent-to-agent ack", mkEvent(contracts.EvMessageAck, contracts.ObjectRef{}, "", peerAck), nil},
		{"grant dead", mkEvent(contracts.EvGrantChanged, grantRef, "", grantDead),
			[]want{{opRaise, contracts.AttnNode, "grant:grt_synth-1", "normal", contracts.KindGrant}}},
		{"grant expired", mkEvent(contracts.EvGrantChanged, contracts.ObjectRef{}, "", grantExpired),
			[]want{{opRaise, contracts.AttnNode, "grant:grt_synth-1", "normal", contracts.KindGrant}}},
		{"grant valid withdraws", mkEvent(contracts.EvGrantChanged, grantRef, "", grantValid),
			[]want{{op: opWithdraw, group: "grant:grt_synth-1"}}},
		{"grant revoked withdraws", mkEvent(contracts.EvGrantChanged, grantRef, "", grantRevoked),
			[]want{{op: opWithdraw, group: "grant:grt_synth-1"}}},
		{"grant expiring is not attention", mkEvent(contracts.EvGrantChanged, grantRef, "", grantExpiring), nil},
		{"delivery indeterminate", mkEvent(contracts.EvMessageDelivery, contracts.ObjectRef{Kind: contracts.KindMessage, ID: "msg_synth-i"}, "", indet),
			[]want{{opRaise, contracts.AttnIndeterminate, "indeterminate:msg_synth-i", "urgent", contracts.KindMessage}}},
		{"delivery receipt indeterminate", mkEvent(contracts.EvMessageDelivery, contracts.ObjectRef{}, "", receipt),
			[]want{{opRaise, contracts.AttnIndeterminate, "indeterminate:msg_synth-r", "urgent", contracts.KindMessage}}},
		{"delivery accepted withdraws", mkEvent(contracts.EvMessageDelivery, contracts.ObjectRef{}, "", accepted),
			[]want{{op: opWithdraw, group: "indeterminate:msg_synth-i"}}},
		{"delivery queued is not attention", mkEvent(contracts.EvMessageDelivery, contracts.ObjectRef{}, "", queued), nil},
		{"account exhausted", mkEvent(contracts.EvAccountExhausted, contracts.ObjectRef{Kind: contracts.KindAccount, ID: "acct_synth-1"}, "",
			contracts.AccountObservation{AccountID: "acct_synth-1", Provider: contracts.ProviderAnthropic, Eligibility: contracts.EligibleExhausted}),
			[]want{{opRaise, contracts.AttnAccountPool, "pool:anthropic", "normal", contracts.KindAccount}}},
		{"fallback waiting via lookup", mkEvent(contracts.EvFallbackPlanned, contracts.ObjectRef{Kind: contracts.KindSession, ID: "ses_synth-a"}, "ses_synth-a",
			contracts.FallbackPlan{SessionID: "ses_synth-a", FromAccountID: "acct_synth-2", Outcome: contracts.FallbackWaiting, Reason: secret}),
			[]want{{opRaise, contracts.AttnAccountPool, "pool:openai", "urgent", contracts.KindSession}}},
		{"fallback intervention", mkEvent(contracts.EvFallbackPlanned, contracts.ObjectRef{}, "ses_synth-a",
			map[string]any{"session_id": "ses_synth-a", "outcome": "intervention", "provider": "anthropic"}),
			[]want{{opRaise, contracts.AttnAccountPool, "pool:anthropic", "urgent", contracts.KindSession}}},
		{"fallback native resume is not attention", mkEvent(contracts.EvFallbackPlanned, contracts.ObjectRef{}, "ses_synth-a",
			contracts.FallbackPlan{SessionID: "ses_synth-a", Outcome: contracts.FallbackNativeResume}), nil},
		{"run failed", mkEvent(contracts.EvRunEnded, contracts.ObjectRef{Kind: contracts.KindRun, ID: "run_synth-f"}, "ses_synth-a", failed),
			[]want{{op: opWithdraw, group: "flag:session:ses_synth-a"}, {opRaise, contracts.AttnFailure, "failure:ses_synth-a", "normal", contracts.KindSession}}},
		// Any run end withdraws the session's own flag; only failure raises.
		{"run unknown is not failure", mkEvent(contracts.EvRunEnded, contracts.ObjectRef{}, "ses_synth-a", unknown), []want{{op: opWithdraw, group: "flag:session:ses_synth-a"}}},
		{"run exited is not failure", mkEvent(contracts.EvRunEnded, contracts.ObjectRef{}, "ses_synth-a", exited), []want{{op: opWithdraw, group: "flag:session:ses_synth-a"}}},
		{"artifact review requested", mkEvent(contracts.EvArtifactPublished, contracts.ObjectRef{Kind: contracts.KindArtifactVersion, ID: "ver_synth-2"}, "", pub),
			[]want{{opRaise, contracts.AttnReviewReady, "review:art_synth-r", "normal", contracts.KindArtifactVersion}}},
		{"artifact without review request", mkEvent(contracts.EvArtifactPublished, contracts.ObjectRef{}, "", pubNoReview), nil},
		{"comment mentioning owner", mkEvent(contracts.EvCommentCreated, contracts.ObjectRef{Kind: contracts.KindComment, ID: "cmt_synth-1"}, "", agentComment),
			[]want{{opRaise, contracts.AttnComment, "comment:ver_synth-2", "normal", contracts.KindComment}}},
		{"owner's own comment", mkEvent(contracts.EvCommentCreated, contracts.ObjectRef{}, "", ownerComment), nil},
		{"comment not mentioning owner", mkEvent(contracts.EvCommentCreated, contracts.ObjectRef{}, "", noMention), nil},
		{"document conflict", mkEvent(contracts.EvDocumentConflict, contracts.ObjectRef{Kind: contracts.KindDocument, ID: "doc_synth-1"}, "", conflict),
			[]want{{opRaise, contracts.AttnConflict, "conflict:doc_synth-1", "normal", contracts.KindDocument}}},
		{"owner's own conflict", mkEvent(contracts.EvDocumentConflict, contracts.ObjectRef{}, "", ownerConflict), nil},
		{"update failure", mkEvent(contracts.EvUpdateResult, nodeRef, "", updFail),
			[]want{{opRaise, contracts.AttnUpdate, "update:claude:node_synth-mac", "normal", contracts.KindNode}}},
		{"update success withdraws", func() contracts.Event {
			e := mkEvent(contracts.EvUpdateResult, contracts.ObjectRef{}, "", updOK)
			e.Origin = "node:node_synth-mac"
			return e
		}(), []want{{op: opWithdraw, group: "update:claude:node_synth-mac"}}},
		{"attention.changed never derives", mkEvent(contracts.EvAttentionChanged, contracts.ObjectRef{Kind: contracts.KindAttention, ID: "att_synth-1"}, "",
			contracts.AttentionItem{AttentionID: "att_synth-1", Kind: contracts.AttnApproval}), nil},
		{"unrelated kind", mkEvent(contracts.EvNodeStatus, nodeRef, "", map[string]any{}), nil},
		{"garbage payload", contracts.Event{Kind: contracts.EvRunEnded, Payload: json.RawMessage(`{"state":`)}, nil},
	}
	lk := fakeLookup{"acct_synth-2": contracts.ProviderOpenAI}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := derive(context.Background(), tc.ev, lk)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d changes %+v, want %d", len(got), got, len(tc.want))
			}
			for i, w := range tc.want {
				g := got[i]
				if g.op != w.op || g.groupKey != w.group {
					t.Errorf("change %d: got %s %s, want %s %s", i, g.op, g.groupKey, w.op, w.group)
				}
				if w.op == opRaise {
					if g.kind != w.kind || g.severity != w.severity || g.object.Kind != w.object || g.object.ID == "" || g.title == "" {
						t.Errorf("change %d: got kind=%s sev=%s obj=%+v title=%q", i, g.kind, g.severity, g.object, g.title)
					}
				}
				if strings.Contains(g.title, secret) || strings.Contains(g.summary, secret) {
					t.Errorf("change %d leaks body text: %q / %q", i, g.title, g.summary)
				}
			}
		})
	}
}

type labelLookup struct{ fakeLookup }

func (labelLookup) AccountLabel(_ context.Context, id string) (string, error) {
	if id == "acct_synth-1" {
		return "work-max", nil
	}
	return "", contracts.Errorf(contracts.ErrNotFound, "no account")
}

func (labelLookup) NodeName(_ context.Context, id string) (string, error) {
	if id == "node_synth-mac" {
		return "synth-mac", nil
	}
	return "", contracts.Errorf(contracts.ErrNotFound, "no node")
}

func TestDeriveGrantTitle(t *testing.T) {
	g := contracts.DeviceGrant{GrantID: "grt_synth-1", AccountID: "acct_synth-1", NodeID: "node_synth-mac", State: contracts.GrantDead, Detail: secret}
	ev := mkEvent(contracts.EvGrantChanged, contracts.ObjectRef{Kind: contracts.KindGrant, ID: g.GrantID}, "", g)
	got := derive(context.Background(), ev, labelLookup{})
	if len(got) != 1 || got[0].title != "Re-login needed for work-max on synth-mac" {
		t.Fatalf("labelled title: %+v", got)
	}
	// Without label lookups the IDs are used.
	got = derive(context.Background(), ev, fakeLookup{})
	if len(got) != 1 || got[0].title != "Re-login needed for acct_synth-1 on node_synth-mac" {
		t.Fatalf("fallback title: %+v", got)
	}
}

func TestDeriveTitles(t *testing.T) {
	a := contracts.Approval{ApprovalID: "apr_synth-1", Kind: contracts.ApprovalPermission, Title: "make\ndeploy-staging\t" + strings.Repeat("x", 300)}
	ch := approvalRaise(a, contracts.Event{})
	if strings.ContainsAny(ch.title, "\n\t") || len([]rune(ch.title)) > maxTitle {
		t.Fatalf("title not sanitized: %q", ch.title)
	}
	if !strings.HasPrefix(ch.title, "Approve: make deploy-staging") {
		t.Fatalf("title = %q", ch.title)
	}
	res := contracts.Approval{ApprovalID: "apr_synth-1", Resolution: &contracts.ApprovalResolution{Source: "native"}}
	if s := approvalEndSummary(contracts.EvApprovalResolved, res); s != "Resolved in the terminal." {
		t.Fatalf("native summary = %q", s)
	}
	if s := approvalEndSummary(contracts.EvApprovalResolved, contracts.Approval{}); s != "Resolved." {
		t.Fatalf("unknown-source summary must not invent a client: %q", s)
	}
}

// A failed launch says why in plain words (server, 2026-10-06: the item read
// only "Session run failed"), and a later run of the session resolves it.
func TestDeriveRunFailedReasonAndResolve(t *testing.T) {
	r := contracts.Run{RunID: "run_synth-f", SessionID: "ses_synth-a", NodeID: "node_synth-mac", State: contracts.RunFailed,
		StateReason: "launch failed: no eligible openai subscription on this node: acct_synth-1: verified at checkout: availability unknown: account/rateLimits/read failed: wham/usage failed: 401 Unauthorized"}
	got := derive(context.Background(), mkEvent(contracts.EvRunEnded, contracts.ObjectRef{Kind: contracts.KindRun, ID: r.RunID}, r.SessionID, r), labelLookup{})
	want := "Couldn't start: work-max's Codex login on synth-mac needs re-auth (401). Fix: pyre accounts login --node synth-mac"
	if len(got) != 2 || got[1].summary != want {
		t.Fatalf("summary: %+v", got)
	}
	started := contracts.Run{RunID: "run_synth-g", SessionID: "ses_synth-a", State: contracts.RunStarting}
	got = derive(context.Background(), mkEvent(contracts.EvRunStarted, contracts.ObjectRef{Kind: contracts.KindRun, ID: started.RunID}, "ses_synth-a", started), labelLookup{})
	if len(got) != 1 || got[0].op != opWithdraw || got[0].groupKey != "failure:ses_synth-a" {
		t.Fatalf("run.started: %+v", got)
	}
}
