package codex

import (
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// A local agent that finishes after the root's turn ended starts exactly
// one turn naming it (iss_…).
func TestCodexAgentWakeAfterTurnEnds(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	h := e.launch(e.a, "run_aw1", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	if r := dlv(t, e.a, h, "msg_aw1", "spawn FAKE_AGENT=400:boundary_audit", contracts.ModeQueue); r.State != contracts.DeliveryAccepted {
		t.Fatalf("%+v", r)
	}
	e.wait(e.a, h, 5*time.Second, idle)
	if n, _ := wakeEntries(t, e.a, h); len(n) != 0 {
		t.Fatalf("woken before the agent finished: %+v", n)
	}
	got := waitNotices(t, e.a, h, 1, 5*time.Second)
	if want := "[pyre] local agent /root/boundary_audit finished; its report is queued for you and arrives in this turn."; !strings.HasPrefix(got[0].Text, want) {
		t.Fatalf("notice %q, want prefix %q", got[0].Text, want)
	}
	e.wait(e.a, h, 5*time.Second, idle)
	time.Sleep(600 * time.Millisecond)
	if n, _ := wakeEntries(t, e.a, h); len(n) != 1 {
		t.Fatalf("want exactly one wake turn, got %d", len(n))
	}
	rec.waitWakes(t, 1)
	if d := rec.String(); !strings.Contains(d, "agent /root/boundary_audit") {
		t.Fatalf("audit detail lacks the agent: %s", d)
	}
}

// Agents finishing close together, and a command finishing with them, share
// one notice.
func TestCodexAgentWakeCoalesces(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	e.a.wakeSettle = 300 * time.Millisecond
	h := e.launch(e.a, "run_aw2", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_aw2", "spawn FAKE_AGENT=300:a FAKE_AGENT=380:b FAKE_BG_MS=420:c", contracts.ModeQueue)
	got := waitNotices(t, e.a, h, 1, 5*time.Second)
	e.wait(e.a, h, 5*time.Second, idle)
	time.Sleep(700 * time.Millisecond)
	if n, _ := wakeEntries(t, e.a, h); len(n) != 1 {
		t.Fatalf("want one coalesced notice, got %d: %+v", len(n), n)
	}
	for _, want := range []string{"[pyre] background command finished: `bg-c`", "[pyre] 2 local agents finished (/root/a, /root/b)"} {
		if !strings.Contains(got[0].Text, want) {
			t.Errorf("notice lacks %q:\n%s", want, got[0].Text)
		}
	}
	rec.waitWakes(t, 1)
}

// Mail delivered before the wake settles starts a turn that gets the
// agent's report: no second turn for the agent.
func TestCodexAgentWakeNotAfterMailTurn(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	e.a.wakeSettle = 1500 * time.Millisecond
	h := e.launch(e.a, "run_aw3", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_aw3a", "spawn FAKE_AGENT=200:a", contracts.ModeQueue)
	e.wait(e.a, h, 5*time.Second, idle)
	time.Sleep(400 * time.Millisecond) // the agent finished; the wake waits to settle
	if r := dlv(t, e.a, h, "msg_aw3b", "a message", contracts.ModeQueue); r.State != contracts.DeliveryAccepted {
		t.Fatalf("%+v", r)
	}
	e.wait(e.a, h, 5*time.Second, idle)
	time.Sleep(2 * time.Second)
	if n, _ := wakeEntries(t, e.a, h); len(n) != 0 {
		t.Fatalf("woken although the mail turn got the report: %+v", n)
	}
	if rec.wakes() != 0 {
		t.Fatalf("wake reported: %s", rec)
	}
}

// An agent finishing inside a root turn that got its report wakes nothing;
// one whose report missed the turn wakes the root once it ends.
func TestCodexAgentWakeInTurn(t *testing.T) {
	t.Parallel()
	e, _ := newWakeEnv(t, nil)
	h := e.launch(e.a, "run_aw4", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_aw4a", "spawn FAKE_AGENT=200:seen FAKE_SLEEP_MS=800", contracts.ModeQueue)
	e.wait(e.a, h, 5*time.Second, idle)
	time.Sleep(800 * time.Millisecond)
	if n, _ := wakeEntries(t, e.a, h); len(n) != 0 {
		t.Fatalf("woken for a report the turn got: %+v", n)
	}
	dlv(t, e.a, h, "msg_aw4b", "spawn FAKE_AGENTU=200:missed FAKE_SLEEP_MS=800", contracts.ModeQueue)
	got := waitNotices(t, e.a, h, 1, 5*time.Second)
	if !strings.Contains(got[0].Text, "local agent /root/missed finished") || strings.Contains(got[0].Text, "/root/seen") {
		t.Fatalf("%s", got[0].Text)
	}
	e.wait(e.a, h, 5*time.Second, idle)
	time.Sleep(600 * time.Millisecond)
	if n, _ := wakeEntries(t, e.a, h); len(n) != 1 {
		t.Fatalf("want exactly one wake turn, got %d", len(n))
	}
}

// A stopped run is never woken for its agents.
func TestCodexAgentWakeNotAfterStop(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	h := e.launch(e.a, "run_aw5", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_aw5", "spawn FAKE_AGENT=500:a", contracts.ModeQueue)
	e.wait(e.a, h, 5*time.Second, idle)
	if _, err := e.a.Stop(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	if rec.wakes() != 0 {
		t.Fatalf("stopped run woken: %s", rec)
	}
}

func TestAgentNotice(t *testing.T) {
	t.Parallel()
	one := agentNotice([]agentDone{{AgentPath: "/root/x"}})
	if !strings.HasPrefix(one, "[pyre] local agent /root/x finished; its report is queued") {
		t.Fatal(one)
	}
	if got := agentNotice([]agentDone{{AgentThreadID: "thr-1"}, {AgentPath: "/root/y"}}); !strings.Contains(got, "2 local agents finished (thread thr-1, /root/y)") {
		t.Fatal(got)
	}
}
