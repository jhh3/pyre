package assemble

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Regression for the owner report of 2026-10-12: 84 open "Session run
// failed" items (53 Codex launches "no Codex account with capacity right
// now"), nearly all sessions started by lead sessions that retried while
// capacity was exhausted; none ever ran, and each flagged its ended session
// "needs you" (Sessions badge 44 with no live session needing the owner).

const capacityMsg = "no eligible openai subscription on this node: acct_synth-1: cooldown until 2026-10-12T20:00:00Z (usage limit); acct_synth-2: usage limit reached"

type failuresEnv struct {
	t     *testing.T
	st    *Stack
	n     *simNode
	owner context.Context
}

func (e *failuresEnv) op() c.Mutation { return c.Mutation{OperationID: c.NewID(c.PrefixOperation)} }

func (e *failuresEnv) start(as context.Context, title string) string {
	e.t.Helper()
	resp, err := e.st.Hub.StartSession(as, c.StartSessionRequest{Mutation: e.op(), NodeID: e.n.nodeID, Harness: c.HarnessMock, Cwd: "/tmp/synth", Title: title})
	if err != nil {
		e.t.Fatal(err)
	}
	e.n.idle()
	return resp.Session.Session.SessionID
}

// failLaunches makes every later launch of a session other than keep fail
// for capacity.
func (e *failuresEnv) failLaunches(keep ...string) {
	e.n.mu.Lock()
	defer e.n.mu.Unlock()
	e.n.failStart = func(sr c.StartRunCommand) *c.Error {
		for _, k := range keep {
			if sr.Session.SessionID == k {
				return nil
			}
		}
		return c.Errorf(c.ErrCapacityExhausted, "%s", capacityMsg)
	}
}

func (e *failuresEnv) inbox() []c.AttentionItem {
	e.t.Helper()
	page, err := e.st.Hub.ListAttention(e.owner, c.ListAttentionRequest{PageRequest: c.PageRequest{Limit: 200}})
	if err != nil {
		e.t.Fatal(err)
	}
	return page.Items
}

// needsYou is the Sessions badge: listed sessions flagged "needs you".
func (e *failuresEnv) needsYou() []string {
	e.t.Helper()
	page, err := e.st.Hub.ListSessions(e.owner, c.ListSessionsRequest{PageRequest: c.PageRequest{Limit: 200}})
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, v := range page.Items {
		if v.Flag != nil && v.Flag.Level == c.FlagNeedsYou {
			out = append(out, v.Session.Title+": "+v.Flag.Reason)
		}
	}
	return out
}

func newFailuresEnv(t *testing.T) *failuresEnv {
	st, n := newStack(t)
	return &failuresEnv{t: t, st: st, n: n, owner: c.WithPrincipal(context.Background(), ownerP)}
}

func TestChildCapacityLaunchFailuresReachParentNotOwner(t *testing.T) {
	e := newFailuresEnv(t)
	lead := e.start(e.owner, "synthetic lead")
	e.failLaunches(lead)
	asLead := c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalSession, ID: lead})
	var children []string
	for i := range 6 {
		children = append(children, e.start(asLead, fmt.Sprintf("synthetic codex reviewer %d", i)))
	}
	for _, id := range children {
		v, err := e.st.Hub.GetSession(e.owner, c.GetRequest{ID: id})
		if err != nil || v.Run == nil || v.Run.State != c.RunFailed || v.Session.Parent() != lead {
			t.Fatalf("child %s: %+v %v", id, v.Run, err)
		}
		if v.Flag != nil && v.Flag.Level == c.FlagNeedsYou {
			t.Fatalf("child %s flagged needs you: %+v", id, v.Flag)
		}
	}
	if items := e.inbox(); len(items) != 0 {
		t.Fatalf("owner inbox has %d items: %+v", len(items), items)
	}
	if ny := e.needsYou(); len(ny) != 0 {
		t.Fatalf("needs-you badge %d: %v", len(ny), ny)
	}

	// The lead hears of every failed launch, in plain words, once each.
	if _, err := e.st.Hub.ProcessChildReports(context.Background()); err != nil {
		t.Fatal(err)
	}
	page, err := e.st.Hub.ListMessages(e.owner, c.ListMessagesRequest{Box: "inbox", Address: &c.Address{Kind: c.AddrSession, ID: lead}, PageRequest: c.PageRequest{Limit: 200}})
	if err != nil {
		t.Fatal(err)
	}
	from := map[string]int{}
	for _, m := range page.Items {
		from[m.Envelope.Sender.ID]++
		if !strings.Contains(m.Envelope.Body.Text, "could not start: no Codex account with capacity right now") {
			t.Fatalf("report text:\n%s", m.Envelope.Body.Text)
		}
	}
	for _, id := range children {
		if from[id] != 1 {
			t.Fatalf("lead got %d reports from %s (all: %v)", from[id], id, from)
		}
	}
	owners, err := e.st.Hub.ListMessages(e.owner, c.ListMessagesRequest{Box: "inbox", Address: &c.Address{Kind: c.AddrOwner, ID: "owner"}})
	if err != nil || len(owners.Items) != 0 {
		t.Fatalf("owner messages: %d %v", len(owners.Items), err)
	}
	// Maintenance changes nothing (nothing stale to clear).
	if err := e.st.Hub.RunMaintenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if items := e.inbox(); len(items) != 0 {
		t.Fatalf("owner inbox after maintenance: %+v", items)
	}
}

// Owner-started launches that fail for capacity are one item per provider
// with a count and the sessions; the item shrinks as sessions are archived
// or run again and goes with the last one. None flags a session.
func TestOwnerCapacityLaunchFailuresGrouped(t *testing.T) {
	e := newFailuresEnv(t)
	e.failLaunches()
	var ids []string
	for i := range 3 {
		ids = append(ids, e.start(e.owner, fmt.Sprintf("synthetic codex job %d", i)))
	}
	items := e.inbox()
	if len(items) != 1 {
		t.Fatalf("want one grouped item, got %+v", items)
	}
	it := items[0]
	if it.Kind != c.AttnFailure || it.GroupKey != "capacity:openai" || it.Count != 3 || it.Title != "Codex out of capacity: 3 launches waiting for the reset" ||
		!strings.Contains(it.Summary, "'synthetic codex job 0'") || it.SessionID != "" {
		t.Fatalf("grouped item: %+v", it)
	}
	if ny := e.needsYou(); len(ny) != 0 {
		t.Fatalf("needs-you badge %d: %v", len(ny), ny)
	}

	archived := true
	if _, err := e.st.Hub.UpdateSession(e.owner, c.UpdateSessionRequest{Mutation: e.op(), SessionID: ids[0], Archived: &archived}); err != nil {
		t.Fatal(err)
	}
	if items := e.inbox(); len(items) != 1 || items[0].Count != 2 {
		t.Fatalf("after archive: %+v", items)
	}
	e.failLaunches(ids[1]) // capacity is back for this one
	if _, err := e.st.Hub.ResumeSession(e.owner, c.ResumeRequest{Mutation: e.op(), SessionID: ids[1]}); err != nil {
		t.Fatal(err)
	}
	e.n.idle()
	if items := e.inbox(); len(items) != 1 || items[0].Count != 1 || items[0].Title != "Codex out of capacity: 1 launch waiting for the reset" {
		t.Fatalf("after a resume went live: %+v", items)
	}
	if _, err := e.st.Hub.UpdateSession(e.owner, c.UpdateSessionRequest{Mutation: e.op(), SessionID: ids[2], Archived: &archived}); err != nil {
		t.Fatal(err)
	}
	if items := e.inbox(); len(items) != 0 {
		t.Fatalf("after the last left: %+v", items)
	}
}

// Items from before these rules (or missed events) are cleared by the
// maintenance sweep, idempotently; a real open problem stays.
func TestMaintenanceSweepsStaleFailureItems(t *testing.T) {
	e := newFailuresEnv(t)
	lead := e.start(e.owner, "synthetic lead") // live (idle)
	e.failLaunches(lead)
	asLead := c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalSession, ID: lead})
	child := e.start(asLead, "synthetic child")
	ended := e.start(e.owner, "synthetic ended") // owner-started, launch failed: its failure stays
	if items := e.inbox(); len(items) != 1 {
		t.Fatalf("setup inbox: %+v", items)
	}
	now := e.n.clock.Now()
	insert := func(kind c.AttentionKind, group, session string, at time.Time) string {
		t.Helper()
		id := c.NewID(c.PrefixAttention)
		ts := sqlitex.FormatTime(at)
		if _, err := e.st.DB.Exec(`INSERT INTO attention_items(attention_id,kind,severity,group_key,object_kind,object_id,session_id,title,state,count,created_at,updated_at,deep_link)
			VALUES(?,?,?,?,?,?,?,?,?,1,?,?,?)`, id, string(kind), "normal", group, string(c.KindSession), session, session, "Session run failed", string(c.AttnOpen), ts, ts, "pyre://session/"+session); err != nil {
			t.Fatal(err)
		}
		return id
	}
	old := now.Add(-time.Hour)
	legacyChild := insert(c.AttnFailure, "failure:"+child, child, old)                     // a child's failure: its parent hears of it
	legacyLive := insert(c.AttnFailure, "failure:"+lead, lead, old)                        // the session runs again
	staleAccount := insert(c.AttnAccountPool, "pool:openai", ended, old)                   // limit hit before the session ended
	freshAccount := insert(c.AttnAccountPool, "pool:anthropic", ended, now.Add(time.Hour)) // raised after it ended: kept
	state := func(id string) c.AttentionState {
		t.Helper()
		it, err := e.st.Hub.GetAttention(e.owner, c.GetRequest{ID: id})
		if err != nil {
			t.Fatal(err)
		}
		return it.State
	}
	changes := func() int {
		var n int
		e.st.DB.QueryRow(`SELECT COUNT(*) FROM events WHERE kind=?`, string(c.EvAttentionChanged)).Scan(&n)
		return n
	}
	first := 0
	for pass := range 2 {
		if err := e.st.Hub.RunMaintenance(context.Background()); err != nil {
			t.Fatal(err)
		}
		for id, want := range map[string]c.AttentionState{legacyChild: c.AttnWithdrawn, legacyLive: c.AttnWithdrawn, staleAccount: c.AttnWithdrawn, freshAccount: c.AttnOpen} {
			if got := state(id); got != want {
				t.Fatalf("pass %d: %s is %s, want %s (child %s, live %s, stale %s, fresh %s)", pass, id, got, want, legacyChild, legacyLive, staleAccount, freshAccount)
			}
		}
		if pass == 0 {
			first = changes()
		} else if n := changes(); n != first {
			t.Fatalf("second sweep changed items: %d -> %d attention.changed events", first, n)
		}
	}
	// The owner-started launch failure itself stays, without "needs you".
	var stays bool
	for _, it := range e.inbox() {
		stays = stays || it.GroupKey == "capacity:openai"
	}
	if !stays {
		t.Fatalf("open launch failure swept: %+v", e.inbox())
	}
	if ny := e.needsYou(); len(ny) != 0 {
		t.Fatalf("needs-you: %v", ny)
	}
}
