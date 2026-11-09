package agy

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Fixtures from iss_… (agy 1.4.7 on server,
// 2026-10-25 03:15Z), redacted: conversation ids and paths are synthetic,
// the prompt and tool lines are replaced, agy's own chrome and the quota
// text are kept. Two agy children told their parents they had "finished
// [their] turn" while they were mid-task: every turn had failed on the
// Google login's quota (Stop terminationReason ERROR, RESOURCE_EXHAUSTED),
// and in one of them the main conversation's first failed Stop came while
// its two subagents (separate conversations, same hook file) still ran.
//
//	testdata/events-subagents-quota.jsonl  the run with subagents, whole
//	testdata/events-quota.jsonl            the single-conversation run, tail
//	testdata/events-background-wake.jsonl  Stop fully_idle=false, then the
//	                                       background command wakes it
//	testdata/screen-quota*.txt             the screens after the quota stops

const (
	convMain = "00000000-0000-4000-8000-000000000001"
)

func loadEvents(t *testing.T, name string) []hookRecord {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []hookRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r hookRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func loadScreen(t *testing.T, name string) screenInfo {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return parseScreen(string(b))
}

func TestQuotaScreensAreIdlePrompt(t *testing.T) {
	for _, n := range []string{"screen-quota.txt", "screen-quota-subagents.txt"} {
		si := loadScreen(t, n)
		if !si.HasInputBox || !si.InputEmpty() || si.Working() || !si.Idle() || si.Dialog != "" {
			t.Errorf("%s: box=%v empty=%v working=%v idle=%v dialog=%q footer=%q", n, si.HasInputBox, si.InputEmpty(), si.Working(), si.Idle(), si.Dialog, si.Footer)
		}
	}
}

// The incident run with subagents, replayed hook by hook (screen
// unavailable, so only the hooks decide): the run is never waiting_input
// before the main conversation's last Stop, its subagents never become the
// run's conversation, and the turn end it finally reports is a failed turn
// on a usage limit, not a finished one.
func TestSubagentsQuotaReplay(t *testing.T) {
	recs := loadEvents(t, "events-subagents-quota.jsonl")
	a := &Adapter{}
	rs := newRunState()
	last := len(recs) - 1
	sawBackground := false
	for i, r := range recs {
		rs.applyHook(r)
		if rs.conversationID != convMain || !strings.Contains(rs.transcriptPath, convMain) {
			t.Fatalf("record %d (%s %s): run conversation became %q (%s)", i, r.Event, r.ConversationID, rs.conversationID, rs.transcriptPath)
		}
		checks := []time.Time{r.At.Add(10 * time.Millisecond)}
		if i < last {
			checks = append(checks, recs[i+1].At.Add(-time.Millisecond)) // the quiet gap before the next hook
		}
		for _, now := range checks {
			st, why := a.derive(rs, nil, now)
			if i < last && st == contracts.RunWaitingInput {
				t.Fatalf("record %d (%s %s at %s): waiting_input (%s) before the run's last Stop", i, r.Event, r.ConversationID[len(r.ConversationID)-2:], now.Format("15:04:05.000"), why)
			}
			if st == contracts.RunWaitingBackground {
				sawBackground = true
			}
		}
	}
	if !sawBackground {
		t.Fatal("the main Stop with fully_idle=false while subagents ran never read waiting_on_background")
	}
	// While the main conversation is stopped and subagents run, agy's own
	// prompt is idle on screen: still not a turn end.
	rs2 := newRunState()
	idle := loadScreen(t, "screen-quota-subagents.txt")
	for _, r := range recs {
		rs2.applyHook(r)
		if r.Event == "Stop" && r.ConversationID == convMain {
			break
		}
	}
	if st, why := a.derive(rs2, &idle, rs2.lastStop.At.Add(30*time.Second)); st != contracts.RunWaitingBackground || !strings.Contains(why, "2 subagent(s)") {
		t.Fatalf("main stopped, subagents open, idle screen: %s (%s)", st, why)
	}

	end := recs[last]
	st, why := rs.hookState()
	if st != contracts.RunWaitingInput || !strings.HasPrefix(why, "turn failed") || !strings.Contains(why, "usage_limit") {
		t.Fatalf("after the last Stop: %s (%s)", st, why)
	}
	if rs.limit == nil || rs.limit.Kind != "usage_limit" || rs.limit.ResetAt == nil || !strings.HasPrefix(rs.limit.Detail, "Individual quota reached") {
		t.Fatalf("limit %+v", rs.limit)
	}
	if want := end.At.Add(13*time.Hour + 19*time.Minute + 24*time.Second).UTC(); !rs.limit.ResetAt.Equal(want) {
		t.Fatalf("reset %s, want %s", rs.limit.ResetAt, want)
	}
	// Debounce: right after the Stop the run is still settling; once the
	// idle prompt has held for idleSettle it is waiting_input.
	if st, _ := a.derive(rs, &idle, end.At.Add(500*time.Millisecond)); st != contracts.RunRunning {
		t.Fatalf("0.5 s after the Stop: %s", st)
	}
	if st, _ := a.derive(rs, &idle, end.At.Add(2*time.Second)); st != contracts.RunRunning {
		t.Fatalf("2 s after the Stop: %s", st)
	}
	if st, why := a.derive(rs, &idle, end.At.Add(4*time.Second)); st != contracts.RunWaitingInput || !strings.HasPrefix(why, "turn failed") {
		t.Fatalf("settled: %s (%s)", st, why)
	}
}

func TestSingleQuotaReplay(t *testing.T) {
	recs := loadEvents(t, "events-quota.jsonl")
	a := &Adapter{}
	rs := newRunState()
	for _, r := range recs[:len(recs)-1] {
		rs.applyHook(r)
		if st, why := a.derive(rs, nil, r.At.Add(time.Second)); st != contracts.RunRunning {
			t.Fatalf("%s at %s: %s (%s)", r.Event, r.At.Format("15:04:05.000"), st, why)
		}
	}
	end := recs[len(recs)-1]
	rs.applyHook(end)
	idle := loadScreen(t, "screen-quota.txt")
	if st, _ := a.derive(rs, &idle, end.At.Add(time.Second)); st != contracts.RunRunning {
		t.Fatalf("1 s after the failed Stop: %s", st)
	}
	st, why := a.derive(rs, &idle, end.At.Add(5*time.Second))
	if st != contracts.RunWaitingInput || why != "turn failed (ERROR: usage_limit)" {
		t.Fatalf("settled: %s (%s)", st, why)
	}
	if rs.limit == nil || rs.limit.ResetAt == nil || !rs.limit.ResetAt.Equal(end.At.Add(13*time.Hour+20*time.Minute+47*time.Second).UTC()) {
		t.Fatalf("limit %+v", rs.limit)
	}
	// A turn that then ends normally lifts the limit.
	at := end.At.Add(time.Hour)
	rs.applyHook(hookRecord{Event: "PreInvocation", At: at, ConversationID: convMain})
	rs.applyHook(hookRecord{Event: "Stop", At: at.Add(time.Second), ConversationID: convMain, Termination: "NO_TOOL_CALL", FullyIdle: boolPtr(true)})
	if rs.limit != nil {
		t.Fatalf("limit kept after a normal turn: %+v", rs.limit)
	}
}

// A Stop with fully_idle=false (agy left a background command running) is
// waiting_on_background, and the command's completion wakes the same
// conversation (PostToolUse, PreInvocation) into a running turn.
func TestBackgroundWakeReplay(t *testing.T) {
	recs := loadEvents(t, "events-background-wake.jsonl")
	a := &Adapter{}
	rs := newRunState()
	stopped := false
	for i, r := range recs {
		rs.applyHook(r)
		st, why := a.derive(rs, nil, r.At.Add(10*time.Second))
		switch {
		case r.Event == "Stop":
			stopped = true
			if st != contracts.RunWaitingBackground || !strings.Contains(why, "background work") {
				t.Fatalf("record %d Stop fully_idle=false: %s (%s)", i, st, why)
			}
		case stopped:
			if st != contracts.RunRunning {
				t.Fatalf("record %d %s after the wake: %s (%s)", i, r.Event, st, why)
			}
			return
		}
	}
	t.Fatal("no Stop in the fixture")
}

// The settle restarts whenever the screen shows a running turn again.
func TestTurnEndSettlesOnScreen(t *testing.T) {
	a := &Adapter{}
	rs := newRunState()
	t0 := time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC)
	rs.applyHook(hookRecord{Event: "PreInvocation", At: t0, ConversationID: convMain})
	rs.applyHook(hookRecord{Event: "Stop", At: t0.Add(time.Second), ConversationID: convMain, Termination: "NO_TOOL_CALL", FullyIdle: boolPtr(true)})
	idle, working := parseScreen(realIdle), parseScreen(realWorking)
	if st, _ := a.derive(rs, &working, t0.Add(5*time.Second)); st != contracts.RunRunning {
		t.Fatalf("Stop hook but esc to cancel on screen: %s", st)
	}
	if st, _ := a.derive(rs, &idle, t0.Add(6*time.Second)); st != contracts.RunRunning {
		t.Fatalf("idle prompt just appeared: %s", st)
	}
	if st, _ := a.derive(rs, &working, t0.Add(7*time.Second)); st != contracts.RunRunning {
		t.Fatalf("working again: %s", st)
	}
	if st, _ := a.derive(rs, &idle, t0.Add(8*time.Second)); st != contracts.RunRunning {
		t.Fatalf("idle again (settle restarted): %s", st)
	}
	if st, why := a.derive(rs, &idle, t0.Add(11500*time.Millisecond)); st != contracts.RunWaitingInput || why != "turn ended" {
		t.Fatalf("held idle: %s (%s)", st, why)
	}
	// Hooks say ended but no input box is visible (an unknown layout): the
	// hooks win only after idleUnseenMax.
	none := parseScreen("something agy draws\n")
	rs.idleSince = time.Time{}
	if st, _ := a.derive(rs, &none, t0.Add(30*time.Second)); st != contracts.RunRunning {
		t.Fatalf("no input box 29 s after the Stop: %s", st)
	}
	if st, _ := a.derive(rs, &none, t0.Add(2*time.Minute)); st != contracts.RunWaitingInput {
		t.Fatalf("no input box 2 min after the Stop: %s", st)
	}
}

func TestStopLimit(t *testing.T) {
	at := time.Date(2026, 10, 25, 3, 15, 8, 0, time.UTC)
	quota := "generating and executing: RESOURCE_EXHAUSTED (code 429): Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 13h20m47s."
	le := stopLimit(hookRecord{Event: "Stop", At: at, Termination: "ERROR", Error: quota})
	if le == nil || le.Kind != "usage_limit" || le.ResetAt == nil || !le.ResetAt.Equal(at.Add(13*time.Hour+20*time.Minute+47*time.Second)) ||
		le.Detail != "Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 13h20m47s." {
		t.Fatalf("quota: %+v", le)
	}
	if le := stopLimit(hookRecord{Event: "Stop", At: at, Termination: "ERROR", Error: "RESOURCE_EXHAUSTED (code 429): quota exceeded. Resets in 1d2h."}); le == nil || le.ResetAt == nil || !le.ResetAt.Equal(at.Add(26*time.Hour)) {
		t.Fatalf("days: %+v", le)
	}
	if le := stopLimit(hookRecord{Event: "Stop", At: at, Termination: "ERROR", Error: "generating: RESOURCE_EXHAUSTED (code 429): too many requests"}); le == nil || le.Kind != "rate_limited" || le.ResetAt != nil {
		t.Fatalf("rate: %+v", le)
	}
	for _, r := range []hookRecord{
		{Event: "Stop", At: at, Termination: "ERROR", Error: "API error (attempt 1): request failed: stream idle timeout (5m0s): context deadline exceeded"},
		{Event: "Stop", At: at, Termination: "NO_TOOL_CALL", Error: quota},
		{Event: "PostToolUse", At: at, Termination: "ERROR", Error: quota},
	} {
		if le := stopLimit(r); le != nil {
			t.Errorf("%+v: %+v", r, le)
		}
	}
}

func boolPtr(b bool) *bool { return &b }

// Review cases: an Escape while subagents run, /new after background work,
// a stale limit, and a late subagent hook.
func TestTurnStateEdges(t *testing.T) {
	a := &Adapter{}
	t0 := time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC)
	const sub = "00000000-0000-4000-8000-000000000002"
	idle := parseScreen(realIdle)
	open := func() *runState {
		rs := newRunState()
		rs.applyHook(hookRecord{Event: "SessionStart", At: t0, ConversationID: convMain})
		rs.applyHook(hookRecord{Event: "PreInvocation", At: t0, ConversationID: convMain})
		rs.applyHook(hookRecord{Event: "SessionStart", At: t0.Add(time.Second), ConversationID: sub})
		return rs
	}

	// Escape fires no Stop hook (main or subagent): the idle screen wins
	// once every conversation has been quiet for subQuiet.
	rs := open()
	if st, _ := a.derive(rs, &idle, t0.Add(10*time.Second)); st != contracts.RunRunning {
		t.Fatalf("subagent open, 9 s quiet: %s", st)
	}
	if st, why := a.derive(rs, &idle, t0.Add(40*time.Second)); st != contracts.RunWaitingInput || !strings.Contains(why, "interrupted") {
		t.Fatalf("subagent open, 39 s quiet, idle screen: %s (%s)", st, why)
	}

	// /new after a Stop that left background work: the new conversation
	// becomes the run's.
	rs = newRunState()
	rs.applyHook(hookRecord{Event: "PreInvocation", At: t0, ConversationID: convMain})
	rs.applyHook(hookRecord{Event: "Stop", At: t0.Add(time.Second), ConversationID: convMain, Termination: "NO_TOOL_CALL", FullyIdle: boolPtr(false)})
	const fresh = "00000000-0000-4000-8000-000000000003"
	rs.applyHook(hookRecord{Event: "SessionStart", At: t0.Add(time.Minute), ConversationID: fresh})
	if rs.conversationID != fresh || !rs.turnOpen {
		t.Fatalf("/new: conversation %q open=%v", rs.conversationID, rs.turnOpen)
	}

	// A later failure that names no limit does not read as the old one.
	rs = newRunState()
	quota := "RESOURCE_EXHAUSTED (code 429): Individual quota reached. Resets in 1h."
	rs.applyHook(hookRecord{Event: "PreInvocation", At: t0, ConversationID: convMain})
	rs.applyHook(hookRecord{Event: "Stop", At: t0.Add(time.Second), ConversationID: convMain, Termination: "ERROR", Error: quota, FullyIdle: boolPtr(true)})
	rs.applyHook(hookRecord{Event: "PreInvocation", At: t0.Add(2 * time.Hour), ConversationID: convMain})
	rs.applyHook(hookRecord{Event: "Stop", At: t0.Add(2*time.Hour + time.Second), ConversationID: convMain, Termination: "ERROR", Error: "stream idle timeout", FullyIdle: boolPtr(true)})
	if _, why := rs.hookState(); rs.limit != nil || why != "turn failed (ERROR)" {
		t.Fatalf("stale limit: %+v (%s)", rs.limit, why)
	}

	// A subagent's rate limit is not the run's limit.
	rs = open()
	rs.applyHook(hookRecord{Event: "Stop", At: t0.Add(2 * time.Second), ConversationID: sub, Termination: "ERROR", Error: "RESOURCE_EXHAUSTED (code 429): too many requests"})
	if rs.limit != nil {
		t.Fatalf("subagent rate limit taken: %+v", rs.limit)
	}

	// A subagent hook stamped before a fully idle main Stop but appended
	// after it does not reopen the subagent.
	rs = open()
	rs.applyHook(hookRecord{Event: "Stop", At: t0.Add(5 * time.Second), ConversationID: convMain, Termination: "NO_TOOL_CALL", FullyIdle: boolPtr(true)})
	rs.applyHook(hookRecord{Event: "PostInvocation", At: t0.Add(4 * time.Second), ConversationID: sub})
	if st, why := rs.hookState(); st != contracts.RunWaitingInput {
		t.Fatalf("late subagent hook: %s (%s)", st, why)
	}
}
