package web

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// Everything here is synthetic: a mock hub, made-up sessions and text.

var owner = c.Principal{Kind: c.PrincipalOwner, ID: "cli_synth-web-owner", Name: "synthetic owner"}

const (
	testEmail   = "owner@example.org"
	testProject = "prj_synth-web"
	testNode    = "node_synth-web"
	testOrigin  = "http://example.com" // httptest.NewRequest's Host
)

// plainHub is a hub without held answers or project pages (the mock
// itself has both).
type plainHub struct {
	c.API
	c.AsksAPI
}

type tenv struct {
	t     *testing.T
	b     *mock.Backend
	clk   *mock.FakeClock
	srv   *Server
	h     http.Handler
	state string
}

const artContent = "<h1>Synthetic report</h1><script>alert(1)</script>"

func newTestEnv(t *testing.T, full bool) *tenv {
	t.Helper()
	clk := mock.NewFakeClock(time.Date(2026, 10, 15, 17, 0, 0, 0, time.UTC)) // 10:00 AM PT
	now := clk.Now()
	node := c.Node{NodeID: testNode, Name: "synth-box", Platform: "linux", Arch: "amd64",
		Connection: c.NodeConnection{State: c.ConnConnected}, Harnesses: []c.HarnessInfo{}, Versions: map[string]string{}, Capabilities: []string{}}
	pid := testProject
	sum := sha256.Sum256([]byte(artContent))
	bundle := &c.FixtureBundle{SchemaVersion: c.SchemaVersion, Name: "synthetic web inbox", Synthetic: true,
		Nodes: []c.Node{node},
		Projects: []c.Project{{ProjectID: testProject, Name: "Synthetic web project", Description: "A made-up project.",
			Repos: []c.RepoBinding{}, Checkouts: []c.CheckoutBinding{}, CreatedBy: owner, CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-48 * time.Hour), Version: 1}},
		Artifacts: []c.Artifact{{ArtifactID: "art_synth-report", ProjectID: &pid, Title: "Synthetic report", LatestVersionID: "ver_synth-report1",
			VersionCount: 1, CreatedBy: owner, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}},
		Versions: []c.ArtifactVersion{{VersionID: "ver_synth-report1", ArtifactID: "art_synth-report", Seq: 1, Digest: c.Digest("sha256:" + hex.EncodeToString(sum[:])),
			Size: int64(len(artContent)), MediaType: "text/html", Filename: "report.html", Author: owner, CreatedAt: now.Add(-time.Hour)}},
		Contents: map[string]string{"ver_synth-report1": artContent},
	}
	b, err := mock.NewE(mock.Options{Clock: clk, FixtureDirs: []string{}, DefaultPrincipal: &owner, Fixtures: []*c.FixtureBundle{bundle}})
	if err != nil {
		t.Fatal(err)
	}
	e := &tenv{t: t, b: b, clk: clk, state: t.TempDir()}
	var api c.API = plainHub{API: b, AsksAPI: b}
	if full {
		api = b
	}
	s, err := New(Options{API: api, StateDir: e.state, Now: clk.Now, Debounce: 5 * time.Millisecond, Heartbeat: 40 * time.Millisecond,
		Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.Start(ctx)
	e.srv, e.h = s, s.Handler()
	return e
}

func sessCtx(id string) context.Context {
	return c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalSession, ID: id})
}

func ownerCtx() context.Context { return c.WithPrincipal(context.Background(), owner) }

func (e *tenv) start(title, parent string) string {
	e.t.Helper()
	pid := testProject
	r, err := e.b.StartSession(ownerCtx(), c.StartSessionRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)},
		NodeID: testNode, Harness: c.HarnessMock, Title: title, Cwd: "/tmp/synth", ProjectID: &pid, ParentSessionID: parent})
	if err != nil {
		e.t.Fatal(err)
	}
	e.b.Tick()
	return r.Session.Session.SessionID
}

func (e *tenv) ask(sid, topic, title string) c.Ask {
	e.t.Helper()
	r, err := e.b.CreateAsk(sessCtx(sid), c.CreateAskRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)},
		TopicKey: topic, Title: title, DefaultOption: "a",
		Context: "Synthetic context: which way should the synthetic rollout go? " + strings.Repeat("More synthetic background. ", 12),
		Options: []c.AskOption{{ID: "a", Label: "Alpha", Detail: "Do the alpha thing"}, {ID: "b", Label: "Beta", Detail: "Do the beta thing"}}})
	if err != nil || r.Ask == nil {
		e.t.Fatalf("ask: %v %+v", err, r)
	}
	return *r.Ask
}

func (e *tenv) mail(sid, text, replyTo string) c.MessageView {
	e.t.Helper()
	mv, err := e.b.SendMessage(sessCtx(sid), c.SendMessageRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)},
		Recipient: c.Address{Kind: c.AddrOwner, ID: "owner"}, Body: c.MessageBody{Text: text}, InReplyTo: replyTo})
	if err != nil {
		e.t.Fatal(err)
	}
	return mv
}

// req sends an authenticated request (JSON body for POST).
func (e *tenv) req(method, path string, body any, mod ...func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, path, rd)
	r.Header.Set(HeaderEmail, testEmail)
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", testOrigin)
	}
	for _, m := range mod {
		m(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func (e *tenv) json(method, path string, body any, out any) {
	e.t.Helper()
	w := e.req(method, path, body)
	if w.Code != http.StatusOK {
		e.t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
	}
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			e.t.Fatalf("%s %s: %v\n%s", method, path, err, w.Body)
		}
	}
}

type inboxResp struct {
	Threads []Thread      `json:"threads"`
	Held    c.HeldAnswers `json:"held"`
}

func (e *tenv) inbox(box string) inboxResp {
	e.t.Helper()
	var r inboxResp
	e.json("GET", "/api/inbox?box="+box, nil, &r)
	return r
}

type threadResp struct {
	Thread   Thread `json:"thread"`
	Messages []Msg  `json:"messages"`
}

func (e *tenv) thread(id string) threadResp {
	e.t.Helper()
	var r threadResp
	e.json("GET", "/api/threads/"+id, nil, &r)
	return r
}

func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil || b.Error.Message == "" {
		t.Fatalf("not an error body: %v %s", err, w.Body)
	}
	return b.Error.Code
}

// waitFor polls cond (events reach the server asynchronously).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func findThread(ts []Thread, id string) (Thread, bool) {
	for _, t := range ts {
		if t.ThreadID == id {
			return t, true
		}
	}
	return Thread{}, false
}

// ---- auth and request hygiene ----

func TestAuthAndSecurityHeaders(t *testing.T) {
	e := newTestEnv(t, false)
	cases := []struct {
		name, email, remote string
		dev                 bool
		code                int
	}{
		{"no header", "", "192.0.2.1:1", false, 401},
		{"no header from loopback without --dev", "", "127.0.0.1:5", false, 401},
		{"other domain", "someone@example.com", "192.0.2.1:1", false, 403},
		{"subdomain", "someone@evil.example.org", "192.0.2.1:1", false, 403},
		{"suffix trick", "someone@notexample.org", "192.0.2.1:1", false, 403},
		{"two at signs", "a@b@example.org", "192.0.2.1:1", false, 403},
		{"allowed", "Owner@example.org", "192.0.2.1:1", false, 200},
		{"dev loopback", "", "127.0.0.1:5", true, 200},
		{"dev but remote", "", "192.0.2.1:1", true, 401},
	}
	for _, tc := range cases {
		e.srv.opts.Dev = tc.dev
		r := httptest.NewRequest("GET", "/api/me", nil)
		r.RemoteAddr = tc.remote
		if tc.email != "" {
			r.Header.Set(HeaderEmail, tc.email)
		}
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Errorf("%s: got %d want %d (%s)", tc.name, w.Code, tc.code, w.Body)
		}
		if w.Header().Get("X-Frame-Options") != "DENY" || w.Header().Get("X-Content-Type-Options") != "nosniff" ||
			!strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self'") {
			t.Errorf("%s: security headers missing: %v", tc.name, w.Header())
		}
		if tc.code == 200 {
			var me map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &me)
			want := strings.ToLower(tc.email)
			if tc.email == "" {
				want = DevEmail
			}
			if me["email"] != want {
				t.Errorf("%s: me = %v", tc.name, me)
			}
		}
	}
	e.srv.opts.Dev = false
	// Two headers (a client-sent one surviving next to the policy's) are refused.
	r := httptest.NewRequest("GET", "/api/me", nil)
	r.Header.Add(HeaderEmail, testEmail)
	r.Header.Add(HeaderEmail, "x@example.org")
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Errorf("duplicate header: %d", w.Code)
	}
	// Static files need sign-in too, and unknown app routes get index.html.
	if w := e.req("GET", "/inbox/some-thread", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "<html") {
		t.Errorf("spa fallback: %d %s", w.Code, w.Body)
	}
	if w := e.req("GET", "/missing.js", nil); w.Code != 404 {
		t.Errorf("missing asset: %d", w.Code)
	}
	if w := e.req("GET", "/api/nope", nil); w.Code != 404 || errCode(t, w) != "not_found" {
		t.Errorf("unknown api: %d %s", w.Code, w.Body)
	}
}

func TestCSRFOnPosts(t *testing.T) {
	e := newTestEnv(t, false)
	path := "/api/threads/msg_synth-none/read"
	cases := []struct {
		name string
		mod  func(*http.Request)
		code int
	}{
		{"form content type", func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }, 400},
		{"no origin", func(r *http.Request) { r.Header.Del("Origin") }, 403},
		{"foreign origin", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, 403},
		{"cross-site fetch", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403},
		{"null origin", func(r *http.Request) { r.Header.Set("Origin", "null") }, 403},
		// Passes the checks; the thread does not exist.
		{"same origin", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") }, 404},
		{"public origin", func(r *http.Request) { r.Header.Set("Origin", DefaultOrigin); r.Host = "127.0.0.1:7390" }, 404},
	}
	for _, tc := range cases {
		w := e.req("POST", path, map[string]any{}, tc.mod)
		if w.Code != tc.code {
			t.Errorf("%s: got %d want %d (%s)", tc.name, w.Code, tc.code, w.Body)
		}
	}
	if w := e.req("DELETE", path, nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE: %d", w.Code)
	}
}

// ---- inbox, threads, replies ----

func TestInboxThreadsAndReplyThreading(t *testing.T) {
	e := newTestEnv(t, false)
	lead := e.start("synth lead", "")
	a := e.ask(lead, "synth-rollout", "Pick the synthetic rollout")
	m1 := e.mail(lead, "Synthetic status: tests pass.\nMore detail here.", "")

	open := e.inbox("open")
	if len(open.Threads) != 2 || open.Held.Items == nil {
		t.Fatalf("open inbox: %+v", open)
	}
	at, ok := findThread(open.Threads, askThreadID(a))
	if !ok || at.Kind != "ask" || at.Ask == nil || at.Ask.AskID != a.AskID || !at.Unread || !at.NeedsReply ||
		at.Session == nil || at.Session.Title != "synth lead" || at.Project == nil || at.Project.Name != "Synthetic web project" {
		t.Fatalf("ask thread: %+v", at)
	}
	mt, ok := findThread(open.Threads, m1.Envelope.MessageID)
	if !ok || mt.Kind != "message" || mt.Title != "Synthetic status: tests pass." || !mt.NeedsReply || !mt.Unread || mt.MessageCount != 1 {
		t.Fatalf("message thread: %+v", mt)
	}
	if done := e.inbox("done"); len(done.Threads) != 0 {
		t.Fatalf("done should be empty: %+v", done.Threads)
	}

	// The ask's thread shows the carrying message tagged with the ask.
	tr := e.thread(askThreadID(a))
	if tr.Thread.Ask == nil || len(tr.Messages) != 1 || tr.Messages[0].AskID != a.AskID || strings.HasPrefix(tr.Messages[0].Text, "[pyre ask]") ||
		tr.Messages[0].From.Kind != "session" || tr.Messages[0].From.Label != "synth lead" {
		t.Fatalf("ask thread view: %+v", tr)
	}

	// Reply to the message thread: a queue message to the session,
	// replying to its newest message, carrying the thread key.
	var rep struct {
		Message Msg `json:"message"`
	}
	e.json("POST", "/api/threads/"+m1.Envelope.MessageID+"/reply", map[string]string{"text": "Synthetic: can you say more?"}, &rep)
	if rep.Message.From.Kind != "owner" || rep.Message.ReplyTo != m1.Envelope.MessageID || rep.Message.Delivery.Summary == "" {
		t.Fatalf("reply: %+v", rep.Message)
	}
	sent, err := e.b.GetMessage(ownerCtx(), c.GetRequest{ID: rep.Message.MessageID})
	if err != nil || sent.Envelope.Recipient != (c.Address{Kind: c.AddrSession, ID: lead}) || sent.Envelope.Mode != c.ModeQueue ||
		sent.Envelope.InReplyTo != m1.Envelope.MessageID || sent.Envelope.ThreadID != m1.Envelope.MessageID ||
		sent.Envelope.ProjectID == nil || *sent.Envelope.ProjectID != testProject {
		t.Fatalf("sent: %v %+v", err, sent.Envelope)
	}
	// Answered: the thread leaves the open box.
	waitFor(t, "thread answered", func() bool {
		_, still := findThread(e.inbox("open").Threads, m1.Envelope.MessageID)
		return !still
	})
	if _, ok := findThread(e.inbox("done").Threads, m1.Envelope.MessageID); !ok {
		t.Fatal("answered thread should be in done")
	}

	// The agent answers with reply_to: same thread, open again.
	m2 := e.mail(lead, "Synthetic: here is more context.", rep.Message.MessageID)
	waitFor(t, "agent reply in thread", func() bool {
		th, ok := findThread(e.inbox("open").Threads, m1.Envelope.MessageID)
		return ok && th.MessageCount == 3 && strings.Contains(th.Preview, "more context")
	})
	tr = e.thread(m1.Envelope.MessageID)
	if len(tr.Messages) != 3 || tr.Messages[2].MessageID != m2.Envelope.MessageID {
		t.Fatalf("thread: %+v", tr.Messages)
	}
	if d := tr.Messages[1].Delivery; d.State != "replied" || !strings.Contains(d.Summary, "synth lead replied") {
		t.Fatalf("owner message delivery after the reply: %+v", d)
	}

	// Reply on the ask's thread: replies to the carrying message.
	e.json("POST", "/api/threads/"+askThreadID(a)+"/reply", map[string]string{"text": "Synthetic: what does beta cost?"}, &rep)
	sent, _ = e.b.GetMessage(ownerCtx(), c.GetRequest{ID: rep.Message.MessageID})
	if sent.Envelope.InReplyTo != a.MessageID || sent.Envelope.Recipient.ID != lead {
		t.Fatalf("ask reply: %+v", sent.Envelope)
	}
	waitFor(t, "ask thread grows", func() bool { return len(e.thread(askThreadID(a)).Messages) == 2 })

	// Empty text and unknown threads are refused plainly.
	if w := e.req("POST", "/api/threads/"+m1.Envelope.MessageID+"/reply", map[string]string{"text": "  "}); w.Code != 400 {
		t.Fatalf("empty reply: %d", w.Code)
	}
	if w := e.req("GET", "/api/threads/msg_synth-unknown", nil); w.Code != 404 {
		t.Fatalf("unknown thread: %d %s", w.Code, w.Body)
	}
}

func TestReadMarksPersistAndReachTheHub(t *testing.T) {
	e := newTestEnv(t, false)
	lead := e.start("synth lead", "")
	a := e.ask(lead, "synth-read", "Synthetic read check")
	m := e.mail(lead, "Synthetic hello.", "")
	for _, id := range []string{askThreadID(a), m.Envelope.MessageID} {
		e.json("POST", "/api/threads/"+id+"/read", map[string]any{}, nil)
	}
	waitFor(t, "threads read", func() bool {
		for _, th := range e.inbox("open").Threads {
			if th.Unread {
				return false
			}
		}
		return true
	})
	got, err := e.b.GetAttention(ownerCtx(), c.GetRequest{ID: a.AttentionID})
	if err != nil || got.State != c.AttnRead {
		t.Fatalf("ask attention should be read at the hub: %v %+v", err, got)
	}
	b, err := os.ReadFile(filepath.Join(e.state, "read.json"))
	if err != nil || !strings.Contains(string(b), m.Envelope.MessageID) {
		t.Fatalf("read state file: %v %s", err, b)
	}
	// A new message after the read is unread again.
	e.clk.Advance(time.Minute)
	e.mail(lead, "Synthetic follow-up.", m.Envelope.MessageID)
	waitFor(t, "unread again", func() bool {
		th, ok := findThread(e.inbox("open").Threads, m.Envelope.MessageID)
		return ok && th.Unread
	})
}

// ---- answers: now and held ----

func TestAnswerNowAndHoldUnsupported(t *testing.T) {
	e := newTestEnv(t, false)
	lead := e.start("synth lead", "")
	a := e.ask(lead, "synth-now", "Synthetic answer now")

	w := e.req("POST", "/api/asks/"+a.AskID+"/answer", map[string]string{"option_id": "b", "mode": "hold"})
	if w.Code != http.StatusNotImplemented || errCode(t, w) != "unsupported" {
		t.Fatalf("hold without hub support: %d %s", w.Code, w.Body)
	}
	if w := e.req("POST", "/api/held/send", map[string]any{}); w.Code != http.StatusNotImplemented {
		t.Fatalf("send held without support: %d", w.Code)
	}
	if w := e.req("POST", "/api/asks/"+a.AskID+"/answer", map[string]string{"mode": "now"}); w.Code != 400 {
		t.Fatalf("no option or text: %d", w.Code)
	}
	var r struct {
		Ask  c.Ask         `json:"ask"`
		Held c.HeldAnswers `json:"held"`
	}
	e.json("POST", "/api/asks/"+a.AskID+"/answer", map[string]string{"option_id": "b"}, &r)
	if r.Ask.State != c.AskAnswered || r.Ask.Answer == nil || r.Ask.Answer.OptionID != "b" || r.Held.Items == nil {
		t.Fatalf("answer now: %+v", r)
	}
	waitFor(t, "decided ask leaves open", func() bool {
		_, ok := findThread(e.inbox("open").Threads, askThreadID(a))
		return !ok
	})
	if th, ok := findThread(e.inbox("done").Threads, askThreadID(a)); !ok || th.Ask.State != c.AskAnswered {
		t.Fatalf("done: %+v", th)
	}
	// Answering again is a plain conflict.
	if w := e.req("POST", "/api/asks/"+a.AskID+"/answer", map[string]string{"option_id": "a"}); w.Code != http.StatusConflict {
		t.Fatalf("second answer: %d %s", w.Code, w.Body)
	}
	// The decision shows in the thread, tagged with the ask.
	tr := e.thread(askThreadID(a))
	if len(tr.Messages) != 2 || tr.Messages[1].AskID != a.AskID || tr.Messages[1].From.Kind != "owner" || tr.Messages[1].Delivery.State == "" {
		t.Fatalf("decision in thread: %+v", tr.Messages)
	}
}

func TestHoldUndoAndSend(t *testing.T) {
	e := newTestEnv(t, true)
	lead := e.start("synth lead", "")
	a1 := e.ask(lead, "synth-one", "Synthetic first decision")
	a2 := e.ask(lead, "synth-two", "Synthetic second decision")

	var r struct {
		Ask  c.Ask         `json:"ask"`
		Held c.HeldAnswers `json:"held"`
	}
	e.json("POST", "/api/asks/"+a1.AskID+"/answer", map[string]string{"option_id": "a", "mode": "hold"}, &r)
	if r.Ask.State != c.AskOpen || len(r.Held.Items) != 1 || r.Held.SendsAt == nil {
		t.Fatalf("hold: %+v", r)
	}
	in := e.inbox("open")
	th, _ := findThread(in.Threads, askThreadID(a1))
	if th.Held == nil || th.Held.OptionID != "a" || th.NeedsReply || len(in.Held.Items) != 1 {
		t.Fatalf("held thread: %+v held=%+v", th, in.Held)
	}
	var u struct {
		Held c.HeldAnswers `json:"held"`
	}
	e.json("POST", "/api/held/"+a1.AskID+"/undo", map[string]any{}, &u)
	if len(u.Held.Items) != 0 {
		t.Fatalf("undo: %+v", u)
	}
	e.json("POST", "/api/asks/"+a1.AskID+"/answer", map[string]string{"option_id": "b", "mode": "hold"}, &r)
	e.json("POST", "/api/asks/"+a2.AskID+"/answer", map[string]string{"text": "Synthetic free text", "mode": "hold"}, &r)
	var s struct {
		Results []c.AnswerAskResult `json:"results"`
		Held    c.HeldAnswers       `json:"held"`
	}
	e.json("POST", "/api/held/send", map[string]any{}, &s)
	if len(s.Results) != 2 || len(s.Held.Items) != 0 {
		t.Fatalf("send: %+v", s)
	}
	for _, res := range s.Results {
		if res.Error != nil || res.Ask == nil || res.Ask.State != c.AskAnswered {
			t.Fatalf("result: %+v", res)
		}
	}
	waitFor(t, "both decided", func() bool { return len(e.inbox("open").Threads) == 0 })
}

// ---- projects ----

func TestProjectsListAndDetail(t *testing.T) {
	e := newTestEnv(t, true)
	lead := e.start("synth lead", "")
	child := e.start("synth helper", lead)
	e.ask(lead, "synth-proj", "Synthetic project decision")
	if _, err := e.b.SetProjectPage(ownerCtx(), c.SetProjectPageRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, ProjectID: testProject,
		Content: "# Synthetic goal\n\nShip the **synthetic** thing. See [the plan](https://example.com/plan).\n\n- step one\n"}); err != nil {
		t.Fatal(err)
	}

	var list struct {
		Projects []ProjectCard `json:"projects"`
	}
	e.json("GET", "/api/projects", nil, &list)
	if len(list.Projects) != 1 {
		t.Fatalf("projects: %+v", list)
	}
	p := list.Projects[0]
	if p.Name != "Synthetic web project" || p.OpenAsks != 1 || !p.Waiting || len(p.Leads) != 1 || p.Leads[0].ID != lead || !p.Leads[0].IsLead ||
		p.PageUpdatedAt == nil || p.PageSummary != "Synthetic goal Ship the synthetic thing. See the plan. step one" {
		t.Fatalf("card: %+v", p)
	}

	var d struct {
		Project   c.Project     `json:"project"`
		Page      c.ProjectPage `json:"page"`
		Sessions  []SessionCard `json:"sessions"`
		Asks      []c.Ask       `json:"asks"`
		Artifacts []ArtifactRef `json:"artifacts"`
	}
	e.json("GET", "/api/projects/"+testProject, nil, &d)
	if d.Project.ProjectID != testProject || !strings.HasPrefix(d.Page.Content, "# Synthetic goal") || len(d.Sessions) != 2 || len(d.Asks) != 1 || len(d.Artifacts) != 1 {
		t.Fatalf("detail: %+v", d)
	}
	if !d.Sessions[0].IsLead || d.Sessions[0].ID != lead || d.Sessions[1].ID != child || d.Sessions[1].IsLead {
		t.Fatalf("sessions (leads first): %+v", d.Sessions)
	}
	// The artifact link serves the bytes inert.
	w := e.req("GET", d.Artifacts[0].URL, nil)
	if w.Code != 200 || w.Body.String() != artContent || !strings.HasPrefix(w.Header().Get("Content-Security-Policy"), "sandbox") ||
		w.Header().Get("Content-Type") != "text/html" {
		t.Fatalf("artifact: %d %v %s", w.Code, w.Header(), w.Body)
	}
	if w := e.req("GET", "/api/projects/prj_synth-missing", nil); w.Code != 404 {
		t.Fatalf("missing project: %d", w.Code)
	}
}

// Without ProjectPageAPI the page is the project's "page" document.
func TestProjectPageFallsBackToDocument(t *testing.T) {
	e := newTestEnv(t, false)
	pid := testProject
	if _, err := e.b.CreateDocument(ownerCtx(), c.CreateDocumentRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)},
		ProjectID: &pid, Title: "Page", Slug: c.ProjectPageSlug, Content: "Synthetic page body."}); err != nil {
		t.Fatal(err)
	}
	var list struct {
		Projects []ProjectCard `json:"projects"`
	}
	e.json("GET", "/api/projects", nil, &list)
	if len(list.Projects) != 1 || list.Projects[0].PageSummary != "Synthetic page body." || list.Projects[0].Waiting {
		t.Fatalf("card: %+v", list.Projects)
	}
}

// ---- live updates ----

func TestEventsFanOut(t *testing.T) {
	e := newTestEnv(t, true)
	ts := httptest.NewServer(e.h)
	t.Cleanup(ts.Close)
	open := func() (*bufio.Reader, func()) {
		req, _ := http.NewRequest("GET", ts.URL+"/api/events", nil)
		req.Header.Set(HeaderEmail, testEmail)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("events: %d %v", resp.StatusCode, resp.Header)
		}
		return bufio.NewReader(resp.Body), func() { resp.Body.Close() }
	}
	r1, close1 := open()
	defer close1()
	r2, close2 := open()
	defer close2()
	// Wait until both are subscribed.
	waitFor(t, "subscribers", func() bool {
		e.srv.bus.mu.Lock()
		defer e.srv.bus.mu.Unlock()
		return len(e.srv.bus.subs) == 2
	})

	lead := e.start("synth lead", "")
	a := e.ask(lead, "synth-live", "Synthetic live ask")
	if _, err := e.b.HoldAnswer(ownerCtx(), c.HoldAnswerRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)},
		AnswerAskItem: c.AnswerAskItem{AskID: a.AskID, OptionID: "a"}}); err != nil {
		t.Fatal(err)
	}

	for i, r := range []*bufio.Reader{r1, r2} {
		got := readEvents(t, r, func(seen map[string][]string) bool {
			return has(seen["inbox"], "{}") && has(seen["thread"], `{"thread_id":"`+askThreadID(a)+`"}`) &&
				has(seen["project"], `{"project_id":"`+testProject+`"}`) && len(seen["held"]) > 0 && len(seen[":ping"]) > 0
		})
		if !strings.Contains(got["held"][len(got["held"])-1], a.AskID) {
			t.Fatalf("client %d held payload: %v", i, got["held"])
		}
	}
}

func has(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// readEvents reads SSE frames until done says it has seen enough.
func readEvents(t *testing.T, r *bufio.Reader, done func(map[string][]string) bool) map[string][]string {
	t.Helper()
	seen := map[string][]string{}
	type line struct {
		s   string
		err error
	}
	lines := make(chan line)
	go func() {
		for {
			s, err := r.ReadString('\n')
			lines <- line{s, err}
			if err != nil {
				return
			}
		}
	}()
	timeout := time.After(5 * time.Second)
	name := ""
	for !done(seen) {
		select {
		case l := <-lines:
			if l.err != nil {
				t.Fatalf("stream ended: %v (seen %v)", l.err, seen)
			}
			s := strings.TrimRight(l.s, "\n")
			switch {
			case strings.HasPrefix(s, ": ping"):
				seen[":ping"] = append(seen[":ping"], "")
			case strings.HasPrefix(s, "event: "):
				name = strings.TrimPrefix(s, "event: ")
			case strings.HasPrefix(s, "data: "):
				seen[name] = append(seen[name], strings.TrimPrefix(s, "data: "))
			}
		case <-timeout:
			t.Fatalf("timed out; seen %v", seen)
		}
	}
	return seen
}

// ---- presentation ----

func TestDeliveryInPlainEnglish(t *testing.T) {
	now := time.Date(2026, 10, 15, 17, 0, 0, 0, time.UTC)
	stored := now.Add(-10 * time.Minute)
	sid := "ses_synth-rcpt"
	run := &c.Run{RunID: "run_synth", SessionID: sid, NodeID: testNode, State: c.RunWaitingInput}
	sn := &snap{now: now, sessions: map[string]c.SessionView{sid: {Session: c.Session{SessionID: sid, Title: "synth worker"}, Run: run}},
		mail: groupThreads(nil)}
	msg := func(st c.DeliveryStatus) c.MessageView {
		st.Storage, st.StoredAt = c.StorageStored, &stored
		return c.MessageView{Envelope: c.MessageEnvelope{MessageID: "msg_synth", Sender: owner, Recipient: c.Address{Kind: c.AddrSession, ID: sid}, Mode: c.ModeQueue}, Status: st}
	}
	accepted := now.Add(-9 * time.Minute)
	cases := []struct {
		st          c.DeliveryStatus
		state, want string
	}{
		{c.DeliveryStatus{Runtime: c.DeliveryAccepted, Receipts: []c.DeliveryReceipt{{State: c.DeliveryAccepted, Evidence: c.EvidenceClaudePromptHook, ObservedAt: accepted}}},
			"delivered", "Delivered to synth worker at 9:51 AM"},
		{c.DeliveryStatus{Runtime: c.DeliveryAccepted, Receipts: []c.DeliveryReceipt{{State: c.DeliveryAccepted, Evidence: c.EvidencePyreRead, ObservedAt: accepted}}},
			"read", "Read by the agent at 9:51 AM"},
		{c.DeliveryStatus{Runtime: c.DeliveryAccepted, Ack: c.AckReplied}, "replied", "synth worker replied"},
		{c.DeliveryStatus{Runtime: c.DeliveryRejected, RuntimeReason: "synthetic refusal"}, "failed", "Not delivered: synth worker refused it"},
		{c.DeliveryStatus{Runtime: c.DeliveryExpired}, "failed", "Not delivered: it waited too long"},
	}
	for _, tc := range cases {
		d := sn.delivery(msg(tc.st))
		if d.State != tc.state || !strings.HasPrefix(d.Summary, tc.want) {
			t.Errorf("%+v: got %+v, want %s %q", tc.st, d, tc.state, tc.want)
		}
	}
	run.State, run.Activity = c.RunRunning, &c.RunActivity{Text: "running synthetic tests", Since: stored}
	d := sn.delivery(msg(c.DeliveryStatus{Runtime: c.DeliveryQueued}))
	if d.State != "queued" || !strings.Contains(d.Summary, "Sent 9:50 AM · waiting: synth worker is working (running synthetic tests); will be delivered at its next idle") {
		t.Errorf("queued: %+v", d)
	}
	sn.sessions[sid] = c.SessionView{Session: c.Session{SessionID: sid, Title: "synth worker", Archived: true}}
	if d := sn.delivery(msg(c.DeliveryStatus{Runtime: c.DeliveryQueued})); d.State != "failed" || d.Summary != "Not delivered: synth worker is archived." {
		t.Errorf("archived: %+v", d)
	}
}

func TestClockIsPacific(t *testing.T) {
	now := time.Date(2026, 10, 15, 17, 0, 0, 0, time.UTC) // 10:00 AM PDT
	for _, tc := range []struct {
		t    time.Time
		want string
	}{
		{time.Date(2026, 10, 15, 16, 41, 0, 0, time.UTC), "9:41 AM"},
		{time.Date(2026, 10, 14, 23, 10, 0, 0, time.UTC), "Yesterday 4:10 PM"},
		{time.Date(2026, 10, 12, 23, 10, 0, 0, time.UTC), "Oct 12 4:10 PM"},
		{time.Date(2025, 10, 12, 23, 10, 0, 0, time.UTC), "Oct 12 2025 4:10 PM"},
	} {
		if got := clock(tc.t, now); got != tc.want {
			t.Errorf("clock(%v) = %q, want %q", tc.t, got, tc.want)
		}
	}
}

func TestCheckListenLoopbackOnly(t *testing.T) {
	for addr, ok := range map[string]bool{"127.0.0.1:7390": true, "[::1]:7390": true, "localhost:7390": true,
		"0.0.0.0:7390": false, ":7390": false, "192.0.2.1:7390": false, "nonsense": false} {
		if err := CheckListen(addr); (err == nil) != ok {
			t.Errorf("%s: %v", addr, err)
		}
	}
}

func TestInboxFilters(t *testing.T) {
	e := newTestEnv(t, false)
	lead := e.start("synth lead", "")
	child := e.start("synth helper", lead)
	other := e.start("synth other lead", "")
	a := e.ask(lead, "synth-filter", "Synthetic filtered ask")
	mc := e.mail(child, "Synthetic note from the helper.", "")
	mo := e.mail(other, "Synthetic note from another lead.", "")
	ids := func(q string) map[string]bool {
		var r inboxResp
		e.json("GET", "/api/inbox?box=open"+q, nil, &r)
		out := map[string]bool{}
		for _, th := range r.Threads {
			out[th.ThreadID] = true
		}
		return out
	}
	got := ids("&session=" + lead)
	if len(got) != 2 || !got[askThreadID(a)] || !got[mc.Envelope.MessageID] {
		t.Fatalf("lead scope (lead + descendants): %v", got)
	}
	if got := ids("&session=" + other); len(got) != 1 || !got[mo.Envelope.MessageID] {
		t.Fatalf("other lead: %v", got)
	}
	if got := ids("&project=" + testProject); len(got) != 3 {
		t.Fatalf("project: %v", got)
	}
	if got := ids("&project=prj_synth-elsewhere"); len(got) != 0 {
		t.Fatalf("other project: %v", got)
	}
}

// An unanswered message ages out of the open box after messageOpenWindow.
func TestOldMessagesLeaveTheInbox(t *testing.T) {
	e := newTestEnv(t, false)
	lead := e.start("synth lead", "")
	m := e.mail(lead, "Synthetic status report.", "")
	if _, ok := findThread(e.inbox("open").Threads, m.Envelope.MessageID); !ok {
		t.Fatal("a fresh message should be open")
	}
	e.clk.Set(e.clk.Now().Add(messageOpenWindow + time.Minute))
	waitFor(t, "message aged out", func() bool {
		_, open := findThread(e.inbox("open").Threads, m.Envelope.MessageID)
		return !open
	})
	if _, ok := findThread(e.inbox("done").Threads, m.Envelope.MessageID); !ok {
		t.Fatal("an aged-out message should be in done")
	}
}

// Projects list in a stable order (by name, not activity) and say whether
// they are active (a live session or an open ask).
func TestProjectsOrderAndActive(t *testing.T) {
	e := newTestEnv(t, true)
	e.start("synth lead", "") // in "Synthetic web project"
	for _, name := range []string{"zz synthetic later", "Aa synthetic empty"} {
		if _, err := e.b.CreateProject(ownerCtx(), c.CreateProjectRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	var list struct {
		Projects []ProjectCard `json:"projects"`
	}
	e.json("GET", "/api/projects", nil, &list)
	var names []string
	for _, p := range list.Projects {
		names = append(names, p.Name)
		wantActive := p.Name == "Synthetic web project"
		if p.Active != wantActive || (p.LiveSessions > 0) != wantActive {
			t.Fatalf("%s: active %v live %d", p.Name, p.Active, p.LiveSessions)
		}
	}
	if got := strings.Join(names, " | "); got != "Aa synthetic empty | Synthetic web project | zz synthetic later" {
		t.Fatalf("order: %s", got)
	}
}

// ---- archive ----

func TestArchiveMessageThreadLeavesOpen(t *testing.T) {
	e := newTestEnv(t, false)
	lead := e.start("synth lead", "")
	m1 := e.mail(lead, "Synthetic status: build green.", "")
	m2 := e.mail(lead, "Synthetic: one more thing.", m1.Envelope.MessageID)
	if _, ok := findThread(e.inbox("open").Threads, m1.Envelope.MessageID); !ok {
		t.Fatal("a fresh message should be open")
	}
	var r struct {
		Thread Thread `json:"thread"`
	}
	e.json("POST", "/api/threads/"+m1.Envelope.MessageID+"/archive", map[string]string{"operation_id": "synth-archive-1"}, &r)
	if r.Thread.ThreadID != m1.Envelope.MessageID || r.Thread.NeedsReply || r.Thread.Unread {
		t.Fatalf("archived thread: %+v", r.Thread)
	}
	for _, m := range []c.MessageView{m1, m2} {
		got, err := e.b.GetMessage(ownerCtx(), c.GetRequest{ID: m.Envelope.MessageID})
		if err != nil || got.Status.Ack != c.AckAcknowledged {
			t.Fatalf("%s should be acknowledged at the hub: %v %+v", m.Envelope.MessageID, err, got.Status)
		}
	}
	if _, still := findThread(e.inbox("open").Threads, m1.Envelope.MessageID); still {
		t.Fatal("archived thread is still open")
	}
	if _, ok := findThread(e.inbox("done").Threads, m1.Envelope.MessageID); !ok {
		t.Fatal("archived thread should be in done")
	}
	// Retrying the same request is harmless.
	e.json("POST", "/api/threads/"+m1.Envelope.MessageID+"/archive", map[string]string{"operation_id": "synth-archive-1"}, &r)
	// A new message from the session opens the thread again.
	e.clk.Advance(time.Minute)
	e.mail(lead, "Synthetic: something new.", m2.Envelope.MessageID)
	waitFor(t, "reopened by a new message", func() bool {
		_, ok := findThread(e.inbox("open").Threads, m1.Envelope.MessageID)
		return ok
	})
	if w := e.req("POST", "/api/threads/msg_synth-unknown/archive", map[string]any{}); w.Code != 404 {
		t.Fatalf("unknown thread: %d %s", w.Code, w.Body)
	}
}

func TestArchiveAsks(t *testing.T) {
	e := newTestEnv(t, false)
	lead := e.start("synth lead", "")
	a := e.ask(lead, "synth-archive-ask", "Synthetic archive ask")
	w := e.req("POST", "/api/threads/"+askThreadID(a)+"/archive", map[string]any{})
	if w.Code != http.StatusConflict || errCode(t, w) != string(c.ErrConflict) || !strings.Contains(w.Body.String(), "open questions can't be archived") {
		t.Fatalf("open ask: %d %s", w.Code, w.Body)
	}
	if _, ok := findThread(e.inbox("open").Threads, askThreadID(a)); !ok {
		t.Fatal("the open ask should stay open")
	}
	e.json("POST", "/api/asks/"+a.AskID+"/answer", map[string]string{"option_id": "a", "mode": "now"}, nil)
	waitFor(t, "ask decided", func() bool {
		_, ok := findThread(e.inbox("done").Threads, askThreadID(a))
		return ok
	})
	var r struct {
		Thread Thread `json:"thread"`
	}
	e.json("POST", "/api/threads/"+askThreadID(a)+"/archive", map[string]any{}, &r)
	if r.Thread.Ask == nil || r.Thread.Ask.State == c.AskOpen || r.Thread.NeedsReply {
		t.Fatalf("decided ask: %+v", r.Thread)
	}
}

func TestArchiveBulk(t *testing.T) {
	e := newTestEnv(t, false)
	lead := e.start("synth lead", "")
	a := e.ask(lead, "synth-bulk-ask", "Synthetic bulk ask")
	m1 := e.mail(lead, "Synthetic report one.", "")
	m2 := e.mail(lead, "Synthetic report two.", "")
	var r struct {
		Results []ArchiveResult `json:"results"`
	}
	ids := []string{m1.Envelope.MessageID, askThreadID(a), m2.Envelope.MessageID, "msg_synth-unknown", m1.Envelope.MessageID}
	e.json("POST", "/api/inbox/archive", map[string]any{"thread_ids": ids, "operation_id": "synth-bulk"}, &r)
	if len(r.Results) != 4 {
		t.Fatalf("results (duplicates collapse): %+v", r.Results)
	}
	if r.Results[0].Thread == nil || r.Results[0].Error != nil || r.Results[2].Thread == nil {
		t.Fatalf("message results: %+v", r.Results)
	}
	if r.Results[1].Error == nil || r.Results[1].Error.Code != c.ErrConflict || r.Results[1].Thread != nil {
		t.Fatalf("open ask result: %+v", r.Results[1])
	}
	if r.Results[3].Error == nil || r.Results[3].Error.Code != c.ErrNotFound {
		t.Fatalf("unknown result: %+v", r.Results[3])
	}
	open := e.inbox("open").Threads
	if len(open) != 1 || open[0].ThreadID != askThreadID(a) {
		t.Fatalf("only the ask should stay open: %+v", open)
	}
	if w := e.req("POST", "/api/inbox/archive", map[string]any{"thread_ids": []string{}}); w.Code != 400 {
		t.Fatalf("empty bulk: %d", w.Code)
	}
}
