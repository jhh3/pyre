package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// A seq read reaches the node as the page that starts at it (so a pyred
// from before seq reads answers it too), and a node that ignores full says
// so instead of returning cut entries silently.
func TestReadSessionSeqAndOldNodeFull(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_1", nodeMac)
	var got contracts.ReadSessionRequest
	x.ch.onSend = func(node string, cmd contracts.NodeCommand) {
		if cmd.Kind != contracts.CmdReadTranscript {
			return
		}
		got = contracts.ReadSessionRequest{}
		_ = json.Unmarshal(cmd.Payload, &got)
		// An old node: no Full in the answer; it returns the page asked for.
		go x.result(node, cmd, contracts.OutcomeSucceeded, contracts.ReadSessionResponse{Entries: []contracts.TranscriptEntry{
			{Seq: 843, Kind: "assistant", Text: "synthetic next"},
		}})
	}
	_, err := x.h.ReadSession(as(owner), contracts.ReadSessionRequest{SessionID: v.Session.SessionID, Seq: 842, Full: true})
	wantCode(t, err, contracts.ErrNotFound) // 842 is not shown; 843 is not it
	if got.Cursor != "841" || got.Limit != 1 || got.Tail || got.Seq != 842 || !got.Full {
		t.Fatalf("node asked %+v", got)
	}
	r, err := x.h.ReadSession(as(owner), contracts.ReadSessionRequest{SessionID: v.Session.SessionID, Seq: 843, Full: true})
	if err != nil || len(r.Entries) != 1 || r.Entries[0].Seq != 843 {
		t.Fatalf("seq read: %v %+v", err, r)
	}
	if !strings.Contains(r.FullNote, "without full reads") {
		t.Fatalf("old node's cut entries not stated: %q", r.FullNote)
	}
	_, err = x.h.ReadSession(as(owner), contracts.ReadSessionRequest{SessionID: v.Session.SessionID, Seq: -1})
	wantCode(t, err, contracts.ErrInvalid)
	// A default read is passed through as before, with no note.
	r, err = x.h.ReadSession(as(owner), contracts.ReadSessionRequest{SessionID: v.Session.SessionID, Tail: true, Limit: 3})
	if err != nil || got.Cursor != "" || got.Limit != 3 || !got.Tail || got.Full || r.FullNote != "" || r.Full {
		t.Fatalf("default read: %v %+v -> %+v", err, got, r)
	}
}

// A default read is redacted at the hub as well, so a pyred from before
// nodes redacted every read does not return secrets; ordinary text is
// unchanged.
func TestReadSessionRedactsOldNodeDefaultRead(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_1", nodeMac)
	plain := "synthetic summary: the token budget held\nnext: key ordering"
	x.ch.onSend = func(node string, cmd contracts.NodeCommand) {
		if cmd.Kind != contracts.CmdReadTranscript {
			return
		}
		go x.result(node, cmd, contracts.OutcomeSucceeded, contracts.ReadSessionResponse{Entries: []contracts.TranscriptEntry{
			{Seq: 1, Kind: "assistant", Text: plain},
			{Seq: 2, Kind: "tool", Text: "command: export GITHUB_TOKEN=ghp_SYNTHETICsyntheticSYNTHETIC1234 (exit 0)"},
		}, Screen: "$ curl -H 'Authorization: Bearer synthetic.bearer.value0123'"})
	}
	r, err := x.h.ReadSession(as(owner), contracts.ReadSessionRequest{SessionID: v.Session.SessionID, Tail: true, Limit: 40})
	if err != nil || len(r.Entries) != 2 {
		t.Fatalf("read: %v %+v", err, r)
	}
	if r.Entries[0].Text != plain {
		t.Fatalf("ordinary text changed: %q", r.Entries[0].Text)
	}
	if strings.Contains(r.Entries[1].Text, "SYNTHETICsynthetic") || !strings.Contains(r.Entries[1].Text, "GITHUB_TOKEN=[REDACTED]") ||
		strings.Contains(r.Screen, "synthetic.bearer") {
		t.Fatalf("not redacted: %q / %q", r.Entries[1].Text, r.Screen)
	}
}

// A session whose latest run failed before it started (a resume whose
// working directory was gone) still reads: the note gives that run's error
// and the read shows the newest run that started, not a node's not_found
// for a run it never recorded (iss_…).
func TestReadSessionLatestRunFailedAtLaunch(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_synth-rf-1", nodeMac)
	first := *v.Run
	first.State, first.StateReason = contracts.RunExited, "process exited"
	x.report(nodeMac, contracts.ReportRun, first)
	x.clock.Add(time.Minute)
	rv, err := x.h.ResumeSession(as(owner), contracts.ResumeRequest{Mutation: contracts.Mutation{OperationID: "op_synth-rf-2"}, SessionID: v.Session.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	rc := x.ch.sentKinds(nodeMac, contracts.CmdResume)
	x.ack(nodeMac, rc[len(rc)-1])
	x.report(nodeMac, contracts.ReportCommandResult, contracts.CommandResult{CommandID: rc[len(rc)-1].CommandID, Kind: rc[len(rc)-1].Kind, Outcome: contracts.OutcomeFailed,
		Error: contracts.Errorf(contracts.ErrInvalid, `working directory "/synth/wt/review" does not exist on this node`), At: x.clock.Now()})
	failed := rv.Session.Run.RunID
	if r, err := x.h.getRun(as(owner), x.h.db, failed); err != nil || !neverStarted(r) {
		t.Fatalf("resume run not failed at launch: %v %+v", err, r)
	}
	var asked contracts.ReadSessionRequest
	x.ch.onSend = func(node string, cmd contracts.NodeCommand) {
		if cmd.Kind != contracts.CmdReadTranscript {
			return
		}
		_ = json.Unmarshal(cmd.Payload, &asked)
		go x.result(node, cmd, contracts.OutcomeSucceeded, contracts.ReadSessionResponse{Entries: []contracts.TranscriptEntry{
			{Seq: 7, Kind: "assistant", Text: "synthetic review verdict: accept"},
		}})
	}
	r, err := x.h.ReadSession(as(owner), contracts.ReadSessionRequest{SessionID: v.Session.SessionID, Tail: true, Limit: 45})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if asked.RunID != first.RunID || r.RunID != first.RunID || len(r.Entries) != 1 {
		t.Fatalf("read %s (asked %s), want %s: %+v", r.RunID, asked.RunID, first.RunID, r)
	}
	if !strings.Contains(r.Note, failed+" failed before it started") || !strings.Contains(r.Note, "does not exist on this node") || !strings.Contains(r.Note, "showing run "+first.RunID) {
		t.Fatalf("note %q", r.Note)
	}
	// A session with no run that started: the error, no entries, no node call.
	resp, err := x.h.StartSession(as(owner), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-rf-3"}, NodeID: nodeMac,
		Harness: contracts.HarnessClaude, Cwd: "/home/synth/gone"})
	if err != nil {
		t.Fatal(err)
	}
	sc := x.ch.sentKinds(nodeMac, contracts.CmdStartRun)
	x.ack(nodeMac, sc[len(sc)-1])
	x.report(nodeMac, contracts.ReportCommandResult, contracts.CommandResult{CommandID: sc[len(sc)-1].CommandID, Kind: sc[len(sc)-1].Kind, Outcome: contracts.OutcomeFailed,
		Error: contracts.Errorf(contracts.ErrInvalid, "synthetic launch error"), At: x.clock.Now()})
	asked = contracts.ReadSessionRequest{}
	r, err = x.h.ReadSession(as(owner), contracts.ReadSessionRequest{SessionID: resp.Session.Session.SessionID, Tail: true})
	if err != nil || len(r.Entries) != 0 || asked.RunID != "" || !strings.Contains(r.Note, "synthetic launch error") || !strings.Contains(r.Note, "no run of this session started") {
		t.Fatalf("never-started session: %v %+v (node asked %+v)", err, r, asked)
	}
}
