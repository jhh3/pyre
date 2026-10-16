package tui_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// Lead signals (owner request 2026-10-14: "I should know when a project
// lead is worth clicking on"). Each lead (top-level session) carries one
// chip for itself and its descendants — asks N, waiting, new, working — or
// reads dim; the project header sums its leads; the Sessions tab counts
// the leads that ask or wait on you.

const outfitProject = "prj_synth-outfit"

// leadsAPI is the mock (asks included) with every session but "long
// refactor" in Synthetic Outfit and this lineage:
//
//	coordinator (working) ─ migration work                → asks 2 (its own asks)
//	pairing session (idle) ─ worker (needs you), deploy prep (approval) → waiting
//	api work (handoff successor): idle, finished after launch            → new
//	scratch (idle, flag cleared) ─ outfit API client (working)          → working
//	long refactor (stale), alone in No project                           → quiet (dim)
type leadsAPI struct {
	*mock.Backend
	mu       sync.Mutex
	finished time.Time       // when the handoff successor's turn ended
	selfTurn bool            // that turn was the lead's own (the owner sent nothing)
	waiting  map[string]bool // sessions flagged "waiting on you" (AttnWaiting)
}

var leadsParents = map[string]string{
	"ses_synth-fb":      "ses_synth-a",          // migration work ← coordinator
	"ses_synth-b":       "ses_synth-leased",     // worker ← pairing session
	"ses_synth-apr":     "ses_synth-leased",     // deploy prep ← pairing session
	"ses_synth-project": "ses_synth-standalone", // outfit API client ← scratch
}

const (
	handoffSess = "ses_synth-fbx2"       // api work (handoff successor)
	pairingSess = "ses_synth-leased"     // pairing session
	scratchSess = "ses_synth-standalone" // scratch (standalone)
)

func (a *leadsAPI) mod(s *contracts.SessionView) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := s.Session.SessionID
	if p, ok := leadsParents[id]; ok {
		s.Session.Lineage.ParentSessionID = p
	}
	if id != "ses_synth-offline" {
		p := outfitProject
		s.Session.ProjectID = &p
	}
	switch id {
	case handoffSess:
		if s.Run != nil {
			r := *s.Run
			r.State, r.StateReason = contracts.RunWaitingInput, ""
			r.Freshness.ObservedAt = a.finished
			s.Run = &r
		}
		// The owner sent it something just before that turn ended.
		if !a.selfTurn {
			oa := a.finished.Add(-30 * time.Second)
			s.Session.OwnerActiveAt = &oa
		}
	case scratchSess:
		s.Flag, s.Attention = nil, 0
	}
	if a.waiting[id] {
		s.Flag = &contracts.SessionFlag{Level: contracts.FlagNeedsYou, Reason: "waiting", Kind: string(contracts.AttnWaiting),
			Text: "Synthetic: which rollout plan?", Count: 1, Since: mock.FixtureEpoch}
		s.Attention = 1
	}
}

func (a *leadsAPI) ListSessions(ctx context.Context, r contracts.ListSessionsRequest) (contracts.Page[contracts.SessionView], error) {
	p, err := a.Backend.ListSessions(ctx, r)
	for i := range p.Items {
		a.mod(&p.Items[i])
	}
	return p, err
}

func (a *leadsAPI) GetSession(ctx context.Context, r contracts.GetRequest) (contracts.SessionView, error) {
	s, err := a.Backend.GetSession(ctx, r)
	a.mod(&s)
	return s, err
}

func (a *leadsAPI) GetManySessions(ctx context.Context, r contracts.GetManySessionsRequest) (contracts.GetManySessionsResponse, error) {
	resp, err := a.Backend.GetManySessions(ctx, r)
	for i := range resp.Items {
		a.mod(&resp.Items[i])
	}
	return resp, err
}

// dwell delivers the selection's "viewed for 2s" timer now: the harness
// default dwell is an hour, so no wall-clock time marks anything seen.
func dwell(h *tuitest.Harness) {
	h.Send(h.M.SeenDwellDue())
	h.Settle()
}

// startLeads runs the TUI over leadsAPI on the harness clock; the handoff
// successor finished a minute after launch (after the seen baseline).
func startLeads(t *testing.T, w, ht int) (*tuitest.Harness, *leadsAPI, *mock.FakeClock) {
	t.Helper()
	b, clk := tuitest.NewMock(t)
	launch := clk.Now()
	api := &leadsAPI{Backend: b, finished: launch.Add(time.Minute), waiting: map[string]bool{}}
	ask(t, b, coordinator, "rollout-plan", "Pick the rollout plan for the synthetic service",
		"Synthetic: canary first is slower but safer.", "a", false, 45*time.Minute, "Canary first", "Big bang")
	ask(t, b, coordinator, "db-migration", "Run the synthetic schema migration now?",
		"Synthetic: the migration locks one table for a minute.", "b", true, 10*time.Minute, "Run now", "Wait")
	opts := tuitest.Options()
	opts.Clock = clk
	opts.Watch = tui.PollWatcher(b)
	h := tuitest.New(t, api, opts, w, ht)
	h.WaitFor("initial data", 5*sec, func() bool {
		return h.M.Loaded("sessions") && h.M.Loaded("nodes") && h.M.Loaded("attention") && h.M.Loaded("asks")
	})
	h.WaitFor("terminal pane", 5*sec, func() bool { return h.Contains("read-only snapshot") }) // not "connecting…"
	h.Settle()
	return h, api, clk
}

func leadIs(t *testing.T, h *tuitest.Harness, id, want string, wantAsks int) {
	t.Helper()
	kind, asks, ok := h.M.LeadSignal(id)
	if !ok || kind != want || asks != wantAsks {
		t.Fatalf("lead %s: signal %q asks %d (lead %v), want %q asks %d:\n%s", id, kind, asks, ok, want, wantAsks, h.View())
	}
}

func TestLeadSignals(t *testing.T) {
	h, api, clk := startLeads(t, 160, 30)
	v := h.View()
	golden(t, "sessions-lead-signals-160", v)

	leadIs(t, h, coordinator, "asks", 2)
	leadIs(t, h, pairingSess, "waiting", 0)
	leadIs(t, h, handoffSess, "new", 0)
	leadIs(t, h, scratchSess, "working", 0)
	leadIs(t, h, "ses_synth-offline", "", 0)
	if _, _, ok := h.M.LeadSignal("ses_synth-b"); ok {
		t.Fatalf("a child is not a lead")
	}

	// Chips at the right of the lead rows; children unchanged.
	for title, chip := range map[string]string{
		"coordinator":       "● asks 2",
		"pairing session":   "◆ waiting",
		"api work (handoff": "• new",
		"scratch (standalo": "◌ working",
	} {
		if l := rowLine(v, title); !strings.Contains(l, chip) {
			t.Fatalf("%s lacks %q: %q", title, chip, l)
		}
	}
	for _, child := range []string{"migration work", "worker", "deploy prep", "outfit API client"} {
		if l := rowLine(v, child); strings.ContainsAny(l, "◆•◌") || strings.Contains(l, "asks") {
			t.Fatalf("child %s carries a chip: %q", child, l)
		}
	}
	// The quiet lead reads dim; a signalling lead does not.
	if raw := rawLine(h, "long refactor"); !strings.Contains(raw, sgrFaint+"long refactor") {
		t.Fatalf("the quiet lead is not dim: %q", raw)
	}
	if raw := rawLine(h, "pairing session"); strings.Contains(raw, sgrFaint+"pairing session") {
		t.Fatalf("a waiting lead is dim: %q", raw)
	}

	// The project header sums its leads, worst first.
	if l := lineOf(v, "Synthetic Outfit"); !strings.Contains(l, "Synthetic Outfit 8 · ● 2 asks · ◆ 1 waiting · • 1 new · ◌ 1 working") {
		t.Fatalf("project header: %q", l)
	}
	// "long refactor" alone in No project, stale: quiet.
	if l := lineOf(v, "No project"); strings.Contains(l, "·") {
		t.Fatalf("a quiet project carries a summary: %q", l)
	}

	// The Sessions tab counts the leads that ask or wait (coordinator,
	// pairing session), not every session that needs you (worker, deploy
	// prep, coordinator) and not the new one.
	if n := headerCount(t, h, "Sessions"); n != 2 {
		t.Fatalf("Sessions badge %d, want 2 leads", n)
	}

	// The lead's facts name the soonest deadline and the web inbox key.
	selectSession(t, h, coordinator)
	h.Key("i")
	h.Settle()
	if l := lineOf(h.View(), "asks you 2"); !strings.Contains(l, "● asks you 2 · I web inbox · soonest: Run the synthetic schema migration now?") {
		t.Fatalf("facts lack the asks line:\n%s", h.View())
	}
	h.Key("esc")
	h.Settle()

	// New until viewed for the dwell: a pass over the row does not count.
	clk.Set(clk.Now().Add(2 * time.Minute))
	selectSession(t, h, handoffSess)
	h.Key("down")
	h.Settle()
	dwell(h) // the next row's dwell, not this one's
	leadIs(t, h, handoffSess, "new", 0)
	selectSession(t, h, handoffSess)
	dwell(h)
	leadIs(t, h, handoffSess, "", 0)
	selectSession(t, h, scratchSess)
	h.Settle()
	leadIs(t, h, handoffSess, "", 0)
	if raw := rawLine(h, "api work (handoff"); !strings.Contains(raw, sgrFaint+"api work") || strings.Contains(h.View(), "• new") {
		t.Fatalf("a seen lead reads quiet: %q", raw)
	}
	if l := lineOf(h.View(), "Synthetic Outfit"); strings.Contains(l, "new") {
		t.Fatalf("project header still counts the seen lead: %q", l)
	}

	// A later turn is new again.
	api.mu.Lock()
	api.finished = clk.Now().Add(time.Minute)
	api.mu.Unlock()
	clk.Set(clk.Now().Add(2 * time.Minute))
	h.Key("ctrl+r")
	h.WaitFor("new again", 5*sec, func() bool { k, _, _ := h.M.LeadSignal(handoffSess); return k == "new" })

	// A turn the lead takes on its own (a child's report, Pyre mail) after
	// you looked is not new.
	selectSession(t, h, handoffSess)
	dwell(h)
	selectSession(t, h, scratchSess)
	h.Settle()
	api.mu.Lock()
	api.finished, api.selfTurn = clk.Now().Add(time.Minute), true
	api.mu.Unlock()
	clk.Set(clk.Now().Add(2 * time.Minute))
	h.Key("ctrl+r")
	h.Settle()
	leadIs(t, h, handoffSess, "", 0)
	api.mu.Lock()
	api.selfTurn = false
	api.mu.Unlock()

	// A lead flagged "waiting on you" (AttnWaiting) waits; its own status
	// says so, so its row has no chip; the strip names it; the tab counts it.
	api.mu.Lock()
	api.waiting[scratchSess] = true
	api.mu.Unlock()
	h.Key("ctrl+r")
	h.WaitFor("waiting", 5*sec, func() bool { k, _, _ := h.M.LeadSignal(scratchSess); return k == "waiting" })
	h.Settle()
	if l := rowLine(h.View(), "scratch (standalone)"); !strings.Contains(l, "! needs you") || strings.Contains(l, "◆") {
		t.Fatalf("waiting lead row: %q", l)
	}
	if s := stripLine(h); !strings.Contains(s, "◆ scratch") {
		t.Fatalf("strip lacks the waiting lead: %q", s)
	}
	if n := headerCount(t, h, "Sessions"); n != 3 {
		t.Fatalf("Sessions badge %d, want 3 leads", n)
	}
}

// Viewed times persist in the state file: a seen lead stays seen after a
// restart; a first launch (no state file) treats nothing earlier as new.
func TestLeadSeenPersists(t *testing.T) {
	state := t.TempDir() + "/tui.json"
	run := func(finished time.Duration) (*tuitest.Harness, *mock.FakeClock) {
		b, clk := tuitest.NewMock(t)
		clk.Set(mock.FixtureEpoch.Add(10 * time.Minute))
		api := &leadsAPI{Backend: b, finished: mock.FixtureEpoch.Add(finished), waiting: map[string]bool{}}
		opts := tuitest.Options()
		opts.Clock = clk
		opts.StateFile = state
		h := tuitest.New(t, api, opts, 160, 30)
		h.WaitFor("sessions", 5*sec, func() bool { return h.M.Loaded("sessions") && h.M.Loaded("nodes") })
		h.Settle()
		return h, clk
	}
	// First launch: the turn ended before it, so it is not new.
	h, _ := run(5 * time.Minute)
	leadIs(t, h, handoffSess, "", 0)
	h.Close()
	// Second launch: a turn that ended after the baseline is new; view it.
	h, clk := run(15 * time.Minute)
	leadIs(t, h, handoffSess, "new", 0)
	clk.Set(mock.FixtureEpoch.Add(20 * time.Minute))
	selectSession(t, h, handoffSess)
	dwell(h)
	leadIs(t, h, handoffSess, "", 0)
	selectSession(t, h, scratchSess)
	h.Settle()
	h.WaitFor("state file", 5*sec, func() bool { // written in the background
		b, _ := os.ReadFile(state)
		return strings.Contains(string(b), `"`+handoffSess+`"`)
	})
	h.Close()
	// Third launch: still seen.
	h, _ = run(15 * time.Minute)
	leadIs(t, h, handoffSess, "", 0)
}
