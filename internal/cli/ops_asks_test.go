package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// fakeAsks is an API offering only the asks ops (W-asks); every other
// method panics (nil embedded API), so a test notices a stray call.
type fakeAsks struct {
	c.API
	created   []c.CreateAskRequest
	listed    []c.ListAsksRequest
	answered  []c.AnswerAsksRequest
	withdrawn []c.WithdrawAskRequest
	create    c.CreateAskResponse
	asks      []c.Ask
	answerErr map[string]*c.Error
}

func (f *fakeAsks) CreateAsk(_ context.Context, req c.CreateAskRequest) (c.CreateAskResponse, error) {
	f.created = append(f.created, req)
	return f.create, nil
}

func (f *fakeAsks) ListAsks(_ context.Context, req c.ListAsksRequest) (c.Page[c.Ask], error) {
	f.listed = append(f.listed, req)
	return c.Page[c.Ask]{Items: f.asks}, nil
}

func (f *fakeAsks) GetAsk(_ context.Context, req c.GetRequest) (c.Ask, error) {
	for _, a := range f.asks {
		if a.AskID == req.ID {
			return a, nil
		}
	}
	return c.Ask{}, c.Errorf(c.ErrNotFound, "no ask %s", req.ID)
}

func (f *fakeAsks) AnswerAsks(_ context.Context, req c.AnswerAsksRequest) (c.AnswerAsksResponse, error) {
	f.answered = append(f.answered, req)
	var out c.AnswerAsksResponse
	for _, it := range req.Answers {
		if e := f.answerErr[it.AskID]; e != nil {
			out.Results = append(out.Results, c.AnswerAskResult{AskID: it.AskID, Error: e})
			continue
		}
		a := synthAsk(it.AskID, c.AskAnswered)
		a.Answer = &c.AskAnswer{OptionID: it.OptionID, Text: it.Text, By: testOwner, At: time.Now()}
		out.Results = append(out.Results, c.AnswerAskResult{AskID: it.AskID, Ask: &a})
	}
	return out, nil
}

func (f *fakeAsks) WithdrawAsk(_ context.Context, req c.WithdrawAskRequest) (c.Ask, error) {
	f.withdrawn = append(f.withdrawn, req)
	st := c.AskWithdrawn
	if req.SupersededBy != "" {
		st = c.AskSuperseded
	}
	a := synthAsk(req.AskID, st)
	a.SupersededBy = req.SupersededBy
	return a, nil
}

func synthAsk(id string, st c.AskState) c.Ask {
	deadline := time.Now().Add(58 * time.Minute)
	return c.Ask{AskID: id, TopicKey: "synth-db-migration", SessionID: "ses_synth-a", Title: "Which synthetic migration path?",
		Context: "Synthetic context: two paths.", State: st, DefaultOption: "b", DeadlineAt: &deadline, DeadlineRule: "1h", Count: 1,
		Options:   []c.AskOption{{ID: "a", Label: "Rewrite"}, {ID: "b", Label: "Add a column", Detail: "reversible"}},
		MessageID: "msg_synth-ask", CreatedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now()}
}

func askTool(t *testing.T, name string) MCPTool {
	t.Helper()
	for _, tl := range MCPTools() {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("no MCP tool %s", name)
	return MCPTool{}
}

func callAskTool(t *testing.T, f c.API, name, args string) (string, error) {
	t.Helper()
	out, err := askTool(t, name).run(context.Background(), &Ops{API: f}, json.RawMessage(args))
	if err != nil {
		return "", err
	}
	s, ok := out.(compactText)
	if !ok {
		t.Fatalf("%s: not compact text: %T", name, out)
	}
	return string(s), nil
}

func TestAskOwnerToolRequestAndStatuses(t *testing.T) {
	f := &fakeAsks{}
	a := synthAsk("ask_synth-1", c.AskOpen)
	f.create = c.CreateAskResponse{Status: c.AskCreated, Ask: &a}
	args := `{"topic_key":" synth-db-migration ","title":"Which synthetic migration path?","context":"Synthetic context.",
		"options":[{"label":"Rewrite"},{"label":"Add a column","detail":"reversible"}],"default":"add a column","deadline_minutes":30}`
	text, err := callAskTool(t, f, "ask_owner", args)
	if err != nil {
		t.Fatal(err)
	}
	req := f.created[0]
	if req.TopicKey != "synth-db-migration" || req.DefaultOption != "b" || req.DeadlineSeconds != 1800 || req.Options[0].ID != "a" ||
		req.Options[1].ID != "b" || req.OperationID == "" {
		t.Fatalf("request: %+v", req)
	}
	for _, want := range []string{"asked ask_synth-1", `defaults to b "Add a column" at `, "(in 5", "reply to msg_synth-ask", "wait_reply msg_synth-ask", "keep working"} {
		if !strings.Contains(text, want) {
			t.Errorf("created text lacks %q:\n%s", want, text)
		}
	}

	merged := a
	merged.Count = 3
	f.create = c.CreateAskResponse{Status: c.AskMerged, Ask: &merged}
	if text, _ = callAskTool(t, f, "ask_owner", args); !strings.Contains(text, "merged into the open ask ask_synth-1") || !strings.Contains(text, "3 times") ||
		!strings.Contains(text, "not notified again") {
		t.Errorf("merged:\n%s", text)
	}

	decided := synthAsk("ask_synth-1", c.AskAnswered)
	decided.Answer = &c.AskAnswer{OptionID: "a", Text: "rewrite it properly", By: testOwner, At: time.Now().Add(-2 * time.Hour)}
	f.create = c.CreateAskResponse{Status: c.AskAlreadyDecided, Ask: &decided}
	text, _ = callAskTool(t, f, "ask_owner", args)
	for _, want := range []string{"already decided, nothing sent", `answered a "Rewrite": rewrite it properly`, "2h ago", "do not ask again", "reopen: true"} {
		if !strings.Contains(text, want) {
			t.Errorf("decided text lacks %q:\n%s", want, text)
		}
	}

	other := synthAsk("ask_synth-2", c.AskOpen)
	other.TopicKey = "synth-schema-change"
	f.create = c.CreateAskResponse{Status: c.AskPossibleDuplicate, Candidates: []c.Ask{other}}
	text, _ = callAskTool(t, f, "ask_owner", args)
	for _, want := range []string{"not sent", "ask_synth-2 · topic synth-schema-change", "confirm_new: true"} {
		if !strings.Contains(text, want) {
			t.Errorf("duplicate text lacks %q:\n%s", want, text)
		}
	}

	f.create = c.CreateAskResponse{Status: c.AskRedirected, MessageID: "msg_synth-to-parent"}
	if text, _ = callAskTool(t, f, "ask_owner", args); !strings.Contains(text, "only top-level sessions ask the owner") || !strings.Contains(text, "wait_reply msg_synth-to-parent") {
		t.Errorf("redirected:\n%s", text)
	}

	// format json returns the contract result.
	out, err := askTool(t, "ask_owner").run(context.Background(), &Ops{API: f}, json.RawMessage(strings.Replace(args, "{", `{"format":"json",`, 1)))
	if fr, ok := out.(fullResult); err != nil || !ok || fr.v.(c.CreateAskResponse).Status != c.AskRedirected {
		t.Fatalf("json: %T %v", out, err)
	}
}

func TestAskOwnerValidatesBeforeCalling(t *testing.T) {
	f := &fakeAsks{}
	for args, want := range map[string]string{
		`{"topic_key":"k","title":"t","options":[{"label":"x"},{"label":"y"}]}`:                                 "context is required",
		`{"topic_key":"","title":"t","context":"c","options":[{"label":"x"},{"label":"y"}]}`:                    "topic_key is required",
		`{"topic_key":"k","title":"t","context":"c","options":[{"label":"x"}]}`:                                 "2–4 options",
		`{"topic_key":"k","title":"t","context":"c","options":[{"label":"x"},{"label":"y"}],"default":"z"}`:     "not one of the options",
		`{"topic_key":"k","title":"t","context":"c","options":[{"id":"a","label":"x"},{"id":"a","label":"y"}]}`: "repeated",
		`{"topic_key":"k","title":"t","context":"c","options":[{"label":"x","why":"?"},{"label":"y"}]}`:         "unknown field",
	} {
		if _, err := callAskTool(t, f, "ask_owner", args); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", args, err, want)
		}
	}
	if len(f.created) != 0 {
		t.Fatalf("invalid asks reached the hub: %+v", f.created)
	}
}

func TestFindAsksDefaultsAndRendering(t *testing.T) {
	open := synthAsk("ask_synth-1", c.AskOpen)
	done := synthAsk("ask_synth-2", c.AskDefaulted)
	done.Answer = &c.AskAnswer{OptionID: "b", ByDefault: true, At: time.Now().Add(-3 * time.Hour)}
	f := &fakeAsks{asks: []c.Ask{open, done}}
	text, err := callAskTool(t, f, "find_asks", `{"topic_key":"synth-db-migration"}`)
	if err != nil {
		t.Fatal(err)
	}
	req := f.listed[0]
	if req.TopicKey != "synth-db-migration" || req.Since == nil || time.Since(*req.Since) < 6*24*time.Hour || len(req.States) != 0 {
		t.Fatalf("default request: %+v", req)
	}
	for _, want := range []string{`ask_synth-1 · topic synth-db-migration · open; defaults to b "Add a column" at `, `ask_synth-2 · topic synth-db-migration · defaulted b "Add a column" 3h ago`,
		"2 asks", "decided ones are final"} {
		if !strings.Contains(text, want) {
			t.Errorf("find text lacks %q:\n%s", want, text)
		}
	}
	if _, err := callAskTool(t, f, "find_asks", `{"query":"migration","include_decided":false}`); err != nil {
		t.Fatal(err)
	}
	if req := f.listed[1]; req.Since != nil || len(req.States) != 1 || req.States[0] != c.AskOpen || req.Query != "migration" {
		t.Fatalf("open-only request: %+v", req)
	}
	f.asks = nil
	if text, _ := callAskTool(t, f, "find_asks", `{"topic_key":"x"}`); !strings.Contains(text, "no matching asks") {
		t.Errorf("empty: %s", text)
	}
	if !askTool(t, "find_asks").Annotations.ReadOnlyHint {
		t.Error("find_asks must be read-only")
	}
}

func TestWithdrawAskTool(t *testing.T) {
	f := &fakeAsks{}
	text, err := callAskTool(t, f, "withdraw_ask", `{"ask_id":"ask_synth-1","reason":"fixed it myself","superseded_by":"ask_synth-2"}`)
	if err != nil {
		t.Fatal(err)
	}
	if w := f.withdrawn[0]; w.AskID != "ask_synth-1" || w.Reason != "fixed it myself" || w.SupersededBy != "ask_synth-2" {
		t.Fatalf("request: %+v", w)
	}
	if !strings.Contains(text, "superseded by ask_synth-2") || !strings.Contains(text, "closed") {
		t.Errorf("text: %s", text)
	}
	if _, err := callAskTool(t, f, "withdraw_ask", `{"ask_id":"msg_synth-1"}`); err == nil {
		t.Error("a message ID accepted as an ask")
	}
}

// A connection without the asks ops says so (exit 6) instead of failing
// obscurely.
func TestAskUnsupportedConnection(t *testing.T) {
	bare := struct{ c.API }{}
	for _, name := range []string{"ask_owner", "find_asks", "withdraw_ask"} {
		_, err := askTool(t, name).run(context.Background(), &Ops{API: bare}, json.RawMessage(`{"topic_key":"k","title":"t","context":"c","options":[{"label":"x"},{"label":"y"}]}`))
		if name == "find_asks" {
			_, err = askTool(t, name).run(context.Background(), &Ops{API: bare}, json.RawMessage(`{}`))
		}
		if name == "withdraw_ask" {
			_, err = askTool(t, name).run(context.Background(), &Ops{API: bare}, json.RawMessage(`{"ask_id":"ask_synth-1"}`))
		}
		if !c.IsCode(err, c.ErrUnsupported) {
			t.Errorf("%s: %v", name, err)
		}
	}
	r := run(t, newEnv(t, nil), &App{API: bare}, "ask", "ls")
	expectCode(t, r, ExitUnsupported)
}

func TestAskCLIAnswerBatchAndOwnerCommands(t *testing.T) {
	e := newEnv(t, nil)
	f := &fakeAsks{asks: []c.Ask{synthAsk("ask_synth-1", c.AskOpen)}}
	r := run(t, e, &App{API: f}, "ask", "answer", "ask_synth-1", "b", "ask_synth-2=a", "ask_synth-3", "--text", "ship it")
	expectCode(t, r, 0)
	if len(f.answered) != 1 {
		t.Fatalf("want one batch call, got %d", len(f.answered))
	}
	got := f.answered[0].Answers
	if len(got) != 3 || got[0] != (c.AnswerAskItem{AskID: "ask_synth-1", OptionID: "b", Text: "ship it"}) ||
		got[1].OptionID != "a" || got[2].OptionID != "" || got[2].Text != "ship it" {
		t.Fatalf("answers: %+v", got)
	}
	if !strings.Contains(r.stdout, `ask_synth-1: answered b "Add a column": ship it`) {
		t.Fatalf("output:\n%s", r)
	}

	// --option for every ask named without one; a failed item exits non-zero
	// after printing every result.
	f.answerErr = map[string]*c.Error{"ask_synth-2": {Code: c.ErrConflict, Message: "already defaulted to b"}}
	r = run(t, e, &App{API: f}, "--json", "ask", "answer", "ask_synth-1", "ask_synth-2", "--option", "a")
	expectCode(t, r, ExitConflict)
	resp := mustJSON[c.AnswerAsksResponse](t, r)
	if len(resp.Results) != 2 || resp.Results[1].Error == nil || f.answered[1].Answers[1].OptionID != "a" {
		t.Fatalf("partial: %+v", resp)
	}
	expectCode(t, run(t, e, &App{API: f}, "ask", "answer", "ask_synth-1"), ExitUsage) // no option, no text
	expectCode(t, run(t, e, &App{API: f}, "ask", "answer", "b", "ask_synth-1"), ExitUsage)

	r = run(t, e, &App{API: f}, "ask", "ls", "--open")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "ask_synth-1") || f.listed[0].States[0] != c.AskOpen {
		t.Fatalf("ls:\n%s", r)
	}
	r = run(t, e, &App{API: f}, "ask", "show", "ask_synth-1")
	expectCode(t, r, 0)
	for _, want := range []string{"Synthetic context: two paths.", "* b ", "Add a column — reversible", "pyre ask answer ask_synth-1"} {
		if !strings.Contains(r.stdout, want) {
			t.Fatalf("show lacks %q:\n%s", want, r)
		}
	}
	r = run(t, e, &App{API: f}, "ask", "withdraw", "ask_synth-1", "--reason", "obsolete")
	expectCode(t, r, 0)
	if f.withdrawn[0].Reason != "obsolete" || !strings.Contains(r.stdout, "withdrawn") {
		t.Fatalf("withdraw:\n%s", r)
	}

	a := synthAsk("ask_synth-9", c.AskOpen)
	f.create = c.CreateAskResponse{Status: c.AskCreated, Ask: &a}
	r = run(t, e, &App{API: f}, "ask", "new", "--topic", "synth-db-migration", "--title", "Which path?", "--context", "Synthetic.",
		"--option", "a=Rewrite", "--option", "b=Add a column|reversible", "--default", "b", "--deadline", "90m")
	expectCode(t, r, 0)
	if req := f.created[0]; len(req.Options) != 2 || req.Options[1] != (c.AskOption{ID: "b", Label: "Add a column", Detail: "reversible"}) ||
		req.DeadlineSeconds != 5400 || req.DefaultOption != "b" {
		t.Fatalf("new: %+v", req)
	}
	if !strings.Contains(r.stdout, "asked ask_synth-9") {
		t.Fatalf("new output:\n%s", r)
	}
	r = run(t, e, &App{API: f}, "ask", "find", "--topic", "synth-db-migration")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "ask_synth-1 · topic synth-db-migration") {
		t.Fatalf("find:\n%s", r)
	}
}

func TestAskWhenShowsZoneAndRelative(t *testing.T) {
	now := time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC) // 2 PM PDT
	d := now.Add(65 * time.Minute)
	if got := askWhen(&d, now); askZone.String() == "America/Los_Angeles" && got != "3:05 PM PDT (in 1h)" {
		t.Fatalf("askWhen: %q", got)
	}
	tomorrow := now.Add(19 * time.Hour)
	if got := askWhen(&tomorrow, now); !strings.Contains(got, "Tue ") {
		t.Fatalf("other day: %q", got)
	}
	if askWhen(nil, now) != "no deadline" {
		t.Fatal("nil deadline")
	}
}
