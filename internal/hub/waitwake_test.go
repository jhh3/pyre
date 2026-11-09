package hub

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// ---- iss_…: a child's progress note is not its
// result; a wait survives Pyre's background-command wake and wakes the
// session at its expected time ----

func tool(text string) contracts.TranscriptEntry {
	return contracts.TranscriptEntry{Kind: "tool", Text: text}
}

func say(text string) contracts.TranscriptEntry {
	return contracts.TranscriptEntry{Kind: "assistant", Text: text}
}

func TestWorkedAfterSend(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []contracts.TranscriptEntry
		want    bool
	}{
		{"codex: progress note, then push and archive (the incident)", []contracts.TranscriptEntry{
			tool("command: /usr/bin/zsh -lc 'make total-format' (exit 0)"), tool("mcp: pyre.send_message (completed)"),
			tool("command: /usr/bin/zsh -lc 'pytest tests/synth' (exit 0)"), say("checks pass"),
			tool("command: /usr/bin/zsh -lc 'git commit -m synth && git push' (exit 0)"), tool("mcp: pyre.archive_session (completed)"),
			say("Pushed synth-branch."),
		}, true},
		{"codex: result sent, then only Pyre bookkeeping", []contracts.TranscriptEntry{
			tool("command: /usr/bin/zsh -lc 'git push' (exit 0)"), tool("mcp: pyre.send_message (completed)"),
			tool("mcp: pyre.archive_session (completed)"), say("Sent the result to the parent."),
		}, false},
		{"claude: result sent, then archive", []contracts.TranscriptEntry{
			tool("Bash: git push"), tool("tool result"), tool("mcp__pyre__send_message"), tool("tool result"),
			tool("ToolSearch"), tool("mcp__pyre__archive_session"), say("Done."),
		}, false},
		{"claude: progress note, then edits", []contracts.TranscriptEntry{
			tool("mcp__pyre__send_message"), tool("tool result"), tool("Edit: /synth/a.go; Bash: go test ./synth"), say("Done."),
		}, true},
		{"shell pyre send, then work", []contracts.TranscriptEntry{
			tool("command: /usr/bin/zsh -lc 'pyre --json send ses_synth \"halfway\"' (exit 0)"), tool("file change: /synth/a.go"),
		}, true},
		{"shell pyre send last", []contracts.TranscriptEntry{
			tool("file change: /synth/a.go"), tool("Bash: pyre send ses_synth done"),
		}, false},
		{"no send in the tail, work in it", []contracts.TranscriptEntry{tool("file change: /synth/a.go"), say("Done.")}, true},
		{"no send, no work", []contracts.TranscriptEntry{say("Done.")}, false},
	} {
		if got := workedAfterSend(tc.entries); got != tc.want {
			t.Errorf("%s: workedAfterSend = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The incident: a child steers a progress note to its parent ("final
// checks running, will push shortly"), keeps working, pushes, archives
// itself and ends with its result a minute later. The parent hears the
// result; a child whose message was its last act is still not reported
// twice.
func TestChildReportAfterProgressNoteThenWork(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tail    []contracts.TranscriptEntry
		reports int
	}{
		{"worked after the note", []contracts.TranscriptEntry{
			tool("mcp: pyre.send_message (completed)"), tool("command: /usr/bin/zsh -lc 'git push' (exit 0)"),
			tool("mcp: pyre.archive_session (completed)"), say("Pushed synth-branch."),
		}, 1},
		{"the note was its last act", []contracts.TranscriptEntry{
			tool("command: /usr/bin/zsh -lc 'git push' (exit 0)"), tool("mcp: pyre.send_message (completed)"),
			tool("mcp: pyre.archive_session (completed)"), say("Sent."),
		}, 0},
	} {
		for _, between := range []bool{false, true} {
			f := newChildFixture(t, nil)
			f.x.h.childTail = func(ctx context.Context, sessionID, runID string) ([]contracts.TranscriptEntry, error) {
				return tc.tail, nil
			}
			sid, parent := f.child.Session.SessionID, f.parent.Session.SessionID
			f.observe(contracts.RunRunning, "working")
			if _, err := f.x.h.SendMessage(as(sesPrincipal(sid)), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_synth-progress"},
				Recipient: contracts.Address{Kind: contracts.AddrSession, ID: parent}, Mode: contracts.ModeSteer,
				Body: contracts.MessageBody{Text: "synthetic progress: final checks running, will push shortly"}}); err != nil {
				t.Fatal(err)
			}
			f.x.clock.Add(90 * time.Second)
			if _, err := archiveWhenDone(f.x, sesPrincipal(sid), "op_synth-self-archive", sid, true); err != nil {
				t.Fatal(err)
			}
			f.finishTurn(between)
			f.process()
			live := f.live()
			if len(live) != 1+tc.reports {
				t.Fatalf("%s (between=%v): want the note and %d report(s): %d %v", tc.name, between, tc.reports, len(live), f.statuses())
			}
			if tc.reports == 1 && !strings.Contains(live[1].Body.Text, "Synthetic final answer") {
				t.Fatalf("%s: the report lacks the last message:\n%s", tc.name, live[1].Body.Text)
			}
		}
	}
}

// backgroundWake reports Pyre's background-command wake of the session's
// current run (codex bgwake.go).
func backgroundWake(x *harness, sessionID string) {
	x.t.Helper()
	r := *x.session(sessionID).Run
	b, _ := json.Marshal(contracts.NodeAutomation{RunID: r.RunID, SessionID: sessionID, Harness: contracts.HarnessCodex,
		Action: contracts.AutomationBackgroundWake, Detail: "synthetic script"})
	x.report(nodeMac, contracts.ReportEvent, contracts.Event{Kind: contracts.EvNodeAutomation, Object: contracts.ObjectRef{Kind: contracts.KindRun, ID: r.RunID}, SessionID: sessionID, Payload: b})
}

// wakes lists Pyre's deadline wakes sent to sessionID.
func wakes(x *harness, sessionID string) []string {
	x.t.Helper()
	rows, err := x.db.Query(`SELECT envelope FROM messages WHERE recipient_kind='session' AND recipient_id=? AND sender_kind='system' AND sender_id=? ORDER BY inbox_cursor`,
		sessionID, waitWakeSender.ID)
	if err != nil {
		x.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var body string
		rows.Scan(&body)
		var e contracts.MessageEnvelope
		json.Unmarshal([]byte(body), &e)
		out = append(out, e.Body.Text)
	}
	return out
}

// The incident's second half: a lead (itself a child) sets a quiet
// 15-minute wait and ends its turn; a second later Pyre wakes it because
// a background script finished, and it ends that turn in seconds without
// a new wait. The wait still stands: at its expected time Pyre wakes the
// lead itself, and its parent hears it may be stuck.
func TestQuietWaitSurvivesBackgroundWakeTurn(t *testing.T) {
	for _, wake := range []bool{true, false} {
		f := newChildFixture(t, nil)
		sid := f.child.Session.SessionID
		f.reportedTurn()
		f.observe(contracts.RunRunning, "working")
		f.waitQuiet("synthetic worker ses_synth", 15)
		f.observe(contracts.RunWaitingInput, "idle")
		f.process()
		f.x.clock.Add(time.Second)
		if wake {
			backgroundWake(f.x, sid)
		}
		f.observe(contracts.RunRunning, "the background command finished")
		f.x.clock.Add(5 * time.Second)
		f.observe(contracts.RunWaitingInput, "idle")
		f.process()
		if !wake {
			// A turn something else started (a message, the owner) ends
			// the wait as before: its end is reported, no wake follows.
			f.x.clock.Add(16 * time.Minute)
			f.process()
			if w := wakes(f.x, sid); len(w) != 0 {
				t.Fatalf("woken after a turn of its own: %v", w)
			}
			if n := len(f.allToParent()); n != 2 {
				t.Fatalf("want the later turn's idle report only: %d %v", n, f.statuses())
			}
			continue
		}
		if n := len(f.allToParent()); n != 1 {
			t.Fatalf("the wake turn of a quiet wait was reported: %v", f.statuses())
		}
		f.x.clock.Add(14 * time.Minute)
		f.process()
		if w := wakes(f.x, sid); len(w) != 0 {
			t.Fatalf("woken before the expected time: %v", w)
		}
		f.x.clock.Add(time.Minute)
		f.process()
		w := wakes(f.x, sid)
		if len(w) != 1 || !strings.Contains(w[0], "synthetic worker ses_synth") || !strings.Contains(w[0], "Check on it") {
			t.Fatalf("deadline wake: %q", w)
		}
		msgs := f.toParent()
		if len(msgs) != 2 || !strings.Contains(msgs[1].Body.Text, "may be stuck") {
			t.Fatalf("the parent's overdue report: %d %v", len(msgs), f.statuses())
		}
		// Once per wait.
		f.x.clock.Add(10 * time.Minute)
		f.process()
		if n := len(wakes(f.x, sid)); n != 1 {
			t.Fatalf("woken %d times", n)
		}
	}
}

// A top-level session's wait wakes it at the expected time while it is
// idle in that turn; not when a later turn started, a new wait replaced
// it, or it is busy (then it is woken once it goes idle).
func TestLeadWaitDeadlineWake(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	x.h.childSummary = func(ctx context.Context, sessionID, runID string) (string, error) { return "Synthetic status.", nil }
	lead := x.start(owner, "op_synth-lead", nodeMac)
	sid := lead.Session.SessionID
	observe := func(st contracts.RunState) {
		r := *x.session(sid).Run
		r.State = st
		x.report(nodeMac, contracts.ReportRun, r)
	}
	wait := func(op string, minutes int) {
		if _, err := x.h.SetWaitingOn(as(sesPrincipal(sid)), contracts.SetWaitingOnRequest{
			Mutation: contracts.Mutation{OperationID: op}, What: "synthetic CI", ExpectMinutes: minutes, Quiet: true}); err != nil {
			t.Fatal(err)
		}
	}
	process := func() {
		if _, err := x.h.ProcessChildReports(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	observe(contracts.RunRunning)
	wait("op_synth-wait-1", 10)
	observe(contracts.RunWaitingInput)
	x.clock.Add(11 * time.Minute)
	process()
	if w := wakes(x, sid); len(w) != 1 || !strings.Contains(w[0], "synthetic CI") {
		t.Fatalf("idle lead past its wait: %q", w)
	}

	// Busy at the expected time (the turn that set it still runs): woken
	// once that turn ends without anything else waking it.
	observe(contracts.RunRunning)
	wait("op_synth-wait-2", 10)
	x.clock.Add(11 * time.Minute)
	process()
	if n := len(wakes(x, sid)); n != 1 {
		t.Fatalf("woken mid-turn: %d", n)
	}
	observe(contracts.RunWaitingInput)
	x.clock.Add(time.Minute)
	process()
	if n := len(wakes(x, sid)); n != 2 {
		t.Fatalf("not woken once idle: %d", n)
	}

	// A later turn of its own ends the wait.
	observe(contracts.RunRunning)
	wait("op_synth-wait-3", 10)
	observe(contracts.RunWaitingInput)
	x.clock.Add(3 * time.Minute)
	observe(contracts.RunRunning)
	observe(contracts.RunWaitingInput)
	x.clock.Add(10 * time.Minute)
	process()
	if n := len(wakes(x, sid)); n != 2 {
		t.Fatalf("woken after a later turn: %d", n)
	}
}

// iss_…: a child pushes, calls archive_session
// and ends its turn while a background task (a monitor) still runs. Its
// run waits on background work, not for input; the archive proceeds (the
// stop ends what "may start a new turn") and the parent gets the result.
func TestSelfArchiveWithBackgroundTaskLeft(t *testing.T) {
	f := newChildFixture(t, nil)
	sid := f.child.Session.SessionID
	f.observe(contracts.RunRunning, "working")
	if _, err := archiveWhenDone(f.x, sesPrincipal(sid), "op_synth-self-archive", sid, true); err != nil {
		t.Fatal(err)
	}
	f.observe(contracts.RunWaitingBackground, "turn ended; 1 background task(s) may start a new turn")
	f.process()
	f.x.clock.Add(time.Second)
	f.stopAndArchive()
	f.process()
	msgs := f.live()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body.Text, "Synthetic final answer") {
		t.Fatalf("want the final report: %d %v", len(msgs), f.statuses())
	}
}

// iss_…: a child sets a quiet 40-minute wait and
// ends its turn; a background task's notification starts the next turn
// two seconds later, so the hub sees it running throughout. Six minutes on
// it ends that turn with its result. The parent hears that turn's end, and
// the finished wait neither wakes the child nor tells the parent it looks
// stalled. Without the new prompt the wait still covers the turn's end.
func TestQuietWaitEndsAtUnobservedNewTurn(t *testing.T) {
	for _, prompt := range []bool{true, false} {
		f := newChildFixture(t, nil)
		sid := f.child.Session.SessionID
		f.reportedTurn()
		f.observe(contracts.RunRunning, "working")
		f.waitQuiet("synthetic e2e run", 40)
		f.x.clock.Add(8 * time.Second)
		if prompt {
			f.observe(contracts.RunRunning, contracts.RunReasonPromptSubmitted)
		}
		f.x.clock.Add(6 * time.Minute)
		f.observe(contracts.RunRunning, "tool finished: Bash")
		f.observe(contracts.RunWaitingInput, "turn ended (Stop)")
		f.process()
		if !prompt {
			if n := len(f.allToParent()); n != 1 {
				t.Fatalf("the quiet wait's own turn end was reported: %v", f.statuses())
			}
			continue
		}
		msgs := f.toParent()
		if len(msgs) != 2 || !strings.Contains(msgs[1].Body.Text, "finished its turn and is waiting for input") {
			t.Fatalf("want the later turn's idle report: %d %v", len(msgs), f.statuses())
		}
		f.x.clock.Add(41 * time.Minute)
		f.process()
		if w := wakes(f.x, sid); len(w) != 0 {
			t.Fatalf("woken for a wait whose turn is over: %v", w)
		}
		if n := len(f.allToParent()); n != 2 {
			t.Fatalf("an overdue report followed: %v", f.statuses())
		}
	}
}
