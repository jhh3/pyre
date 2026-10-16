package mock

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/httpapi"
)

// Held answers and project pages on the mock, driven through httpapi with
// the real client (so the client's optional interfaces are exercised).
func TestWebInboxOverHTTP(t *testing.T) {
	b, clk, node := newEmpty(t)
	clk.Set(time.Date(2026, 10, 4, 17, 0, 0, 0, time.UTC)) // 10:00 PT: no quiet hours
	srv := httptest.NewServer(httpapi.NewServer(b, httpapi.NewStaticTokens(map[string]c.Principal{"dev-token": owner}), httpapi.Options{StreamWait: time.Second}))
	defer srv.Close()
	var api c.API = client.New(srv.URL, "dev-token", client.Options{RetryBase: time.Millisecond})
	held, ok := api.(c.HeldAnswersAPI)
	if !ok {
		t.Fatal("client is not a HeldAnswersAPI")
	}
	pages, ok := api.(c.ProjectPageAPI)
	if !ok {
		t.Fatal("client is not a ProjectPageAPI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	prj, err := b.CreateProject(as(owner), c.CreateProjectRequest{Mutation: op(), Name: "synthetic-alpha"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := b.StartSession(as(owner), c.StartSessionRequest{Mutation: op(), NodeID: node, Harness: c.HarnessMock, Cwd: "/tmp/x", ProjectID: &prj.ProjectID})
	if err != nil {
		t.Fatal(err)
	}
	b.Tick()
	sess := as(c.Principal{Kind: c.PrincipalSession, ID: resp.Session.Session.SessionID})
	a1, err := b.CreateAsk(sess, askReq("synthetic-one", "Pick the first synthetic plan", "a"))
	if err != nil || a1.Status != c.AskCreated {
		t.Fatalf("ask 1: %+v %v", a1, err)
	}
	a2, err := b.CreateAsk(sess, askReq("synthetic-two", "Choose the second synthetic rollout", "b"))
	if err != nil || a2.Status != c.AskCreated {
		t.Fatalf("ask 2: %+v %v", a2, err)
	}

	// Hold, then the quiet window sends the batch.
	if _, err := held.HoldAnswer(ctx, c.HoldAnswerRequest{AnswerAskItem: c.AnswerAskItem{AskID: a1.Ask.AskID, OptionID: "nope"}}); !c.IsCode(err, c.ErrInvalid) {
		t.Fatalf("bad option: %v", err)
	}
	h, err := held.HoldAnswer(ctx, c.HoldAnswerRequest{AnswerAskItem: c.AnswerAskItem{AskID: a1.Ask.AskID, OptionID: "b"}})
	if err != nil || len(h.Items) != 1 || h.SendsAt == nil {
		t.Fatalf("hold: %+v %v", h, err)
	}
	if h, err = held.HoldAnswer(ctx, c.HoldAnswerRequest{AnswerAskItem: c.AnswerAskItem{AskID: a2.Ask.AskID, Text: "synthetic: later"}}); err != nil || len(h.Items) != 2 {
		t.Fatalf("hold 2: %+v %v", h, err)
	}
	if h, err = held.UnholdAnswer(ctx, c.UnholdAnswerRequest{AskID: a2.Ask.AskID}); err != nil || len(h.Items) != 1 {
		t.Fatalf("unhold: %+v %v", h, err)
	}
	clk.Advance(c.AskBatchQuiet)
	if h, err = held.ListHeldAnswers(ctx, struct{}{}); err != nil || len(h.Items) != 0 {
		t.Fatalf("after the window: %+v %v", h, err)
	}
	if g, _ := b.GetAsk(as(owner), c.GetRequest{ID: a1.Ask.AskID}); g.State != c.AskAnswered || g.Answer.OptionID != "b" || g.DecisionMessageID == "" {
		t.Fatalf("a1 after the window: %+v", g)
	}
	// Flush sends now; a held answer for a decided ask is dropped.
	if _, err := held.HoldAnswer(ctx, c.HoldAnswerRequest{AnswerAskItem: c.AnswerAskItem{AskID: a2.Ask.AskID, OptionID: "a"}}); err != nil {
		t.Fatal(err)
	}
	fr, err := held.FlushHeldAnswers(ctx, c.FlushHeldAnswersRequest{})
	if err != nil || len(fr.Results) != 1 || fr.Results[0].Error != nil || fr.Results[0].Ask.State != c.AskAnswered {
		t.Fatalf("flush: %+v %v", fr, err)
	}

	// Held answer wins over the deadline default.
	a3, err := b.CreateAsk(sess, askReq("synthetic-three", "Select the third synthetic option", "b"))
	if err != nil || a3.Status != c.AskCreated {
		t.Fatalf("ask 3: %+v %v", a3, err)
	}
	if _, err := held.HoldAnswer(ctx, c.HoldAnswerRequest{AnswerAskItem: c.AnswerAskItem{AskID: a3.Ask.AskID, OptionID: "a"}}); err != nil {
		t.Fatal(err)
	}
	clk.Set(*a3.Ask.DeadlineAt)
	if g, _ := b.GetAsk(as(owner), c.GetRequest{ID: a3.Ask.AskID}); g.State != c.AskAnswered || g.Answer.OptionID != "a" || g.Answer.ByDefault {
		t.Fatalf("deadline with a held answer: %+v", g.Answer)
	}

	// Project page: the session writes, the owner reads over HTTP.
	if pg, err := pages.GetProjectPage(ctx, c.GetProjectPageRequest{ProjectID: prj.ProjectID}); err != nil || pg.Revision != nil {
		t.Fatalf("empty page: %+v %v", pg, err)
	}
	if _, err := b.SetProjectPage(sess, c.SetProjectPageRequest{Mutation: op(), Content: "# Synthetic alpha"}); err != nil {
		t.Fatal(err)
	}
	pg, err := pages.SetProjectPage(ctx, c.SetProjectPageRequest{ProjectID: prj.ProjectID, Content: "# Synthetic alpha\n\nOwner edit."})
	if err != nil || pg.Revision == nil || pg.Revision.Seq != 2 {
		t.Fatalf("owner write: %+v %v", pg, err)
	}
	if got, err := pages.GetProjectPage(ctx, c.GetProjectPageRequest{ProjectID: prj.ProjectID}); err != nil || got.Content != "# Synthetic alpha\n\nOwner edit." {
		t.Fatalf("read: %+v %v", got, err)
	}
	// Short asks are rejected with guidance.
	short := askReq("synthetic-four", "Short synthetic ask", "a")
	short.Context = "too short"
	if _, err := b.CreateAsk(sess, short); !c.IsCode(err, c.ErrInvalid) {
		t.Fatalf("short ask: %v", err)
	}
}
