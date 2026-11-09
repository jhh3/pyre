package hub

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/transport"
)

// ---- archive when done across an account switch (decision I67) ----

// newSwitchFixture is a child session whose credential is real (minted by
// a transport.AuthStore), so a test can check that what the run was handed
// still authenticates. It returns that credential.
func newSwitchFixture(t *testing.T) (*childFixture, *transport.AuthStore, string) {
	t.Helper()
	x := newHarness(t)
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	auth, err := transport.NewAuthStore(db, x.clock)
	if err != nil {
		t.Fatal(err)
	}
	x.tokens.auth = auth
	x.connect(nodeMac, 0)
	x.h.childSummary = func(ctx context.Context, sessionID, runID string) (string, error) {
		return "Synthetic final answer: the tests pass.", nil
	}
	f := &childFixture{x: x, parent: x.start(owner, "op_synth-parent", nodeMac)}
	f.child = f.startChild("op_synth-child", nil)
	starts := x.ch.sentKinds(nodeMac, contracts.CmdStartRun)
	var sc contracts.StartRunCommand
	if err := json.Unmarshal(starts[len(starts)-1].Payload, &sc); err != nil || sc.SessionToken == "" {
		t.Fatalf("the child's start carries its credential: %v", err)
	}
	x.clock.Add(time.Second)
	return f, auth, sc.SessionToken
}

// successor reports the run pyred started for an account switch: same
// session, lineage to the stopped run and, like StartContinuation, no
// start command from the hub, so it runs with the session's existing
// credential.
func (f *childFixture) successor(prev, runID string) {
	f.x.t.Helper()
	old := f.x.session(f.child.Session.SessionID).Run
	r := contracts.Run{RunID: runID, SessionID: f.child.Session.SessionID, NodeID: nodeMac, Adapter: "claude-synth", Harness: contracts.HarnessClaude,
		Cwd: old.Cwd, State: contracts.RunStarting, StateReason: "launching", StartedAt: f.x.clock.Now(), Capabilities: old.Capabilities,
		PreviousRunID: prev, ContinuationKind: contracts.ContinuationNativeResume}
	f.x.report(nodeMac, contracts.ReportRun, r)
}

// iss_…: a session that scheduled its own archive
// hit a rate limit mid-turn; pyred stopped it and resumed it on another
// account. Archive when done took the switch's exit for the end of the
// work, archived the session and revoked its credential, and every Pyre
// call of the resumed run was refused (401). The pending archive now
// follows the successor: the credential keeps working through the switch
// and the turn the successor continues, and is revoked once that is done.
func TestArchiveWhenDoneFollowsAccountSwitch(t *testing.T) {
	f, auth, tok := newSwitchFixture(t)
	x, sid := f.x, f.child.Session.SessionID
	authOK := func(when string) {
		t.Helper()
		p, err := auth.Authenticate(context.Background(), tok)
		if err != nil || p.Kind != contracts.PrincipalSession || p.ID != sid {
			t.Fatalf("%s: the session's credential is refused: %v %+v", when, err, p)
		}
	}
	jh := f.account("personal")
	f.observe(contracts.RunRunning, "working")
	if _, err := archiveWhenDone(x, sesPrincipal(sid), "op_synth-self-archive", sid, true); err != nil {
		t.Fatal(err)
	}
	stops := func() []contracts.NodeCommand { return x.ch.sentKinds(nodeMac, contracts.CmdStop) }

	// The limit ends the turn as a failure; pyred's fallback marks the run
	// and stops it (the hub's own stop may follow).
	f.observe(contracts.RunWaitingInput, "turn failed (StopFailure)")
	f.processArchives()
	old := f.switchExit(contracts.AccountSwitchLimit)
	f.processArchives()
	if v := x.session(sid); v.Session.Archived || !slices.Contains(v.Session.Tags, contracts.TagArchiveWhenDone) {
		t.Fatalf("archived at the account switch: %+v", v.Session)
	}
	if len(x.tokens.revoked) != 0 {
		t.Fatalf("credential revoked at the account switch: %v", x.tokens.revoked)
	}
	authOK("after the switch stop")

	// The successor starts in the same session with the same credential.
	next := "run_synth-switched"
	f.successor(old, next)
	x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: sid, FromRunID: old, ToAccountID: jh,
		Outcome: contracts.FallbackNativeResume, NewRunID: next, PlannedAt: x.clock.Now(), Reason: "rate_limited; native resume"})
	if cur := x.session(sid).Run.RunID; cur != next {
		t.Fatalf("current run %s, want the successor", cur)
	}
	before := len(stops())
	// Idle at launch, before its continue prompt is submitted: not done.
	f.observe(contracts.RunWaitingInput, "session started (resume)")
	f.processArchives()
	if n := len(stops()); n != before {
		t.Fatal("the successor was stopped before the turn it continues")
	}
	authOK("while the successor starts")
	f.observe(contracts.RunRunning, "prompt submitted")
	f.processArchives()
	authOK("while the successor works")

	// Its turn ends: the archive completes as usual.
	f.observe(contracts.RunWaitingInput, "turn ended (Stop)")
	f.processArchives()
	got := stops()
	if len(got) != before+1 {
		t.Fatalf("want one stop of the successor after its turn, got %d", len(got)-before)
	}
	var rc contracts.RunCommand
	if err := json.Unmarshal(got[len(got)-1].Payload, &rc); err != nil || rc.RunID != next {
		t.Fatalf("stop targets %q, want the successor: %v", rc.RunID, err)
	}
	authOK("before the successor exits")
	f.observe(contracts.RunExited, "stopped by pyre (/exit)")
	f.processArchives()
	if !x.session(sid).Session.Archived {
		t.Fatal("not archived once the successor ended")
	}
	if _, err := auth.Authenticate(context.Background(), tok); !contracts.IsCode(err, contracts.ErrUnauthorized) {
		t.Fatalf("credential still valid after the archive: %v", err)
	}
}

// A limit switch's successor whose turn the hub never saw running (shorter
// than an observation) archives once switchResumeSettle has passed.
func TestArchiveWhenDoneSwitchSuccessorSettles(t *testing.T) {
	f, _, _ := newSwitchFixture(t)
	x, sid := f.x, f.child.Session.SessionID
	f.observe(contracts.RunRunning, "working")
	if _, err := archiveWhenDone(x, sesPrincipal(sid), "op_synth-self-archive", sid, true); err != nil {
		t.Fatal(err)
	}
	old := f.switchExit(contracts.AccountSwitchLimit)
	f.successor(old, "run_synth-switched")
	f.observe(contracts.RunWaitingInput, "turn ended (Stop)")
	f.processArchives()
	if n := len(x.ch.sentKinds(nodeMac, contracts.CmdStop)); n != 0 {
		t.Fatalf("stopped within the settle window: %d", n)
	}
	x.clock.Add(switchResumeSettle)
	f.processArchives()
	if n := len(x.ch.sentKinds(nodeMac, contracts.CmdStop)); n != 1 {
		t.Fatalf("want one stop after the settle window, got %d", n)
	}
}

// A proactive move's successor waits idle for its next message: its idle
// is the end of the turn, so a pending archive stops it at once.
func TestArchiveWhenDoneProactiveSwitchSuccessorIdle(t *testing.T) {
	f, _, _ := newSwitchFixture(t)
	x, sid := f.x, f.child.Session.SessionID
	f.observe(contracts.RunRunning, "working")
	if _, err := archiveWhenDone(x, sesPrincipal(sid), "op_synth-self-archive", sid, true); err != nil {
		t.Fatal(err)
	}
	old := f.switchExit(contracts.AccountSwitchProactive)
	f.processArchives()
	if x.session(sid).Session.Archived {
		t.Fatal("archived at the move")
	}
	f.successor(old, "run_synth-moved")
	f.observe(contracts.RunWaitingInput, "session started (resume)")
	f.processArchives()
	if n := len(x.ch.sentKinds(nodeMac, contracts.CmdStop)); n != 1 {
		t.Fatalf("want the idle successor stopped, got %d stops", n)
	}
}

// Asked to archive when done between the switch's stop and the
// successor's start, the session is scheduled, not archived on the spot
// (which would revoke the credential the successor is about to use). Once
// the node has said where the switch went, an ended session is archived at
// once as before.
func TestArchiveWhenDoneDuringAccountSwitchSchedules(t *testing.T) {
	f, auth, tok := newSwitchFixture(t)
	x, sid, parent := f.x, f.child.Session.SessionID, f.parent.Session.SessionID
	f.observe(contracts.RunRunning, "working")
	old := f.switchExit(contracts.AccountSwitchLimit)
	v, err := archiveWhenDone(x, sesPrincipal(parent), "op_synth-p-archive", sid, true)
	if err != nil {
		t.Fatal(err)
	}
	if v.Session.Archived || !slices.Contains(v.Session.Tags, contracts.TagArchiveWhenDone) || len(x.tokens.revoked) != 0 {
		t.Fatalf("archived mid-switch: %+v revoked %v", v.Session, x.tokens.revoked)
	}
	if _, err := auth.Authenticate(context.Background(), tok); err != nil {
		t.Fatalf("credential refused mid-switch: %v", err)
	}
	f.successor(old, "run_synth-switched")
	if v := x.session(sid); v.Session.Archived || v.Run.RunID != "run_synth-switched" {
		t.Fatalf("successor: %+v", v.Session)
	}

	// A switch that left the session ended (no account had room): an
	// explicit archive when done archives it now.
	g, _, _ := newSwitchFixture(t)
	gs := g.child.Session.SessionID
	g.observe(contracts.RunRunning, "working")
	gold := g.switchExit(contracts.AccountSwitchLimit)
	g.x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: gs, FromRunID: gold,
		Outcome: contracts.FallbackWaiting, PlannedAt: g.x.clock.Now(), Reason: "no eligible account"})
	if v, err := archiveWhenDone(g.x, sesPrincipal(g.parent.Session.SessionID), "op_synth-g-archive", gs, true); err != nil || !v.Session.Archived {
		t.Fatalf("ended after the switch: archived now: %v %+v", err, v.Session)
	}
}
