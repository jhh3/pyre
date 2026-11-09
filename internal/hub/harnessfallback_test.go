package hub

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// startCodexChild re-hellos the node with Codex installed and starts a
// Codex review under the fixture's parent, as MCP start_session with
// fallback_harness claude sends it (the CLI adds TagFallbackClaude).
func (f *limitFixture) startCodexChild(tags ...string) string {
	x := f.x
	x.t.Helper()
	hello := helloFor(nodeMac, 8, f.runOf(f.parent.Session.SessionID), f.runOf(f.child.Session.SessionID))
	for i := range hello.Harnesses {
		switch hello.Harnesses[i].Harness {
		case contracts.HarnessCodex:
			hello.Harnesses[i] = contracts.HarnessInfo{Harness: contracts.HarnessCodex, Version: "0.162.0", Available: true, Capabilities: claudeCaps}
		case contracts.HarnessClaude:
			hello.Harnesses[i].Efforts = []string{"low", "medium", "high", "xhigh", "max"}
		}
	}
	if _, err := x.h.HandleHello(nodeCtx(nodeMac), nodeMac, hello); err != nil {
		x.t.Fatal(err)
	}
	f.serveLimitResume()
	resp, err := x.h.StartSession(as(sesPrincipal(f.parent.Session.SessionID)), contracts.StartSessionRequest{
		Mutation: contracts.Mutation{OperationID: "op_synth-codex-review"}, NodeID: nodeMac, Harness: contracts.HarnessCodex, Cwd: "/home/synth/review",
		Title: "synthetic review #1", InitialPrompt: "review synthetic PR 1", Tags: append([]string{contracts.TagFallbackClaude}, tags...),
		Args: []string{"--pyre-permissions=bypass", "--pyre-model=gpt-synthetic", "--pyre-effort=ultra"}})
	if err != nil {
		x.t.Fatalf("start codex child: %v", err)
	}
	cmds := x.ch.sentKinds(nodeMac, contracts.CmdStartRun)
	cmd := cmds[len(cmds)-1]
	x.ack(nodeMac, cmd)
	run := *resp.Session.Run
	run.State, run.Adapter = contracts.RunWaitingInput, "codex-synth"
	x.result(nodeMac, cmd, contracts.OutcomeSucceeded, run)
	return resp.Session.Session.SessionID
}

// lastFrom is the latest message the parent received carrying session
// sid's report (alone, or in a digest while the parent is busy).
func (f *limitFixture) lastFrom(sid string) string {
	f.x.t.Helper()
	var body string
	if err := f.x.db.QueryRow(`SELECT envelope FROM messages WHERE recipient_kind='session' AND recipient_id=? AND (sender_id=? OR sender_id='child-reports')
		ORDER BY inbox_cursor DESC LIMIT 1`, f.parent.Session.SessionID, sid).Scan(&body); err != nil {
		f.x.t.Fatalf("no report from %s: %v %v", sid, err, f.statuses())
	}
	var e contracts.MessageEnvelope
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		f.x.t.Fatal(err)
	}
	return e.Body.Text
}

// ambientLimit reports what pyred does when a Codex run on the machine's
// own login hits its weekly limit: the run is stopped (marked) and the plan
// is an intervention (the own login is never switched).
func (f *limitFixture) ambientLimit(sid string, reset time.Time) string {
	f.x.t.Helper()
	run := f.exitForLimit(sid)
	lim := &contracts.LimitEvent{Kind: "usage_limit", Detail: "synthetic: You've hit your usage limit.", ResetAt: &reset, At: f.x.clock.Now()}
	f.x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: sid, FromRunID: run, Outcome: contracts.FallbackIntervention,
		PlannedAt: f.x.clock.Now(), Limit: lim, Reason: "run used the ambient default login; automatic account fallback is disabled for ambient runs"})
	return run
}

// The runtime half of iss_…: a Codex review with
// fallback_harness claude that stops on its usage limit with no other
// OpenAI account restarts on Claude in a new session under the same
// parent (same node, folder, task; the Codex model dropped), instead of
// sitting stopped until the reset; the parent hears both.
func TestCodexLimitRestartsOnClaude(t *testing.T) {
	f := newLimitFixture(t)
	x := f.x
	sid := f.startCodexChild()
	x.clock.Add(time.Minute)
	run := f.ambientLimit(sid, x.clock.Now().Add(5*24*time.Hour))
	if r := f.row(sid); r.Mode != limitResumeHarness || r.State != lrScheduled {
		t.Fatalf("armed: %+v", r)
	}
	f.process()
	if text := f.lastFrom(sid); !strings.Contains(text, "no other OpenAI account has room, so Pyre restarts it on Claude") {
		t.Fatalf("ended report:\n%s", text)
	}
	before := len(x.ch.sentKinds(nodeMac, contracts.CmdStartRun))
	f.processResumes()
	cmds := x.ch.sentKinds(nodeMac, contracts.CmdStartRun)
	if len(cmds) != before+1 {
		t.Fatalf("%d launches, want one more than %d", len(cmds), before)
	}
	var sc contracts.StartRunCommand
	if err := json.Unmarshal(cmds[len(cmds)-1].Payload, &sc); err != nil {
		t.Fatal(err)
	}
	s := sc.Session
	if s.Harness != contracts.HarnessClaude || s.Title != "Codex unavailable, Claude review: synthetic review #1" || s.Lineage.ParentSessionID != f.parent.Session.SessionID ||
		!slices.Contains(s.Tags, contracts.TagCodexUnavailable) || slices.Contains(s.Tags, contracts.TagFallbackClaude) || sc.Cwd != "/home/synth/review" {
		t.Fatalf("stand-in session %+v cwd %q", s, sc.Cwd)
	}
	if strings.Join(sc.Args, " ") != "--pyre-permissions=bypass" {
		t.Fatalf("args %v: the Codex model and an effort Claude lacks must be dropped", sc.Args)
	}
	for _, want := range []string{"[pyre] Codex unavailable, Claude review: Codex session " + sid, "no other Codex (OpenAI) account had room",
		"pyre read " + sid, "model gpt-synthetic and effort ultra", "review synthetic PR 1"} {
		if !strings.Contains(sc.InitialPrompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, sc.InitialPrompt)
		}
	}
	if r := f.row(sid); r.State != lrDone || r.RunID != run || r.ResumedRun != s.CurrentRunID {
		t.Fatalf("row %+v", r)
	}
	f.process()
	if text := f.lastFrom(sid); !strings.Contains(text, "restarted on Claude as session "+s.SessionID) {
		t.Fatalf("restart report:\n%s", text)
	}
	// Once: a second pass starts nothing more.
	f.processResumes()
	if n := len(x.ch.sentKinds(nodeMac, contracts.CmdStartRun)); n != before+1 {
		t.Fatalf("%d launches after a second pass", n)
	}
}

// Without the tag, or with input since the stop, nothing restarts on
// Claude; a Claude session never falls back.
func TestCodexLimitRestartGuards(t *testing.T) {
	sess := contracts.Session{Harness: contracts.HarnessCodex, Tags: []string{contracts.TagFallbackClaude}}
	reset := time.Now().Add(time.Hour)
	plan := contracts.FallbackPlan{Outcome: contracts.FallbackIntervention, Limit: &contracts.LimitEvent{Kind: "usage_limit", ResetAt: &reset},
		Reason: "run used the ambient default login; automatic account fallback is disabled for ambient runs"}
	if !harnessFallbackFor(sess, plan) {
		t.Fatal("tagged Codex on its own login at its limit")
	}
	waiting := plan
	waiting.Outcome, waiting.Reason = contracts.FallbackWaiting, "no eligible openai subscription"
	if !harnessFallbackFor(sess, waiting) {
		t.Fatal("tagged Codex with every account at its limit")
	}
	for name, c := range map[string]struct {
		s contracts.Session
		p contracts.FallbackPlan
	}{
		"untagged":    {contracts.Session{Harness: contracts.HarnessCodex}, plan},
		"claude":      {contracts.Session{Harness: contracts.HarnessClaude, Tags: sess.Tags}, plan},
		"archived":    {contracts.Session{Harness: contracts.HarnessCodex, Tags: sess.Tags, Archived: true}, plan},
		"auth":        {sess, contracts.FallbackPlan{Outcome: contracts.FallbackIntervention, Limit: &contracts.LimitEvent{Kind: "auth_invalid"}}},
		"paused":      {sess, contracts.FallbackPlan{Outcome: contracts.FallbackIntervention, Limit: plan.Limit, Reason: "account switching paused: synthetic"}},
		"not stopped": {sess, contracts.FallbackPlan{Outcome: contracts.FallbackIntervention, Limit: plan.Limit, Reason: "could not quiesce the limited run; synthetic"}},
		"switched":    {sess, contracts.FallbackPlan{Outcome: contracts.FallbackHandoff, Limit: plan.Limit}},
	} {
		if harnessFallbackFor(c.s, c.p) {
			t.Errorf("%s: restarted on Claude", name)
		}
	}

	f := newLimitFixture(t)
	sid := f.startCodexChild()
	f.x.clock.Add(time.Minute)
	f.ambientLimit(sid, f.x.clock.Now().Add(24*time.Hour))
	// The parent messaged it since the stop: someone is dealing with it.
	f.x.clock.Add(time.Second)
	if _, err := f.x.h.SendMessage(as(sesPrincipal(f.parent.Session.SessionID)), contracts.SendMessageRequest{
		Mutation: contracts.Mutation{OperationID: "op_synth-nudge"}, Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sid},
		Body: contracts.MessageBody{Text: "synthetic: I will rerun it myself"}}); err != nil {
		t.Fatal(err)
	}
	before := len(f.x.ch.sentKinds(nodeMac, contracts.CmdStartRun))
	f.processResumes()
	if n := len(f.x.ch.sentKinds(nodeMac, contracts.CmdStartRun)); n != before || f.row(sid).State != lrCancelled {
		t.Fatalf("restarted despite input: %d launches, row %+v", n-before, f.row(sid))
	}
}
