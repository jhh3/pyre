package attention

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/attention/sessionflag"
	"github.com/jhh3/pyre/internal/contracts"
)

// Stale launch failures (owner report 2026-10-12): 84 open "Session run
// failed" items, 53 of them Codex launches by lead sessions waiting for a
// capacity reset, each flagging an ended session "needs you".

// lineageLookup resolves session titles and parents.
type lineageLookup struct {
	fakeLookup
	titles  map[string]string
	parents map[string]string
}

func (l lineageLookup) SessionTitle(_ context.Context, id string) (string, error) {
	return l.titles[id], nil
}

func (l lineageLookup) SessionParent(_ context.Context, id string) (string, error) {
	return l.parents[id], nil
}

func newLineageEnv(t *testing.T, lk lineageLookup) *env {
	e := newEnv(t)
	e.der = NewDeriver(e.store, e.log, DeriverOptions{Lookup: lk})
	return e
}

const capacityReason = "launch failed: no eligible openai subscription on this node: acct_synth-1: cooldown until 2026-10-12T20:00:00Z (usage limit)"

func (e *env) runFailed(sid, runID string, h contracts.Harness, reason string) {
	r := contracts.Run{RunID: runID, SessionID: sid, Harness: h, State: contracts.RunFailed, StateReason: reason}
	e.log.AppendPayload(contracts.EvRunEnded, contracts.ObjectRef{Kind: contracts.KindRun, ID: runID}, sid, nil, r)
}

func (e *env) runState(sid, runID string, st contracts.RunState) {
	r := contracts.Run{RunID: runID, SessionID: sid, State: st}
	kind := contracts.EvRunObserved
	if st.Terminal() {
		kind = contracts.EvRunEnded
	}
	e.log.AppendPayload(kind, contracts.ObjectRef{Kind: contracts.KindRun, ID: runID}, sid, nil, r)
}

func (e *env) archived(sid string) {
	s := contracts.Session{SessionID: sid, Archived: true}
	e.log.AppendPayload(contracts.EvSessionUpdated, contracts.ObjectRef{Kind: contracts.KindSession, ID: sid}, sid, nil, s)
}

func (e *env) active() []contracts.AttentionItem {
	return e.list(contracts.AttnOpen, contracts.AttnRead)
}

// A failed run of a session that sits under another session is reported to
// that parent by the hub, never raised for the owner; the same failure of
// an owner-started session still is.
func TestChildFailureIsNotOwnerAttention(t *testing.T) {
	e := newLineageEnv(t, lineageLookup{parents: map[string]string{"ses_synth-child": "ses_synth-lead"}})
	e.runFailed("ses_synth-child", "run_synth-c1", contracts.HarnessCodex, capacityReason)
	e.runFailed("ses_synth-child", "run_synth-c2", contracts.HarnessCodex, "launch failed: checkout missing")
	e.drain()
	if got := e.active(); len(got) != 0 {
		t.Fatalf("child failures raised owner items: %+v", got)
	}
	e.runFailed("ses_synth-own", "run_synth-o1", contracts.HarnessCodex, "launch failed: checkout missing")
	e.drain()
	if got := e.byGroup("failure:ses_synth-own"); len(got) != 1 || got[0].State != contracts.AttnOpen {
		t.Fatalf("owner-started failure: %+v", got)
	}
}

// Owner-started launches that failed for capacity collapse into one item
// per provider, with the count and the session list; a retry of the same
// session is not a second launch waiting.
func TestCapacityFailuresGroupPerProvider(t *testing.T) {
	e := newLineageEnv(t, lineageLookup{titles: map[string]string{"ses_synth-1": "review api", "ses_synth-2": "judge ui", "ses_synth-3": "claude docs"}})
	e.runFailed("ses_synth-1", "run_synth-1a", contracts.HarnessCodex, capacityReason)
	e.clock.Advance(time.Second)
	e.runFailed("ses_synth-2", "run_synth-2a", contracts.HarnessCodex, capacityReason)
	e.clock.Advance(time.Second)
	e.runFailed("ses_synth-1", "run_synth-1b", contracts.HarnessCodex, capacityReason) // retry, still no capacity
	e.runFailed("ses_synth-3", "run_synth-3a", contracts.HarnessClaude, "launch failed: no eligible anthropic subscription on this node: acct_synth-9: limit reached")
	e.drain()
	got := e.active()
	if len(got) != 2 {
		t.Fatalf("want one item per provider, got %d: %+v", len(got), got)
	}
	codex := e.byGroup("capacity:openai")
	if len(codex) != 1 {
		t.Fatalf("codex items: %+v", codex)
	}
	it := codex[0]
	if it.Title != "Codex out of capacity: 2 launches waiting for the reset" || it.Count != 2 || it.SessionID != "" ||
		it.Kind != contracts.AttnFailure || !strings.Contains(it.Summary, "'review api'") || !strings.Contains(it.Summary, "'judge ui'") {
		t.Fatalf("codex item: %+v", it)
	}
	if cl := e.byGroup("capacity:anthropic"); len(cl) != 1 || cl[0].Title != "Claude out of capacity: 1 launch waiting for the reset" {
		t.Fatalf("claude item: %+v", cl)
	}
	// Not one session's flag: the grouped item has no session.
	if n, f := e.flag("ses_synth-1"); n != 0 || f != nil {
		t.Fatalf("grouped item flags a session: %d %+v", n, f)
	}

	// One session runs again, another is archived: the item shrinks, then
	// goes when the last launch is resolved.
	e.runState("ses_synth-1", "run_synth-1c", contracts.RunStarting)
	e.drain()
	if it := e.byGroup("capacity:openai")[0]; it.Count != 2 {
		t.Fatalf("a starting run resolved the launch: %+v", it)
	}
	e.runState("ses_synth-1", "run_synth-1c", contracts.RunRunning)
	e.drain()
	it = e.byGroup("capacity:openai")[0]
	if it.State != contracts.AttnOpen || it.Count != 1 || it.Title != "Codex out of capacity: 1 launch waiting for the reset" || strings.Contains(it.Summary, "review api") {
		t.Fatalf("after one went live: %+v", it)
	}
	e.archived("ses_synth-2")
	e.drain()
	if it := e.byGroup("capacity:openai")[0]; it.State != contracts.AttnWithdrawn {
		t.Fatalf("after the last left: %+v", it)
	}
	// A new capacity failure starts a new item, not the old members.
	e.runFailed("ses_synth-2", "run_synth-2b", contracts.HarnessCodex, capacityReason)
	e.drain()
	var open []contracts.AttentionItem
	for _, it := range e.byGroup("capacity:openai") {
		if it.State == contracts.AttnOpen {
			open = append(open, it)
		}
	}
	if len(open) != 1 || open[0].Count != 1 {
		t.Fatalf("new group item: %+v", open)
	}
}

// A failure item resolves when the session is archived or a later run goes
// live; a retry that only starts (and may fail again) keeps it.
func TestFailureResolvesOnArchiveAndLiveRun(t *testing.T) {
	e := newLineageEnv(t, lineageLookup{})
	e.runFailed("ses_synth-a", "run_synth-a1", contracts.HarnessClaude, "launch failed: checkout missing")
	e.runFailed("ses_synth-b", "run_synth-b1", contracts.HarnessClaude, "launch failed: checkout missing")
	e.drain()
	e.runState("ses_synth-a", "run_synth-a2", contracts.RunStarting)
	e.drain()
	if it := e.byGroup("failure:ses_synth-a"); it[0].State != contracts.AttnOpen {
		t.Fatalf("starting run resolved: %+v", it)
	}
	e.runState("ses_synth-a", "run_synth-a2", contracts.RunWaitingInput)
	e.archived("ses_synth-b")
	e.drain()
	for _, sid := range []string{"ses_synth-a", "ses_synth-b"} {
		if it := e.byGroup("failure:" + sid); len(it) != 1 || it[0].State != contracts.AttnWithdrawn {
			t.Fatalf("%s: %+v", sid, it)
		}
	}
}

// Account limits a session hit go when its run ends; a "waiting for the
// reset" item raised after it ended stays.
func TestAccountItemResolvesWhenSessionEnds(t *testing.T) {
	e := newLineageEnv(t, lineageLookup{})
	plan := contracts.FallbackPlan{SessionID: "ses_synth-a", FromRunID: "run_synth-a", Outcome: contracts.FallbackWaiting, Provider: contracts.ProviderAnthropic}
	e.log.AppendPayload(contracts.EvFallbackPlanned, contracts.ObjectRef{Kind: contracts.KindSession, ID: "ses_synth-a"}, "ses_synth-a", nil, plan)
	e.drain()
	if it := e.byGroup("pool:anthropic"); len(it) != 1 || it[0].State != contracts.AttnOpen {
		t.Fatalf("raised: %+v", it)
	}
	e.runState("ses_synth-a", "run_synth-a", contracts.RunExited)
	e.drain()
	if it := e.byGroup("pool:anthropic"); it[0].State != contracts.AttnWithdrawn {
		t.Fatalf("after the run ended: %+v", it)
	}
	e.log.AppendPayload(contracts.EvFallbackPlanned, contracts.ObjectRef{Kind: contracts.KindSession, ID: "ses_synth-a"}, "ses_synth-a", nil, plan)
	e.drain()
	if got := e.active(); len(got) != 1 || got[0].Kind != contracts.AttnAccountPool {
		t.Fatalf("raised after the end: %+v", got)
	}
}

// The sweep's store half: ResolveSession withdraws and leaves once, keeps
// items newer than the state it decided on, and is idempotent.
func TestResolveSessionIdempotent(t *testing.T) {
	e := newLineageEnv(t, lineageLookup{})
	e.runFailed("ses_synth-a", "run_synth-a1", contracts.HarnessCodex, capacityReason)
	e.runFailed("ses_synth-b", "run_synth-b1", contracts.HarnessCodex, capacityReason)
	e.runFailed("ses_synth-c", "run_synth-c1", contracts.HarnessClaude, "launch failed: checkout missing")
	e.drain()
	ctx := context.Background()
	refs, err := e.store.ResolvableSessions(ctx, "", 100)
	if err != nil || len(refs) != 3 || !refs[0].Member || refs[0].Failure || !refs[2].Failure {
		t.Fatalf("resolvable: %+v %v", refs, err)
	}
	if page, _ := e.store.ResolvableSessions(ctx, refs[0].SessionID, 1); len(page) != 1 || page[0].SessionID != refs[1].SessionID {
		t.Fatalf("paging: %+v", page)
	}
	stale := e.clock.Now().Add(-time.Minute)
	if ch, err := e.store.ResolveSession(ctx, "ses_synth-c", ResolvableKinds, true, stale, "x"); err != nil || ch {
		t.Fatalf("resolved an item newer than the decision: %v %v", ch, err)
	}
	now := e.clock.Now()
	for i := range 2 {
		ch, err := e.store.ResolveSession(ctx, "ses_synth-c", ResolvableKinds, true, now, "The session was archived.")
		if err != nil || ch != (i == 0) {
			t.Fatalf("pass %d: changed=%v err=%v", i, ch, err)
		}
		ch, err = e.store.ResolveSession(ctx, "ses_synth-a", ResolvableKinds, true, now, "The session was archived.")
		if err != nil || ch != (i == 0) {
			t.Fatalf("pass %d leave: changed=%v err=%v", i, ch, err)
		}
	}
	if it := e.byGroup("capacity:openai"); len(it) != 1 || it[0].Count != 1 || it[0].State != contracts.AttnOpen {
		t.Fatalf("group after one left: %+v", it)
	}
	if refs, _ := e.store.ResolvableSessions(ctx, "", 100); len(refs) != 1 || refs[0].SessionID != "ses_synth-b" {
		t.Fatalf("resolvable after: %+v", refs)
	}
	e.store.ResolveSession(ctx, "ses_synth-b", nil, true, now, "")
	if n, err := e.store.PruneMembers(ctx, 10); err != nil || n != 0 {
		t.Fatalf("prune: %d %v", n, err) // leaving deletes the rows itself
	}
}

// An ended session flagged only by its failed launch is not "needs you";
// a failure of a run that did run still is.
func TestLaunchFailureFlagIsInformational(t *testing.T) {
	e := newLineageEnv(t, lineageLookup{})
	e.runFailed("ses_synth-a", "run_synth-a1", contracts.HarnessClaude, "launch failed: checkout missing")
	e.drain()
	_, f, err := e.store.SessionAttentionRun(context.Background(), "ses_synth-a",
		&contracts.Run{RunID: "run_synth-a1", State: contracts.RunFailed, StateReason: "launch failed: checkout missing"})
	if err != nil || f == nil || f.Level != contracts.FlagMessage || f.Reason != "failed" {
		t.Fatalf("launch failure flag: %+v %v", f, err)
	}
	ran := &contracts.Run{RunID: "run_synth-a1", State: contracts.RunFailed, StateReason: "claude exited with an API error"}
	if _, f, _ := e.store.SessionAttentionRun(context.Background(), "ses_synth-a", ran); f == nil || f.Level != contracts.FlagNeedsYou {
		t.Fatalf("crash flag: %+v", f)
	}
	// A blocking item still wins on a launch-failed session.
	items := []contracts.AttentionItem{
		{Kind: contracts.AttnFailure, State: contracts.AttnOpen, Title: "Session run failed"},
		{Kind: contracts.AttnConflict, State: contracts.AttnOpen, Title: "Document edit conflict"},
	}
	if f := sessionflag.ComputeRun(items, &contracts.Run{State: contracts.RunFailed, StateReason: "launch failed: x"}); f.Level != contracts.FlagNeedsYou || f.Reason != "conflict" {
		t.Fatalf("conflict on a launch-failed session: %+v", f)
	}
}

// A message item's summary is the newest message's sanitized excerpt.
func TestMessageItemSummary(t *testing.T) {
	e := newEnv(t)
	send := func(id, body string) {
		v := contracts.MessageView{Envelope: contracts.MessageEnvelope{MessageID: id, Sender: contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_synth-a"},
			Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}, ThreadID: "thr_synth-1", Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: body}}}
		e.log.AppendPayload(contracts.EvMessageStored, contracts.ObjectRef{Kind: contracts.KindMessage, ID: id}, "ses_synth-a", nil, v)
		e.drain()
	}
	send("msg_synth-1", "\x1b[31mBuild\x1b[0m   failed:\n\n  see the log\x07")
	it := e.byGroup("message:thr_synth-1")[0]
	if it.Summary != "Build failed: see the log" {
		t.Fatalf("summary: %q", it.Summary)
	}
	send("msg_synth-2", strings.Repeat("long ", 100))
	it = e.byGroup("message:thr_synth-1")[0]
	if it.Count != 2 || !strings.HasPrefix(it.Summary, "long long") || len([]rune(it.Summary)) != maxExcerpt {
		t.Fatalf("repeat summary: %d %q", it.Count, it.Summary)
	}
	if e.jobs() < 1 {
		t.Fatal("no notify job")
	}

	// Backfill: an item from before summaries is filled once.
	if _, err := e.db.Exec(`UPDATE attention_items SET summary='' WHERE attention_id=?`, it.AttentionID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	todo, err := e.store.MessageItemsWithoutSummary(ctx, "", 10)
	if err != nil || len(todo) != 1 {
		t.Fatalf("todo: %+v %v", todo, err)
	}
	for i := range 2 {
		ch, err := e.store.FillMessageSummary(ctx, it.AttentionID, "synthetic backfilled body")
		if err != nil || ch != (i == 0) {
			t.Fatalf("fill %d: %v %v", i, ch, err)
		}
	}
	if it := e.byGroup("message:thr_synth-1")[0]; it.Summary != "synthetic backfilled body" || it.State != contracts.AttnOpen {
		t.Fatalf("backfilled: %+v", it)
	}
	if todo, _ := e.store.MessageItemsWithoutSummary(ctx, "", 10); len(todo) != 0 {
		t.Fatalf("still todo: %+v", todo)
	}
}
