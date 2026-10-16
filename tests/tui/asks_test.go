package tui_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// Open asks (owner request 2026-10-15): the inbox moved to the web app;
// the TUI has no Inbox view or panel, only the signal that asks are open —
// "● N asks" in the header, the lead chips and the project headers — each
// an OSC 8 link to the web inbox filtered to match, and I / :inbox opens
// the link for the selection (or copies it when there is no display).

const (
	coordinator = "ses_synth-a"       // "coordinator": top level, no project
	outfitSess  = "ses_synth-project" // "outfit API client" in Synthetic Outfit
	testWebURL  = "https://pyre-web.example.test"
)

// noAsksAPI hides the mock's asks ops: a hub without them.
type noAsksAPI struct{ contracts.API }

// asksNow is mid-morning Pacific (10:00 PDT), outside quiet hours, so
// default deadlines stay one hour away.
var asksNow = time.Date(2026, 10, 4, 17, 0, 0, 0, time.UTC)

// opened records the links the TUI opens; fail makes the opener fail.
type opened struct {
	mu   sync.Mutex
	urls []string
	clip []string
	fail error
}

func (o *opened) open(u string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fail != nil {
		return o.fail
	}
	o.urls = append(o.urls, u)
	return nil
}

func (o *opened) last() (opened, copied string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if n := len(o.urls); n > 0 {
		opened = o.urls[n-1]
	}
	if n := len(o.clip); n > 0 {
		copied = o.clip[n-1]
	}
	return opened, copied
}

// startAsks runs the TUI over the fixture mock (with the event stream) at
// asksNow with the web app at testWebURL; seed runs before the TUI starts.
func startAsks(t *testing.T, w, ht int, wrap func(*mock.Backend) contracts.API, seed func(api contracts.API)) (*tuitest.Harness, *mock.Backend, *mock.FakeClock, *opened) {
	t.Helper()
	b, clk := tuitest.NewMock(t)
	clk.Set(asksNow)
	var api contracts.API = b
	if wrap != nil {
		api = wrap(b)
	}
	if seed != nil {
		seed(b)
	}
	rec := &opened{}
	opts := tuitest.Options()
	opts.Clock = clk
	opts.Watch = tui.PollWatcher(b)
	opts.WebURL = testWebURL + "/"
	opts.OpenURL = rec.open
	opts.Clipboard = func(s string) { rec.mu.Lock(); rec.clip = append(rec.clip, s); rec.mu.Unlock() }
	h := tuitest.New(t, api, opts, w, ht)
	h.WaitFor("initial data", 5*sec, func() bool {
		return h.M.Loaded("sessions") && h.M.Loaded("nodes") && h.M.Loaded("attention")
	})
	h.Settle()
	return h, b, clk, rec
}

func sessCtx(sid string) context.Context {
	return contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalSession, ID: sid})
}

// ask has a session ask the owner (synthetic text).
func ask(t *testing.T, api contracts.API, sid, topic, title, ctx string, def string, urgent bool, deadline time.Duration, opts ...string) contracts.Ask {
	t.Helper()
	var options []contracts.AskOption
	for i, o := range opts {
		options = append(options, contracts.AskOption{ID: string(rune('a' + i)), Label: o, Detail: "Synthetic consequence of " + o + "."})
	}
	// The hub requires AskMinContext characters; pad synthetic context.
	for len(ctx) < contracts.AskMinContext {
		ctx += " Synthetic background for the owner: nothing else is known yet."
	}
	resp, err := api.(contracts.AsksAPI).CreateAsk(sessCtx(sid), contracts.CreateAskRequest{
		Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
		TopicKey: topic, Title: title, Context: ctx, Options: options, DefaultOption: def, Urgent: urgent,
		DeadlineSeconds: int(deadline / time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Ask == nil {
		t.Fatalf("ask %s: status %s (%s)", topic, resp.Status, resp.Detail)
	}
	return *resp.Ask
}

// seedAsks: two asks from the coordinator (one urgent), one from the
// Outfit session.
func seedAsks(t *testing.T) func(api contracts.API) {
	return func(api contracts.API) {
		ask(t, api, coordinator, "rollout-plan", "Pick the rollout plan for the synthetic service",
			"Staging is green. Canary first is slower but safer; big bang finishes today.",
			"a", false, 0, "Canary first", "Big bang")
		ask(t, api, coordinator, "db-migration", "Run the synthetic schema migration now?",
			"The migration locks one table for about a minute.",
			"b", true, 10*time.Minute, "Run now", "Wait for the batch job", "Skip it")
		ask(t, api, outfitSess, "client-retries", "Retry policy for the synthetic API client",
			"Three retries with backoff, or fail fast and surface the error to the caller.",
			"", false, 45*time.Minute, "Three retries", "Fail fast")
	}
}

func header(h *tuitest.Harness) string { return strings.Split(h.View(), "\n")[0] }

// osc8 is the opening of an OSC 8 hyperlink to u.
func osc8(u string) string { return "\x1b]8;;" + u + "\x07" }

func TestNoInboxViewOrPanel(t *testing.T) {
	h, _, _, _ := startAsks(t, 200, 50, nil, seedAsks(t))
	h.WaitFor("asks", 5*sec, func() bool { return h.M.Loaded("asks") })
	if strings.Contains(header(h), "Inbox") {
		t.Fatalf("no Inbox tab: %s", header(h))
	}
	selectSession(t, h, coordinator)
	h.Settle()
	if h.Contains("INBOX ·") {
		t.Fatalf("no inbox panel beside the terminal:\n%s", h.View())
	}
	h.Key("I")
	if v := h.M.ActiveView(); v != "Sessions" {
		t.Fatalf("I opens the web inbox, not a view: %s", v)
	}
	for _, k := range []string{"g", "i", "ctrl+w"} {
		h.Key(k)
	}
	if h.Contains("INBOX ·") || h.Contains("inbox panel") {
		t.Fatalf("g i / ctrl+w no longer show a panel:\n%s", h.View())
	}
}

func TestAsksHeaderCount(t *testing.T) {
	h, b, clk, _ := startAsks(t, 160, 34, nil, func(api contracts.API) {
		ask(t, api, outfitSess, "client-retries", "Retry policy for the synthetic API client",
			"Synthetic: retries or fail fast.", "", false, 45*time.Minute, "Three retries", "Fail fast")
	})
	h.WaitFor("count", 5*sec, func() bool { return strings.Contains(header(h), "● 1 ask") })
	if n, hot := h.M.OpenAsks(); n != 1 || hot {
		t.Fatalf("one ask due in 45m: n %d hot %v", n, hot)
	}
	// Within 15 minutes of its deadline the count reads red.
	clk.Advance(31 * time.Minute)
	if n, hot := h.M.OpenAsks(); n != 1 || !hot {
		t.Fatalf("due in 14m: n %d hot %v", n, hot)
	}
	// An urgent ask reads red at once; the count follows the event.
	ask(t, b, coordinator, "db-migration", "Run the synthetic schema migration now?",
		"Synthetic: the migration locks one table.", "b", true, 2*time.Hour, "Run now", "Wait")
	h.WaitFor("two asks", 5*sec, func() bool { return strings.Contains(header(h), "● 2 asks") })
	// The count links to the web inbox.
	raw := strings.Split(h.M.View().Content, "\n")[0]
	if !strings.Contains(raw, osc8(testWebURL+"/#/inbox")) {
		t.Fatalf("the header's count is a link to the web inbox: %q", raw)
	}
}

func TestAsksHeaderHiddenWithoutAsks(t *testing.T) {
	h, _, _, _ := startAsks(t, 160, 34, func(b *mock.Backend) contracts.API { return noAsksAPI{b} }, nil)
	if strings.Contains(header(h), "ask") {
		t.Fatalf("no ask count without asks: %s", header(h))
	}
	if h.M.Loaded("asks") {
		t.Fatal("no asks call against a hub without them")
	}
}

func TestAskLinks(t *testing.T) {
	h, _, _ := startLeads(t, 160, 30)
	h.Key("down")
	h.Settle()
	raw := h.M.View().Content
	for _, want := range []string{
		osc8("https://pyre-example.ngrok.app/#/inbox"),                          // the header (default web URL)
		osc8("https://pyre-example.ngrok.app/#/inbox?session=" + coordinator),   // the coordinator's chip
		osc8("https://pyre-example.ngrok.app/#/inbox?project=" + outfitProject), // the project header
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("no link %q in the frame:\n%q", want, raw)
		}
	}
	// The links take no cells: the frame reads as before.
	if l := rowLine(h.View(), "coordinator"); !strings.Contains(l, "● asks 2") {
		t.Fatalf("coordinator chip: %q", l)
	}
}

func TestOpenWebInboxForSelection(t *testing.T) {
	h, _, _, rec := startAsks(t, 200, 50, nil, seedAsks(t))
	h.WaitFor("asks", 5*sec, func() bool { return h.M.Loaded("asks") })
	// A lead with asks: its asks.
	selectSession(t, h, coordinator)
	h.Key("I")
	if u, _ := rec.last(); u != testWebURL+"/#/inbox?session="+coordinator {
		t.Fatalf("I on the coordinator opened %q", u)
	}
	if !h.Contains("opened asks from coordinator in the browser") {
		t.Fatalf("I says what it opened:\n%s", h.View())
	}
	// A session whose lead has no asks: the whole inbox.
	selectSession(t, h, "ses_synth-offline")
	h.Key("I")
	if u, _ := rec.last(); u != testWebURL+"/#/inbox" {
		t.Fatalf("I without asks opened %q", u)
	}
	// :inbox on a project with asks (the Projects list).
	h.Key("P")
	h.WaitFor("projects", 5*sec, func() bool { return h.Contains("Synthetic Outfit") })
	for i := 0; i < 5 && h.M.SelectedProject() != "prj_synth-outfit"; i++ {
		h.Key("j")
	}
	h.Key(":")
	h.Type("inbox")
	h.Key("enter")
	if u, _ := rec.last(); u != testWebURL+"/#/inbox?project=prj_synth-outfit" {
		t.Fatalf(":inbox on Synthetic Outfit opened %q", u)
	}
	// No display (ssh): the link is copied instead, and the toast says so.
	rec.mu.Lock()
	rec.fail = errors.New("no display")
	rec.mu.Unlock()
	h.Key("I")
	if _, c := rec.last(); c != testWebURL+"/#/inbox?project=prj_synth-outfit" {
		t.Fatalf("copied %q", c)
	}
	if !h.Contains("copied the Synthetic Outfit asks link") {
		t.Fatalf("the toast says the link was copied:\n%s", h.View())
	}
}

func TestClickHeaderCountOpensWebInbox(t *testing.T) {
	h, _, _, rec := startAsks(t, 160, 34, nil, seedAsks(t))
	h.WaitFor("count", 5*sec, func() bool { return strings.Contains(header(h), "● 3 asks") })
	click(t, h, "● 3 asks")
	if u, _ := rec.last(); u != testWebURL+"/#/inbox" {
		t.Fatalf("a click on the count opened %q", u)
	}
}

// TestAskLinksReachTheTerminal runs the real Bubble Tea program (no
// terminal; an ANSI profile, as a terminal would detect — the NoTTY
// profile drops links): the renderer writes the header's OSC 8 link.
func TestAskLinksReachTheTerminal(t *testing.T) {
	b, clk := tuitest.NewMock(t)
	clk.Set(asksNow)
	seedAsks(t)(b)
	opts := tuitest.Options()
	opts.Clock = clk
	opts.WebURL = testWebURL
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m := tui.New(ctx, b, opts)
	defer m.Close()
	inR, inW := io.Pipe()
	defer inW.Close()
	out := &lockedBuf{}
	p := tea.NewProgram(m, tea.WithInput(inR), tea.WithOutput(out), tea.WithContext(ctx), tea.WithoutSignalHandler(), tea.WithWindowSize(160, 30),
		tea.WithColorProfile(colorprofile.ANSI))
	done := make(chan struct{})
	go func() { _, _ = p.Run(); close(done) }()
	p.Send(tea.WindowSizeMsg{Width: 160, Height: 30})
	waitUntil(t, "the header's ask link", 10*time.Second, func() bool {
		return out.contains(osc8(testWebURL+"/#/inbox")) && out.contains("3 asks")
	})
	p.Quit()
	<-done
}
