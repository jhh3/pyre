package assemble

import (
	"context"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
)

// W-asks through the assembled hub (real attention store and deriver): an
// ask is one "ask" inbox item (its carrying message raises none), flags its
// session "needs you", and answering it handles the item.
func TestAskAttentionEndToEnd(t *testing.T) {
	st, n := newStack(t)
	bg := context.Background()
	owner := c.WithPrincipal(bg, ownerP)
	op := func() c.Mutation { return c.Mutation{OperationID: c.NewID(c.PrefixOperation)} }
	resp, err := st.Hub.StartSession(owner, c.StartSessionRequest{Mutation: op(), NodeID: n.nodeID, Harness: c.HarnessMock, Cwd: "/tmp/synth", Title: "synthetic lead"})
	if err != nil {
		t.Fatal(err)
	}
	n.idle()
	sid := resp.Session.Session.SessionID
	sess := c.WithPrincipal(bg, c.Principal{Kind: c.PrincipalSession, ID: sid})
	cr, err := st.Hub.CreateAsk(sess, c.CreateAskRequest{Mutation: op(), TopicKey: "synthetic-deploy", Title: "Ship the synthetic build?",
		Context: "Synthetic: the build is green; shipping now or after the soak. " + strings.Repeat("Synthetic background a newcomer can follow: what, why now, what was tried, what each option does. ", 4), DefaultOption: "b",
		Options: []c.AskOption{{ID: "a", Label: "Ship now", Detail: "release today"}, {ID: "b", Label: "After the soak", Detail: "release tomorrow"}}})
	if err != nil || cr.Status != c.AskCreated {
		t.Fatalf("create: %+v %v", cr, err)
	}
	items := func(states ...c.AttentionState) []c.AttentionItem {
		t.Helper()
		p, err := st.Hub.ListAttention(owner, c.ListAttentionRequest{SessionID: sid, States: states})
		if err != nil {
			t.Fatal(err)
		}
		return p.Items
	}
	waitFor(t, "ask item", func() bool { return len(items()) > 0 })
	open := items()
	if len(open) != 1 || open[0].Kind != c.AttnAsk || open[0].Object.ID != cr.Ask.AskID {
		t.Fatalf("inbox: %+v", open)
	}
	if a, err := st.Hub.GetAsk(owner, c.GetRequest{ID: cr.Ask.AskID}); err != nil || a.AttentionID != open[0].AttentionID {
		t.Fatalf("ask attention id: %q %v (want %q)", a.AttentionID, err, open[0].AttentionID)
	}
	sv, err := st.Hub.GetSession(owner, c.GetRequest{ID: sid})
	if err != nil || sv.Flag == nil || sv.Flag.Level != c.FlagNeedsYou || sv.Flag.Reason != "ask" {
		t.Fatalf("flag: %+v %v", sv.Flag, err)
	}
	res, err := st.Hub.AnswerAsks(owner, c.AnswerAsksRequest{Mutation: op(), Answers: []c.AnswerAskItem{{AskID: cr.Ask.AskID, OptionID: "a"}}})
	if err != nil || res.Results[0].Error != nil {
		t.Fatalf("answer: %+v %v", res, err)
	}
	waitFor(t, "ask handled", func() bool {
		h := items(c.AttnHandled)
		return len(h) == 1 && h[0].Kind == c.AttnAsk
	})
	if left := items(); len(left) != 0 {
		t.Fatalf("open after answer: %+v", left)
	}
}
