package mock

import (
	"context"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

func askReq(topic, title string, def string) c.CreateAskRequest {
	return c.CreateAskRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, TopicKey: topic, Title: title,
		Context: "Synthetic context. " + strings.Repeat("Synthetic background a newcomer can follow: what, why now, what was tried, what each option does. ", 4), DefaultOption: def,
		Options: []c.AskOption{{ID: "a", Label: "Alpha", Detail: "the alpha path"}, {ID: "b", Label: "Beta", Detail: "the beta path"}}}
}

func TestAsksMergeDecideAndDefault(t *testing.T) {
	b, clk := newFixtureBackend(t)
	// 17:00 UTC is 10:00 in Los Angeles: outside quiet hours.
	clk.Set(time.Date(2026, 10, 4, 17, 0, 0, 0, time.UTC))
	sess := c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalSession, ID: "ses_synth-a"})
	owner := as(c.Principal{Kind: c.PrincipalOwner, ID: "cli_synth"})

	r, err := b.CreateAsk(sess, askReq("Rollout Plan", "Pick the synthetic rollout plan", "a"))
	if err != nil || r.Status != c.AskCreated || r.Ask.TopicKey != "rollout-plan" || r.Ask.DeadlineRule != "1h" {
		t.Fatalf("create: %v %+v", err, r)
	}
	first := *r.Ask
	if first.AttentionID == "" || first.MessageID == "" {
		t.Fatalf("an ask gets an attention item and a carrying message: %+v", first)
	}
	// The same topic merges; nothing new for the owner.
	r, err = b.CreateAsk(sess, askReq("rollout-plan", "Pick the synthetic rollout plan", "a"))
	if err != nil || r.Status != c.AskMerged || r.Ask.AskID != first.AskID || r.Ask.Count != 2 || len(r.Ask.Updates) != 1 {
		t.Fatalf("merge: %v %+v", err, r)
	}
	// A similar title under another topic is a possible duplicate.
	r, err = b.CreateAsk(sess, askReq("plan-for-rollout", "Pick the synthetic rollout plan now", "a"))
	if err != nil || r.Status != c.AskPossibleDuplicate || len(r.Candidates) != 1 {
		t.Fatalf("duplicate: %v %+v", err, r)
	}
	r, err = b.CreateAsk(sess, askReq("db-migration", "Run the synthetic migration?", ""))
	if err != nil || r.Status != c.AskCreated {
		t.Fatalf("second: %v %+v", err, r)
	}
	second := *r.Ask

	// One batch answer; a second answer to the same ask conflicts.
	resp, err := b.AnswerAsks(owner, c.AnswerAsksRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)},
		Answers: []c.AnswerAskItem{{AskID: second.AskID, OptionID: "b"}}})
	if err != nil || len(resp.Results) != 1 || resp.Results[0].Error != nil || resp.Results[0].Ask.State != c.AskAnswered {
		t.Fatalf("answer: %v %+v", err, resp)
	}
	resp, _ = b.AnswerAsks(owner, c.AnswerAsksRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)},
		Answers: []c.AnswerAskItem{{AskID: second.AskID, OptionID: "a"}}})
	if resp.Results[0].Error == nil || resp.Results[0].Error.Code != c.ErrConflict {
		t.Fatalf("second answer conflicts: %+v", resp.Results[0])
	}
	// The decided topic answers with the decision instead of asking again.
	r, _ = b.CreateAsk(sess, askReq("db-migration", "Run the synthetic migration?", ""))
	if r.Status != c.AskAlreadyDecided || r.Ask.Answer.OptionID != "b" {
		t.Fatalf("decided: %+v", r)
	}

	// The deadline applies the default.
	clk.Advance(61 * time.Minute)
	got, err := b.GetAsk(owner, c.GetRequest{ID: first.AskID})
	if err != nil || got.State != c.AskDefaulted || got.Answer == nil || !got.Answer.ByDefault || got.Answer.OptionID != "a" || got.DecisionMessageID == "" {
		t.Fatalf("defaulted: %v %+v", err, got)
	}
	att, _ := b.ListAttention(owner, c.ListAttentionRequest{Kinds: []c.AttentionKind{c.AttnAsk}})
	if len(att.Items) != 0 {
		t.Fatalf("decided asks leave the inbox: %+v", att.Items)
	}
}

func TestAsksQuietHoursAndChildRedirect(t *testing.T) {
	b, clk := newFixtureBackend(t)
	clk.Set(time.Date(2026, 10, 5, 5, 30, 0, 0, time.UTC)) // 22:30 PDT
	sess := c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalSession, ID: "ses_synth-a"})
	r, err := b.CreateAsk(sess, askReq("night", "A synthetic question at night", "a"))
	if err != nil || r.Ask.DeadlineRule != "morning" {
		t.Fatalf("night ask: %v %+v", err, r)
	}
	if want := time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC); !r.Ask.DeadlineAt.Equal(want) { // 08:00 PDT
		t.Fatalf("deadline %v, want %v", r.Ask.DeadlineAt, want)
	}
	urgent := askReq("urgent", "An urgent synthetic question", "a")
	urgent.Urgent = true
	r, _ = b.CreateAsk(sess, urgent)
	if r.Ask.DeadlineRule != "urgent" {
		t.Fatalf("urgent keeps its deadline: %+v", r.Ask)
	}

	// A child session's ask goes to its parent.
	b.mu.Lock()
	b.sessions["ses_synth-b"].Lineage.ParentSessionID = "ses_synth-a"
	b.mu.Unlock()
	child := c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalSession, ID: "ses_synth-b"})
	r, err = b.CreateAsk(child, askReq("child", "A synthetic child question", "a"))
	if err != nil || r.Status != c.AskRedirected || r.MessageID == "" || r.Ask != nil {
		t.Fatalf("redirect: %v %+v", err, r)
	}
}
