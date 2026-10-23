package assemble

import (
	"context"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
)

// "Waiting on you" end to end through the assembled hub (real attention
// store and deriver, simulated node whose transcript ends with a question).
// Live case 2026-10-14: five idle leads had open decisions for the owner at
// the end of their chat turns and no inbox item or needs-you flag.
func TestWaitingOnYouEndToEnd(t *testing.T) {
	st, n := newStack(t)
	bg := context.Background()
	owner := c.WithPrincipal(bg, ownerP)
	op := func() c.Mutation { return c.Mutation{OperationID: c.NewID(c.PrefixOperation)} }
	const question = "Still waiting on you: should I build the synthetic voice-match score?"
	n.mu.Lock()
	n.lastText = func(string) string { return "Synthetic report is ready.\n\n" + question + "\nDefault: skip it." }
	n.mu.Unlock()

	start := func(ctx context.Context, title string) (sid, runID string) {
		t.Helper()
		resp, err := st.Hub.StartSession(ctx, c.StartSessionRequest{Mutation: op(), NodeID: n.nodeID, Harness: c.HarnessMock, Cwd: "/tmp/synth", Title: title})
		if err != nil {
			t.Fatal(err)
		}
		n.idle()
		sid = resp.Session.Session.SessionID
		waitFor(t, "run of "+title, func() bool {
			sv, err := st.Hub.GetSession(owner, c.GetRequest{ID: sid})
			if err == nil && sv.Run != nil {
				runID = sv.Run.RunID
			}
			return runID != ""
		})
		return sid, runID
	}
	sid, runID := start(owner, "synth lead")
	// idle ends a turn and runs the hub's turn-end checks (the stack's
	// background workers are not started in these tests).
	idle := func(id string) {
		t.Helper()
		if err := n.SetRunState(bg, id, c.RunWaitingInput); err != nil {
			t.Fatal(err)
		}
		n.idle()
		if _, err := st.Hub.ProcessChildReports(bg); err != nil {
			t.Fatal(err)
		}
	}
	turn := func(id string) {
		t.Helper()
		if err := n.SetRunState(bg, id, c.RunRunning); err != nil {
			t.Fatal(err)
		}
		idle(id)
	}
	items := func(id string, states ...c.AttentionState) []c.AttentionItem {
		t.Helper()
		page, err := st.Hub.ListAttention(owner, c.ListAttentionRequest{SessionID: id, Kinds: []c.AttentionKind{c.AttnWaiting},
			States: append(states, c.AttnOpen, c.AttnRead)})
		if err != nil {
			t.Fatal(err)
		}
		return page.Items
	}
	all := []c.AttentionState{c.AttnHandled, c.AttnWithdrawn}
	flag := func() *c.SessionFlag {
		t.Helper()
		sv, err := st.Hub.GetSession(owner, c.GetRequest{ID: sid})
		if err != nil {
			t.Fatal(err)
		}
		return sv.Flag
	}
	open := func(what string, count int) c.AttentionItem {
		t.Helper()
		var it c.AttentionItem
		waitFor(t, what, func() bool {
			got := items(sid)
			if len(got) == 1 && got[0].Count == count {
				it = got[0]
				return true
			}
			return false
		})
		return it
	}
	// resolved: nothing active, the newest item in state.
	resolved := func(what string, state c.AttentionState) {
		t.Helper()
		waitFor(t, what, func() bool {
			got := items(sid, all...)
			return len(got) > 0 && len(items(sid)) == 0 && got[0].State == state && flag() == nil
		})
	}

	// 1. A lead's turn ends with a question: one item, needs-you flag.
	turn(runID)
	it := open("waiting item", 1)
	if it.Title != "Waiting on you: "+question || it.Object != (c.ObjectRef{Kind: c.KindSession, ID: sid}) || it.Severity != "normal" ||
		!strings.Contains(it.Summary, "Default: skip it.") {
		t.Fatalf("item: %+v", it)
	}
	if f := flag(); f == nil || f.Level != c.FlagNeedsYou || f.Reason != "waiting" || f.Text != it.Title {
		t.Fatalf("flag: %+v", f)
	}

	// 2. A child session's turn never raises one (it reports to its lead).
	child, childRun := start(c.WithPrincipal(bg, c.Principal{Kind: c.PrincipalSession, ID: sid}), "synth child")
	turn(childRun)

	// 3. The owner typing into the session handles it.
	l, err := st.Hub.AcquireLease(owner, c.LeaseRequest{Mutation: op(), RunID: runID, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	resolved("typing handles it", c.AttnHandled)
	if _, err := st.Hub.ReleaseLease(owner, c.LeaseReleaseRequest{Mutation: op(), LeaseID: l.LeaseID}); err != nil {
		t.Fatal(err)
	}

	// 4. The next turn only repeats the request (owner report 2026-10-19:
	// idle turns re-raised one 27 times): handled stays handled.
	n.mu.Lock()
	n.lastText = func(string) string { return "Nothing new from me.\n\n" + question }
	n.mu.Unlock()
	turn(runID)
	resolved("a repeat leaves it handled", c.AttnHandled)
	// A new question is a new item, its own conversation (owner report
	// 2026-10-19: one item reopened for days showed the whole history as
	// one thread); the handled one stays handled.
	const newQuestion = "Should I publish the synthetic report to the staging page?"
	n.mu.Lock()
	n.lastText = func(string) string { return "Score built.\n\n" + newQuestion }
	n.mu.Unlock()
	turn(runID)
	again := open("new question", 1)
	if again.AttentionID == it.AttentionID || again.Title != "Waiting on you: "+newQuestion {
		t.Fatalf("new question reused the old item: %+v vs %+v", again, it)
	}
	if old, err := st.Hub.GetAttention(owner, c.GetRequest{ID: it.AttentionID}); err != nil || old.State != c.AttnHandled {
		t.Fatalf("old item: %+v %v", old, err)
	}
	if got := func() []c.AttentionItem {
		page, _ := st.Hub.ListAttention(owner, c.ListAttentionRequest{SessionID: child, States: []c.AttentionState{c.AttnOpen, c.AttnRead, c.AttnHandled, c.AttnWithdrawn}})
		return page.Items
	}(); len(got) != 0 {
		t.Fatalf("child session got owner attention: %+v", got)
	}

	// 5. Its next turn starting withdraws it.
	if err := n.SetRunState(bg, runID, c.RunRunning); err != nil {
		t.Fatal(err)
	}
	resolved("next turn withdraws it", c.AttnWithdrawn)

	// 6. The turn ends on the same question: it comes back, not counted and
	// without a push.
	idle(runID)
	if back := open("same question back", 1); back.State != c.AttnOpen || back.AttentionID != again.AttentionID {
		t.Fatalf("repeat after the next turn: %+v", back)
	}
	// Filing an ask withdraws it (the ask is the inbox item now).
	sess := c.WithPrincipal(bg, c.Principal{Kind: c.PrincipalSession, ID: sid})
	if _, err := st.Hub.CreateAsk(sess, c.CreateAskRequest{Mutation: op(), TopicKey: "synthetic-voice-match", Title: "Build the voice-match score?",
		Context:       "Synthetic context: the report is ready; the score costs a day. " + strings.Repeat("Synthetic background a newcomer can follow: what, why now, what was tried, what each option does. ", 4),
		Options:       []c.AskOption{{ID: "a", Label: "Build it", Detail: "costs a day"}, {ID: "b", Label: "Skip it", Detail: "report ships without it"}},
		DefaultOption: "b"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "ask withdraws it", func() bool {
		got := items(sid, all...)
		f := flag()
		return len(got) == 2 && got[0].AttentionID == again.AttentionID && got[0].State == c.AttnWithdrawn && f != nil && f.Reason == "ask"
	})
}
