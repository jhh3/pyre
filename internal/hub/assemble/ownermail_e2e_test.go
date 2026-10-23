package assemble

import (
	"context"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Owner mail through the assembled hub (real message store, attention
// store and deriver, simulated node). Owner report 2026-10-19: "why are
// there a bunch of useless messages in my inbox" — status reports and the
// carriers of long-decided asks sat unacked for days.
func TestOwnerMailLeavesTheInboxEndToEnd(t *testing.T) {
	st, n := newStack(t)
	bg := context.Background()
	owner := c.WithPrincipal(bg, ownerP)
	op := func() c.Mutation { return c.Mutation{OperationID: c.NewID(c.PrefixOperation)} }
	resp, err := st.Hub.StartSession(owner, c.StartSessionRequest{Mutation: op(), NodeID: n.nodeID, Harness: c.HarnessMock, Cwd: "/tmp/synth", Title: "synth reporter"})
	if err != nil {
		t.Fatal(err)
	}
	n.idle()
	sid := resp.Session.Session.SessionID
	sess := c.WithPrincipal(bg, c.Principal{Kind: c.PrincipalSession, ID: sid})
	send := func(text string) c.MessageView {
		t.Helper()
		m, err := st.Hub.SendMessage(sess, c.SendMessageRequest{Mutation: op(), Recipient: c.Address{Kind: c.AddrOwner, ID: "owner"}, Body: c.MessageBody{Text: text}})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	unacked := func() map[string]bool {
		t.Helper()
		page, err := st.Hub.ListMessages(owner, c.ListMessagesRequest{Unacked: true})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, m := range page.Items {
			out[m.Envelope.MessageID] = true
		}
		return out
	}
	items := func() []c.AttentionItem {
		t.Helper()
		page, err := st.Hub.ListAttention(owner, c.ListAttentionRequest{SessionID: sid})
		if err != nil {
			t.Fatal(err)
		}
		return page.Items
	}

	// 1. A report that asks nothing: stored as an update, no item, no flag.
	upd := send("Synthetic digest: merged two PRs, deployed at 1:15pm PT. Nothing urgent.")
	if upd.Status.Ack != c.AckAcknowledged || upd.Status.AckRef != c.AckRefUpdate || upd.Delivery == nil || !strings.Contains(upd.Delivery.Summary, "update") {
		t.Fatalf("update: %+v %+v", upd.Status, upd.Delivery)
	}
	if unacked()[upd.Envelope.MessageID] {
		t.Fatal("an update is unacked owner mail")
	}

	// 2. A question: in the inbox, an item, and the sender is pointed to asks.
	q := send("The synthetic export fix is ready. Should I merge it?")
	if q.Status.Ack != c.AckNone || q.Delivery == nil || !strings.Contains(q.Delivery.Summary, "ask_owner") {
		t.Fatalf("question: %+v %+v", q.Status, q.Delivery)
	}
	waitFor(t, "question item", func() bool { return len(items()) == 1 })
	for _, it := range items() {
		if it.Object.ID == upd.Envelope.MessageID {
			t.Fatalf("the update raised an item: %+v", it)
		}
	}

	// 3. An ask's carrier leaves the inbox when the ask is answered.
	ar, err := st.Hub.CreateAsk(sess, c.CreateAskRequest{Mutation: op(), TopicKey: "synthetic-rollout", Title: "Roll out the synthetic build?",
		Context:       "Synthetic context: the build is ready. " + strings.Repeat("Synthetic background a newcomer can follow: what, why now, what was tried, what each option does. ", 4),
		Options:       []c.AskOption{{ID: "a", Label: "Roll out", Detail: "today"}, {ID: "b", Label: "Hold", Detail: "one more day"}},
		DefaultOption: "b"})
	if err != nil || ar.Ask == nil {
		t.Fatalf("ask: %v %+v", err, ar)
	}
	carrier := ar.Ask.MessageID
	if !unacked()[carrier] {
		t.Fatal("an open ask's carrier is waiting on the owner")
	}
	if _, err := st.Hub.AnswerAsks(owner, c.AnswerAsksRequest{Mutation: op(), Answers: []c.AnswerAskItem{{AskID: ar.Ask.AskID, OptionID: "a"}}}); err != nil {
		t.Fatal(err)
	}
	if unacked()[carrier] {
		t.Fatal("an answered ask's carrier is still unacked")
	}

	// 4. The owner viewing the question acknowledges it.
	if _, err := st.Hub.GetMessage(owner, c.GetRequest{ID: q.Envelope.MessageID}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "viewed question acknowledged", func() bool { return len(unacked()) == 0 })

	// 5. Marking a message item done (the phone's Done) acknowledges its mail;
	// another thread's stays.
	q2 := send("Should I rotate the synthetic key?")
	thr, err := st.Hub.SendMessage(sess, c.SendMessageRequest{Mutation: op(), Recipient: c.Address{Kind: c.AddrOwner, ID: "owner"}, ThreadID: "thr_synth-region",
		Body: c.MessageBody{Text: "Which synthetic region should I use?"}})
	if err != nil {
		t.Fatal(err)
	}
	var item c.AttentionItem
	waitFor(t, "items for both", func() bool {
		n := 0
		for _, it := range items() {
			if it.State == c.AttnOpen {
				n++
				if it.Object.Kind == c.KindMessage {
					item = it
				}
			}
		}
		return n == 2 && item.AttentionID != ""
	})
	if _, err := st.Hub.UpdateAttention(owner, c.UpdateAttentionRequest{Mutation: op(), AttentionID: item.AttentionID, State: c.AttnHandled}); err != nil {
		t.Fatal(err)
	}
	if got := unacked(); got[q2.Envelope.MessageID] || !got[thr.Envelope.MessageID] {
		t.Fatalf("after done: %v (done %s, other thread %s)", got, q2.Envelope.MessageID, thr.Envelope.MessageID)
	}
}
