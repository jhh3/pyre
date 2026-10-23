package attention

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Failure groups (owner report 2026-10-17: 332 "Session run failed" items
// in two weeks, 179 on one day): every owner-started run failure joins one
// item per project and cause, with the count and the session list.

func (e *env) runFailedIn(sid, runID string, project *string, reason string, exit *int) {
	r := contracts.Run{RunID: runID, SessionID: sid, Harness: contracts.HarnessClaude, State: contracts.RunFailed, StateReason: reason, ExitCode: exit}
	e.log.AppendPayload(contracts.EvRunEnded, contracts.ObjectRef{Kind: contracts.KindRun, ID: runID}, sid, project, r)
}

// failedItems returns the active failure-group items.
func (e *env) failedItems() []contracts.AttentionItem {
	var out []contracts.AttentionItem
	for _, it := range e.active() {
		if strings.HasPrefix(it.GroupKey, failedPrefix) {
			out = append(out, it)
		}
	}
	return out
}

func (e *env) notifyJobs(attentionID string) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM attention_notify WHERE attention_id=? AND action='notify'`, attentionID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func cwdMissing(dir string) string {
	return `launch failed: working directory "/home/synth/` + dir + `" does not exist on this node`
}

// The same cause in different sessions (different paths) is one item whose
// count and title grow; the owner is notified once, also when a read item
// gets a new member.
func TestFailuresGroupPerCause(t *testing.T) {
	e := newLineageEnv(t, lineageLookup{titles: map[string]string{"ses_synth-1": "review api", "ses_synth-2": "judge ui", "ses_synth-3": "docs pass"}})
	e.runFailedIn("ses_synth-1", "run_synth-1a", nil, cwdMissing("wt-api"), nil)
	e.drain()
	got := e.failedItems()
	if len(got) != 1 {
		t.Fatalf("items: %+v", got)
	}
	one := got[0]
	if one.Title != "'review api' failed to start: working folder missing" || one.Count != 1 || one.SessionID != "" || one.Kind != contracts.AttnFailure ||
		one.Object.ID != "ses_synth-1" || !strings.HasPrefix(one.Summary, "Couldn't start: working folder missing.") || strings.Contains(one.Summary, "/home/synth") {
		t.Fatalf("single failure: %+v", one)
	}
	if _, err := e.store.UpdateAttention(ownerCtx(), contracts.UpdateAttentionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-read"}, AttentionID: one.AttentionID, State: contracts.AttnRead}); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Second)
	e.runFailedIn("ses_synth-2", "run_synth-2a", nil, cwdMissing("wt-ui"), nil)
	e.drain()
	e.clock.Advance(time.Second)
	e.runFailedIn("ses_synth-3", "run_synth-3a", nil, cwdMissing("wt-docs"), nil)
	e.runFailedIn("ses_synth-1", "run_synth-1b", nil, cwdMissing("wt-api"), nil) // a retry is not another session
	e.drain()
	got = e.failedItems()
	if len(got) != 1 {
		t.Fatalf("want one item per cause, got %d: %+v", len(got), got)
	}
	it := got[0]
	if it.AttentionID != one.AttentionID || it.Count != 3 || it.State != contracts.AttnOpen || it.Title != "3 sessions failed to start: working folder missing" ||
		!strings.HasPrefix(it.Summary, "Sessions: 'docs pass', 'judge ui', 'review api'. The working folder does not exist") {
		t.Fatalf("grouped: %+v", it)
	}
	if n := e.notifyJobs(it.AttentionID); n != 1 {
		t.Fatalf("notify jobs = %d, want 1 (on create only)", n)
	}
	// Each session is still flagged by its own failure.
	if _, f := e.flag("ses_synth-2"); f == nil || f.Reason != "failed" || f.Text != "'judge ui' failed to start: working folder missing" {
		t.Fatalf("member flag: %+v", f)
	}
}

// Different causes are different items: crashes group by exit code, launch
// failures by what failed; an unrecognised reason is never copied.
func TestFailuresSeparateCauses(t *testing.T) {
	e := newLineageEnv(t, lineageLookup{titles: map[string]string{"ses_synth-1": "a", "ses_synth-2": "b", "ses_synth-3": "c", "ses_synth-4": "d"}})
	one, two := 1, 2
	e.runFailedIn("ses_synth-1", "run_synth-1", nil, "claude exited", &one)
	e.runFailedIn("ses_synth-2", "run_synth-2", nil, "process exited with status 1 after 4123 ms", &one)
	e.runFailedIn("ses_synth-3", "run_synth-3", nil, "claude exited", &two)
	e.runFailedIn("ses_synth-4", "run_synth-4", nil, "launch failed: SYNTH-SECRET-BODY-TEXT", nil)
	e.drain()
	titles := map[string]int{}
	for _, it := range e.failedItems() {
		titles[it.Title] = it.Count
		if strings.Contains(it.Title+it.Summary+it.GroupKey, "SECRET") {
			t.Fatalf("copied an unrecognised reason: %+v", it)
		}
	}
	want := map[string]int{"2 sessions crashed: exit code 1": 2, "'c' crashed: exit code 2": 1, "'d' failed to start": 1}
	if len(titles) != len(want) {
		t.Fatalf("items: %v", titles)
	}
	for k, v := range want {
		if titles[k] != v {
			t.Fatalf("items: %v, want %v", titles, want)
		}
	}
	// A crash flags its session "needs you", as its own item did.
	if _, f := e.flag("ses_synth-2"); f == nil || f.Level != contracts.FlagNeedsYou || f.Text != "'b' crashed: exit code 1" {
		t.Fatalf("crash flag: %+v", f)
	}

	// Failing again for another cause moves the session.
	e.runFailedIn("ses_synth-3", "run_synth-3b", nil, "claude exited", &one)
	e.drain()
	for _, it := range e.failedItems() {
		if it.Title == "'c' crashed: exit code 2" {
			t.Fatalf("old group kept the session: %+v", it)
		}
	}
	if got := e.byGroup(failedGroup(nil, "run.exit-1")); len(got) != 1 || got[0].Count != 3 {
		t.Fatalf("moved: %+v", got)
	}
}

// Groups are per project; a session outside projects has its own group.
func TestFailuresGroupPerProject(t *testing.T) {
	e := newLineageEnv(t, lineageLookup{})
	p1, p2 := "prj_synth-1", "prj_synth-2"
	e.runFailedIn("ses_synth-1", "run_synth-1", &p1, cwdMissing("x"), nil)
	e.runFailedIn("ses_synth-2", "run_synth-2", &p1, cwdMissing("y"), nil)
	e.runFailedIn("ses_synth-3", "run_synth-3", &p2, cwdMissing("z"), nil)
	e.runFailedIn("ses_synth-4", "run_synth-4", nil, cwdMissing("w"), nil)
	e.drain()
	got := e.failedItems()
	if len(got) != 3 {
		t.Fatalf("want one item per project, got %+v", got)
	}
	for _, it := range got {
		switch {
		case it.ProjectID != nil && *it.ProjectID == p1:
			if it.Count != 2 {
				t.Fatalf("project 1: %+v", it)
			}
		case it.ProjectID != nil && *it.ProjectID == p2, it.ProjectID == nil:
			if it.Count != 1 {
				t.Fatalf("%+v", it)
			}
		default:
			t.Fatalf("unexpected: %+v", it)
		}
	}
	// Archiving and the sweep's store half still resolve members.
	e.archived("ses_synth-1")
	e.drain()
	if it := e.byGroup(failedGroup(&p1, "start.cwd-missing")); len(it) != 1 || it[0].Count != 1 || it[0].State != contracts.AttnOpen {
		t.Fatalf("after archive: %+v", it)
	}
	if ch, err := e.store.ResolveSession(context.Background(), "ses_synth-2", ResolvableKinds, true, e.clock.Now(), "The session was archived."); err != nil || !ch {
		t.Fatalf("sweep: %v %v", ch, err)
	}
	if it := e.byGroup(failedGroup(&p1, "start.cwd-missing")); it[0].State != contracts.AttnWithdrawn {
		t.Fatalf("after the last left: %+v", it)
	}
}
