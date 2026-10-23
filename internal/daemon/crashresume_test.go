package daemon

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/resumenote"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// orderAdapter records the session of every launch, in order.
type orderAdapter struct {
	*countingAdapter
	mu       sync.Mutex
	sessions []string
}

func (o *orderAdapter) Launch(ctx context.Context, l contracts.ResolvedLaunch) (contracts.RunHandle, error) {
	o.mu.Lock()
	o.sessions = append(o.sessions, l.SessionID)
	o.mu.Unlock()
	return o.countingAdapter.Launch(ctx, l)
}

func (o *orderAdapter) launched() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.sessions)
}

func shortCrashResume(t *testing.T) {
	gap, wait := crashResumeGap, crashResumeLiveWait
	crashResumeGap, crashResumeLiveWait = 10*time.Millisecond, 5*time.Second
	t.Cleanup(func() { crashResumeGap, crashResumeLiveWait = gap, wait })
}

// latestRun is the session's newest run on the node.
func (n *node) latestRun(sessionID string) runRecord {
	n.t.Helper()
	rec, ok, err := n.d.st.latestRunForSession(context.Background(), sessionID)
	if err != nil || !ok {
		n.t.Fatalf("no run for %s: %v", sessionID, err)
	}
	if lr, live := n.d.getLive(rec.Run.RunID); live {
		rec, _ = lr.snapshot()
	}
	return rec
}

func (n *node) outboxBody(messageID string) (string, bool) {
	n.t.Helper()
	var req []byte
	if err := n.db.QueryRow(`SELECT request FROM daemon_outbox WHERE message_id=?`, messageID).Scan(&req); err != nil {
		return "", false
	}
	var r contracts.SendMessageRequest
	if err := json.Unmarshal(req, &r); err != nil {
		n.t.Fatal(err)
	}
	return r.Body.Text, true
}

// After an unclean restart (every process died underneath pyred), live
// sessions are natively resumed by pyred itself, parents before children,
// and each gets the crash note (held in the outbox: no hub here); a run
// whose stop was requested is left ended, and a crash-resumed run that dies
// again right away is not resumed a second time.
func TestCrashResumeAfterUncleanRestart(t *testing.T) {
	shortCrashResume(t)
	ma := mock.NewAdapter()
	ad := &orderAdapter{countingAdapter: &countingAdapter{RuntimeAdapter: ma}}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{ad}})
	cwd := t.TempDir()
	start := func(session, run, parent string) {
		sc := startCmd(session, run, contracts.HarnessMock, cwd)
		sc.SessionToken = "tok-synthetic-" + session
		sc.Session.Lineage.ParentSessionID = parent
		n.start(sc)
	}
	// The child was started first and sorts first by ID: the resume order
	// must come from lineage.
	start("ses_achild", "run_c1", "ses_zparent")
	start("ses_zparent", "run_p1", "")
	start("ses_stopped", "run_s1", "")
	// The child is mid-turn; the parent idles at its prompt.
	ma.SetState("run_c1", contracts.RunRunning)
	n.d.PollNow(context.Background(), "run_c1")
	if s := n.run("run_c1").Run.State; s != contracts.RunRunning {
		t.Fatalf("child state = %s", s)
	}
	// A stop was requested but pyred died before it observed the exit.
	lr, _ := n.d.getLive("run_s1")
	n.d.markStopRequested(context.Background(), lr, "owner:test")

	// Everything dies with pyred (systemd user manager killed).
	n.stop()
	for _, r := range []string{"run_p1", "run_c1", "run_s1"} {
		ma.Vanish(r)
	}
	n = n.restart()

	resumed := map[string]runRecord{}
	waitFor(t, "crash resumes up", func() bool {
		for _, s := range []string{"ses_zparent", "ses_achild"} {
			r := n.latestRun(s)
			if r.Run.PreviousRunID == "" || !slices.Contains(crashResumeLive, r.Run.State) {
				return false
			}
			resumed[s] = r
		}
		return true
	})
	for s, prev := range map[string]string{"ses_zparent": "run_p1", "ses_achild": "run_c1"} {
		r := resumed[s]
		if r.Run.PreviousRunID != prev || r.Run.ContinuationKind != contracts.ContinuationNativeResume || r.Run.AdapterData[adapterDataCrashResumed] == "" {
			t.Fatalf("%s resumed run = %+v", s, r.Run)
		}
		old := n.run(prev)
		if old.Phase != phaseEnded || old.Run.State != contracts.RunExited || !strings.Contains(old.Run.StateReason, "exit not observed") {
			t.Fatalf("old run %s = %s %s: %s", prev, old.Phase, old.Run.State, old.Run.StateReason)
		}
		// The native thread continues; nothing is replayed as a prompt.
		if r.Handle == nil || r.Handle.NativeRef == nil || r.Handle.NativeRef.ID != "mock-"+prev {
			t.Fatalf("%s native ref = %+v", s, r.Handle)
		}
	}
	if got := ad.launched(); !slices.Equal(got[3:], []string{"ses_zparent", "ses_achild"}) {
		t.Fatalf("launch order after restart = %v, want parent then child", got[3:])
	}
	if s := n.latestRun("ses_stopped"); s.Run.RunID != "run_s1" || s.Phase != phaseEnded {
		t.Fatalf("stopped session was resumed: %+v", s.Run)
	}

	// Each resumed session gets the note once, under the shared stable ID.
	for _, s := range []string{"ses_zparent", "ses_achild"} {
		id := resumenote.MessageID(s, resumed[s].Run.RunID)
		var body string
		waitFor(t, "note to "+s, func() bool {
			var ok bool
			body, ok = n.outboxBody(id)
			return ok
		})
		for _, want := range []string{"Pyre restarted after a crash", "died at about", "check in with it before continuing"} {
			if !strings.Contains(body, want) {
				t.Fatalf("note to %s lacks %q: %s", s, want, body)
			}
		}
	}
	var notes int
	n.db.QueryRow(`SELECT COUNT(*) FROM daemon_outbox`).Scan(&notes)
	if notes != 2 {
		t.Fatalf("outbox has %d messages, want 2", notes)
	}

	// The crash-resumed runs die again at once: no second resume (no loop).
	launches := ad.launches.Load()
	n.stop()
	for _, r := range resumed {
		ma.Vanish(r.Run.RunID)
	}
	n = n.restart()
	for s, r := range resumed {
		got := n.run(r.Run.RunID)
		if got.Phase != phaseEnded {
			t.Fatalf("%s re-crashed run = %s", s, got.Phase)
		}
		if ok, why := n.d.crashResumeCandidate(runRecord{Session: got.Session, Run: func() contracts.Run {
			run := got.Run
			run.State = contracts.RunWaitingInput
			return run
		}()}); ok || !strings.Contains(why, "crash loop") {
			t.Fatalf("%s candidate again: %v %s", s, ok, why)
		}
	}
	time.Sleep(100 * time.Millisecond) // well past crashResumeGap
	n.d.WaitLaunches()
	if ad.launches.Load() != launches {
		t.Fatalf("launches after re-crash = %d, want %d (no crash loop)", ad.launches.Load(), launches)
	}
	for s, r := range resumed {
		if l := n.latestRun(s); l.Run.RunID != r.Run.RunID {
			t.Fatalf("%s resumed again as %s", s, l.Run.RunID)
		}
	}
}

func TestCrashResumeOrderAndPolicy(t *testing.T) {
	rec := func(sid, parent string) runRecord {
		return runRecord{Session: contracts.Session{SessionID: sid, Lineage: contracts.Lineage{ParentSessionID: parent}}}
	}
	recs := []runRecord{rec("ses_a", "ses_b"), rec("ses_b", "ses_c"), rec("ses_c", ""), rec("ses_d", "ses_gone")}
	crashResumeOrder(recs)
	var got []string
	for _, r := range recs {
		got = append(got, r.Session.SessionID)
	}
	// ses_d's parent is not being resumed: it is a root.
	if want := []string{"ses_c", "ses_d", "ses_b", "ses_a"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if p := crashResumePolicy(contracts.Run{Harness: contracts.HarnessClaude, AccountID: "acct_x"}); p.Mode != contracts.AccountExplicit || p.AccountID != "acct_x" || p.Strict {
		t.Fatalf("account policy = %+v", p)
	}
	if p := crashResumePolicy(contracts.Run{Harness: contracts.HarnessClaude, AdapterData: map[string]string{"pyre.account": "ambient"}}); p.Mode != contracts.AccountAmbient {
		t.Fatalf("ambient policy = %+v", p)
	}
	if p := crashResumePolicy(contracts.Run{Harness: contracts.HarnessMock}); p.Mode != contracts.AccountEligible {
		t.Fatalf("mock policy = %+v", p)
	}
	d := &Daemon{cfg: Config{Clock: contracts.SystemClock{}}}
	for _, c := range []struct {
		run  contracts.Run
		want bool
	}{
		{contracts.Run{Harness: contracts.HarnessClaude, State: contracts.RunWaitingInput, Capabilities: []string{contracts.CapResumeNative}}, true},
		{contracts.Run{Harness: contracts.HarnessClaude, State: contracts.RunWaitingApproval, Capabilities: []string{contracts.CapResumeNative}}, true},
		{contracts.Run{Harness: contracts.HarnessClaude, State: contracts.RunUnknown, Capabilities: []string{contracts.CapResumeNative}}, false},
		{contracts.Run{Harness: contracts.HarnessCodex, State: contracts.RunRunning, Capabilities: []string{contracts.CapResumeNative}}, false},
		{contracts.Run{Harness: contracts.HarnessShell, State: contracts.RunWaitingInput}, false},
		{contracts.Run{Harness: contracts.HarnessClaude, State: contracts.RunRunning}, false},
	} {
		if ok, why := d.crashResumeCandidate(runRecord{Run: c.run}); ok != c.want {
			t.Errorf("%s %s: candidate = %v (%s), want %v", c.run.Harness, c.run.State, ok, why, c.want)
		}
	}
}

// crashHub answers the hub calls crash resume makes (GetSession, the note's
// SendMessage, delivery-facts GetMessage) and records the credentials used.
type crashHub struct {
	mu          sync.Mutex
	unreachable bool
	sessions    map[string]contracts.SessionView
	errs        map[string]error
	getBearers  []string
	sent        []contracts.SendMessageRequest
}

type crashHubAPI struct {
	contracts.API
	h      *crashHub
	bearer string
}

func (f crashHubAPI) GetSession(ctx context.Context, r contracts.GetRequest) (contracts.SessionView, error) {
	f.h.mu.Lock()
	defer f.h.mu.Unlock()
	f.h.getBearers = append(f.h.getBearers, f.bearer)
	if f.h.unreachable {
		return contracts.SessionView{}, contracts.Errorf(contracts.ErrUnavailable, "synthetic: hub unreachable")
	}
	if err := f.h.errs[r.ID]; err != nil {
		return contracts.SessionView{}, err
	}
	sv, ok := f.h.sessions[r.ID]
	if !ok {
		return contracts.SessionView{}, contracts.Errorf(contracts.ErrNotFound, "synthetic: no session %s", r.ID)
	}
	return sv, nil
}

func (f crashHubAPI) SendMessage(ctx context.Context, r contracts.SendMessageRequest) (contracts.MessageView, error) {
	f.h.mu.Lock()
	defer f.h.mu.Unlock()
	if f.h.unreachable {
		return contracts.MessageView{}, contracts.Errorf(contracts.ErrUnavailable, "synthetic: hub unreachable")
	}
	f.h.sent = append(f.h.sent, r)
	return contracts.MessageView{Envelope: contracts.MessageEnvelope{MessageID: r.MessageID, Recipient: r.Recipient, Body: r.Body}}, nil
}

func (f crashHubAPI) GetMessage(ctx context.Context, r contracts.GetRequest) (contracts.MessageView, error) {
	return contracts.MessageView{}, contracts.Errorf(contracts.ErrUnavailable, "synthetic")
}

func (h *crashHub) set(fn func(h *crashHub)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(h)
}

func (h *crashHub) sentTo(sessionID string) []contracts.SendMessageRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []contracts.SendMessageRequest
	for _, r := range h.sent {
		if r.Recipient.ID == sessionID {
			out = append(out, r)
		}
	}
	return out
}

const crashNodeToken = "node-tok-synthetic-crash"

// crashHubNode starts a node whose hub is h, launches one idle session per
// ID (the hub records each as current), kills every process with pyred and
// restarts it.
func crashHubNode(t *testing.T, h *crashHub, ad contracts.RuntimeAdapter, sessions ...string) *node {
	t.Helper()
	cfg := Config{Adapters: []contracts.RuntimeAdapter{ad}, NodeToken: crashNodeToken,
		Upstream: func(bearer string) contracts.API { return crashHubAPI{h: h, bearer: bearer} }}
	n := startNode(t, t.TempDir(), cfg)
	cwd := t.TempDir()
	h.set(func(h *crashHub) {
		if h.sessions == nil {
			h.sessions = map[string]contracts.SessionView{}
		}
	})
	for _, s := range sessions {
		run := "run_" + strings.TrimPrefix(s, "ses_") + "1"
		sc := startCmd(s, run, contracts.HarnessMock, cwd)
		sc.SessionToken = "tok-synthetic-" + s
		n.start(sc)
		h.set(func(h *crashHub) {
			if _, ok := h.sessions[s]; !ok {
				h.sessions[s] = contracts.SessionView{Session: contracts.Session{SessionID: s, CurrentRunID: run}}
			}
		})
	}
	n.stop()
	for _, s := range sessions {
		ad.(*mock.Adapter).Vanish("run_" + strings.TrimPrefix(s, "ses_") + "1")
	}
	return n.restart()
}

func oldRunOf(s string) string { return "run_" + strings.TrimPrefix(s, "ses_") + "1" }

// resumedAs waits until the session's newest run is a crash resume that is
// up, and returns it.
func (n *node) resumedAs(s string) runRecord {
	n.t.Helper()
	var r runRecord
	waitFor(n.t, "crash resume of "+s, func() bool {
		r = n.latestRun(s)
		return r.Run.PreviousRunID == oldRunOf(s) && slices.Contains(crashResumeLive, r.Run.State)
	})
	return r
}

// The hub's view decides, fail closed and with the node's own credential:
// an archived session, one the hub does not know or refuses, and one
// resumed elsewhere stay ended; a drained one is resumed without pyred's
// note (pyre node resume sends the owner's); the rest are resumed and noted
// through the hub.
func TestCrashResumeHubView(t *testing.T) {
	shortCrashResume(t)
	h := &crashHub{sessions: map[string]contracts.SessionView{
		"ses_arch":    {Session: contracts.Session{SessionID: "ses_arch", CurrentRunID: "run_arch1", Archived: true}},
		"ses_moved":   {Session: contracts.Session{SessionID: "ses_moved", CurrentRunID: "run_01SYNTHELSEWHERE"}},
		"ses_drained": {Session: contracts.Session{SessionID: "ses_drained", CurrentRunID: "run_drained1", Tags: []string{"drained:node_test"}}},
	}, errs: map[string]error{
		"ses_revoked": contracts.Errorf(contracts.ErrUnauthorized, "synthetic: session token revoked"),
	}}
	ma := mock.NewAdapter()
	all := []string{"ses_ok", "ses_arch", "ses_revoked", "ses_moved", "ses_drained", "ses_gone"}
	n := crashHubNode(t, h, ma, all...)
	h.set(func(h *crashHub) { delete(h.sessions, "ses_gone") }) // the hub has no such session

	ok := n.resumedAs("ses_ok")
	n.resumedAs("ses_drained")
	waitFor(t, "note to ses_ok via the hub", func() bool { return len(h.sentTo("ses_ok")) == 1 })
	note := h.sentTo("ses_ok")[0]
	if note.MessageID != resumenote.MessageID("ses_ok", ok.Run.RunID) || !strings.Contains(note.Body.Text, "Pyre restarted after a crash") {
		t.Fatalf("note = %+v", note)
	}
	n.d.WaitLaunches()
	time.Sleep(100 * time.Millisecond) // the follow-ups of both resumes have run
	if got := h.sentTo("ses_drained"); len(got) != 0 {
		t.Fatalf("drained session got pyred's note: %+v", got)
	}
	for _, s := range []string{"ses_arch", "ses_revoked", "ses_moved", "ses_gone"} {
		if r := n.latestRun(s); r.Run.RunID != oldRunOf(s) || r.Phase != phaseEnded {
			t.Errorf("%s resumed as %s (%s)", s, r.Run.RunID, r.Phase)
		}
	}
	n.d.mu.Lock()
	errs := strings.Join(n.d.errs, "\n")
	n.d.mu.Unlock()
	if !strings.Contains(errs, "refused this node's credential checking session ses_revoked") {
		t.Fatalf("refused node credential not reported: %s", errs)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.getBearers) < len(all) {
		t.Fatalf("hub asked %d times for %d sessions", len(h.getBearers), len(all))
	}
	for _, b := range h.getBearers {
		if b != crashNodeToken {
			t.Fatalf("hub asked with %q, want the node credential", b)
		}
	}
}

// While the hub cannot be reached nothing is resumed blind; a stop the hub
// queued for a dead run arrives meanwhile and cancels that resume; when the
// hub answers (the link comes up) the rest are resumed. A stop that arrives
// after the resume stops the run that replaced the dead one.
func TestCrashResumeWaitsForHubAndHonoursQueuedStops(t *testing.T) {
	shortCrashResume(t)
	retry := crashHubRetry
	crashHubRetry = time.Hour // only the link coming up retries
	t.Cleanup(func() { crashHubRetry = retry })
	h := &crashHub{unreachable: true}
	ma := mock.NewAdapter()
	n := crashHubNode(t, h, ma, "ses_a", "ses_stopq", "ses_late")
	// Registered by recoverRuns, before Start returned (and before any hub
	// link could deliver a queued stop).
	n.d.crash.mu.Lock()
	registered := len(n.d.crash.runs)
	n.d.crash.mu.Unlock()
	if registered != 3 {
		t.Fatalf("%d crash-resume runs registered at Start, want 3", registered)
	}
	time.Sleep(50 * time.Millisecond)
	for _, s := range []string{"ses_a", "ses_stopq", "ses_late"} {
		if r := n.latestRun(s); r.Run.RunID != oldRunOf(s) {
			t.Fatalf("%s resumed while the hub was unreachable", s)
		}
	}
	// The owner stopped ses_stopq while the node was down.
	stop := func(s string) contracts.CommandResult {
		return n.do(command(contracts.CmdStop, contracts.RunCommand{SessionID: s, RunID: oldRunOf(s)}))
	}
	if res := stop("ses_stopq"); res.Error != nil || res.Outcome != contracts.OutcomeSucceeded {
		t.Fatalf("queued stop = %+v", res)
	}
	h.set(func(h *crashHub) { h.unreachable = false })
	n.d.SetHubLink(true)
	n.resumedAs("ses_a")
	late := n.resumedAs("ses_late")
	n.d.WaitLaunches()
	if r := n.latestRun("ses_stopq"); r.Run.RunID != oldRunOf("ses_stopq") {
		t.Fatalf("stopped session resumed as %s", r.Run.RunID)
	}
	// The stop for ses_late arrives after its resume: the new run stops.
	if res := stop("ses_late"); res.Error != nil {
		t.Fatalf("late stop = %+v", res)
	}
	waitFor(t, "resumed run stopped", func() bool {
		n.d.PollNow(context.Background(), late.Run.RunID)
		return n.run(late.Run.RunID).Phase == phaseEnded
	})
	if r := n.run(late.Run.RunID); r.Run.AdapterData[adapterDataStopRequested] == "" {
		t.Fatalf("stopped run has no stop marker: %+v", r.Run.AdapterData)
	}
}

// A hub that stays unreachable past crashHubWait: the sessions stay ended.
func TestCrashResumeHubNeverAnswers(t *testing.T) {
	shortCrashResume(t)
	wait, retry := crashHubWait, crashHubRetry
	crashHubWait, crashHubRetry = 150*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { crashHubWait, crashHubRetry = wait, retry })
	h := &crashHub{unreachable: true}
	n := crashHubNode(t, h, mock.NewAdapter(), "ses_a")
	time.Sleep(300 * time.Millisecond)
	h.set(func(h *crashHub) { h.unreachable = false }) // too late
	n.d.SetHubLink(true)
	time.Sleep(50 * time.Millisecond)
	n.d.WaitLaunches()
	if r := n.latestRun("ses_a"); r.Run.RunID != oldRunOf("ses_a") || r.Phase != phaseEnded {
		t.Fatalf("resumed after the hub wait: %+v", r.Run)
	}
	h.mu.Lock()
	asked := len(h.getBearers)
	h.mu.Unlock()
	if asked < 3 {
		t.Fatalf("hub asked %d times; want retries", asked)
	}
}

// stopFailAdapter refuses stops.
type stopFailAdapter struct{ *mock.Adapter }

func (s stopFailAdapter) Stop(context.Context, contracts.RunHandle) (contracts.CommandReceipt, error) {
	return contracts.CommandReceipt{}, contracts.Errorf(contracts.ErrUnavailable, "synthetic: stop refused")
}

// A stop that fails leaves no stop marker: a later crash still resumes it.
func TestFailedStopClearsStopMarker(t *testing.T) {
	shortCrashResume(t)
	ad := stopFailAdapter{mock.NewAdapter()}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{ad}})
	n.start(startCmd("ses_a", "run_a1", contracts.HarnessMock, t.TempDir()))
	res := n.do(command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_a", RunID: "run_a1"}))
	if res.Error == nil {
		t.Fatalf("stop = %+v, want refused", res)
	}
	rec, _, err := n.d.st.run(context.Background(), "run_a1")
	if err != nil || rec.Run.AdapterData[adapterDataStopRequested] != "" {
		t.Fatalf("stop marker after a failed stop: %v %+v", err, rec.Run.AdapterData)
	}
	if lr, _ := n.d.getLive("run_a1"); lr.rec.Run.AdapterData[adapterDataStopRequested] != "" {
		t.Fatal("stop marker kept in memory")
	}
	n.stop()
	ad.Vanish("run_a1")
	n = n.restart()
	waitFor(t, "crash resume", func() bool { return n.latestRun("ses_a").Run.PreviousRunID == "run_a1" })
}

// gatedAdapter holds launches until open is closed.
type gatedAdapter struct {
	*mock.Adapter
	open chan struct{}
}

func (g gatedAdapter) Launch(ctx context.Context, l contracts.ResolvedLaunch) (contracts.RunHandle, error) {
	<-g.open
	return g.Adapter.Launch(ctx, l)
}

// A stop that arrives while the resumed run is still launching is kept
// until the run is up, even past the note's wait: the run is then stopped.
func TestCrashResumeStopBeforeResumedRunIsUp(t *testing.T) {
	shortCrashResume(t)
	crashResumeLiveWait = 20 * time.Millisecond // the note gives up long before the launch finishes
	ma := mock.NewAdapter()
	open := make(chan struct{})
	close(open)
	ad := gatedAdapter{Adapter: ma, open: open}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{ad}})
	n.start(startCmd("ses_a", "run_a1", contracts.HarnessMock, t.TempDir()))
	n.stop()
	ma.Vanish("run_a1")
	gate := make(chan struct{})
	n.cfg.Adapters = []contracts.RuntimeAdapter{gatedAdapter{Adapter: ma, open: gate}}
	n = n.restart()
	var newRun string
	waitFor(t, "resume launching", func() bool {
		r := n.latestRun("ses_a")
		newRun = r.Run.RunID
		return r.Run.PreviousRunID == "run_a1"
	})
	res := n.do(command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_a", RunID: "run_a1"}))
	var rcpt contracts.CommandReceipt
	json.Unmarshal(res.Result, &rcpt)
	if res.Error != nil || !strings.Contains(rcpt.Detail, "as soon as it is up") {
		t.Fatalf("stop while launching = %+v / %+v", res, rcpt)
	}
	time.Sleep(100 * time.Millisecond) // past crashResumeLiveWait
	close(gate)
	waitFor(t, "resumed run stopped once up", func() bool {
		n.d.PollNow(context.Background(), newRun)
		return n.run(newRun).Phase == phaseEnded
	})
	if r := n.run(newRun); r.Run.AdapterData[adapterDataStopRequested] == "" {
		t.Fatalf("resumed run ended without the stop: %+v", r.Run)
	}
}
