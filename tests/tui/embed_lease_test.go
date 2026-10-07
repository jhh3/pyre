package tui_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// otherClient makes the run's input lease held by another client (by
// default the owner's TUI on another machine) until a takeover.
type otherClient struct {
	contracts.API
	mu     sync.Mutex
	held   bool
	holder contracts.Principal
}

func (o *otherClient) hold(v bool) {
	o.mu.Lock()
	o.held = v
	o.mu.Unlock()
}

func (o *otherClient) AcquireLease(ctx context.Context, req contracts.LeaseRequest) (contracts.InputLease, error) {
	o.mu.Lock()
	held := o.held
	if req.Takeover {
		o.held = false
	}
	o.mu.Unlock()
	if held && !req.Takeover {
		l := contracts.InputLease{LeaseID: "lease_synth-other", RunID: req.RunID, Holder: o.holder, ExpiresAt: time.Now().Add(2 * time.Minute)}
		return contracts.InputLease{}, contracts.Errorf(contracts.ErrConflict, "run input is leased by %s", o.holder.Name).
			WithRef(contracts.KindLease, l.LeaseID).WithDetail("lease", l)
	}
	return o.API.AcquireLease(ctx, req)
}

var synthOtherTUI = contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synth-other", Name: "synth-server"}

func startOther(t *testing.T, holder contracts.Principal, mod func(*tui.Options)) (*tuitest.Harness, *tuitest.Recorder, *otherClient) {
	t.Helper()
	var rec *tuitest.Recorder
	var oc *otherClient
	h, _ := start(t, 160, 40, func(a contracts.API) contracts.API {
		oc = &otherClient{API: a, holder: holder}
		rec = tuitest.NewRecorder(tuitest.NewEmbeddable(oc))
		return rec
	}, mod)
	return h, rec, oc
}

func takeovers(rec *tuitest.Recorder) (plain, takeover int) {
	for _, c := range rec.Calls("AcquireLease") {
		if c.Request.(contracts.LeaseRequest).Takeover {
			takeover++
		} else {
			plain++
		}
	}
	return
}

// The owner's own other client holds the lease: focusing says where you are
// typing and offers a takeover; enter takes it over (contract takeover) and
// the pane types.
func TestEmbeddedTerminalTakesOverOwnClientsLease(t *testing.T) {
	h, rec, oc := startOther(t, synthOtherTUI, nil)
	oc.hold(true)
	selectSession(t, h, "ses_synth-a")
	waitTerm(t, h, "ses_synth-a", true)

	h.Key("enter")
	h.WaitFor("takeover offer", 3*sec, func() bool {
		return h.Contains("You're typing in this session on synth-server — take over here?") && h.Contains("typing on synth-server · enter to take over")
	})
	if h.M.TermFocused() {
		t.Fatal("focus must not be taken while another client holds the lease")
	}
	if _, n := takeovers(rec); n != 0 {
		t.Fatal("took over without asking")
	}
	h.Key("enter")
	inter := waitTerm(t, h, "ses_synth-a", false)
	h.WaitFor("focused after takeover", 3*sec, func() bool { _, n := takeovers(rec); return h.M.TermFocused() && n == 1 })
	h.Type("hi")
	h.WaitFor("typed", 3*sec, func() bool { return inter.Input() == "hi" })
	if h.Contains("take over here?") {
		t.Fatalf("offer still shown after takeover:\n%s", h.View())
	}
}

// A lease held by anyone else (not one of the owner's clients) is never
// taken silently: the reason is shown inline and enter does not take over.
func TestEmbeddedTerminalExplainsForeignLease(t *testing.T) {
	agent := contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_synth-peer", Name: "synth-peer"}
	h, rec, oc := startOther(t, agent, nil)
	oc.hold(true)
	selectSession(t, h, "ses_synth-a")
	waitTerm(t, h, "ses_synth-a", true)
	h.Key("enter")
	h.WaitFor("inline reason", 3*sec, func() bool { return h.Contains("input leased by synth-peer") })
	h.Key("enter")
	h.Settle()
	if _, n := takeovers(rec); n != 0 || h.M.TermFocused() {
		t.Fatalf("a foreign lease was taken over (takeovers=%d focused=%v)", n, h.M.TermFocused())
	}
}

// An idle focused pane gives its lease up after LeaseIdle (staying focused)
// and takes it back on the next key, which then reaches the session.
func TestEmbeddedTerminalIdleReleaseAndRetake(t *testing.T) {
	h, rec, _ := startOther(t, synthOtherTUI, func(o *tui.Options) { o.LeaseIdle = 300 * time.Millisecond })
	selectSession(t, h, "ses_synth-a")
	waitTerm(t, h, "ses_synth-a", true)
	h.Key("enter")
	inter := waitTerm(t, h, "ses_synth-a", false)
	h.WaitFor("lease", 3*sec, func() bool { return h.M.TermFocused() && len(rec.Calls("AcquireLease")) == 1 })
	h.WaitFor("idle release", 3*sec, func() bool { return len(rec.Calls("ReleaseLease")) == 1 })
	if !h.M.TermFocused() || inter.Closed() {
		t.Fatal("the pane must stay focused (and writable) after an idle release")
	}
	acq := len(rec.Calls("AcquireLease"))
	h.Type("x")
	h.WaitFor("re-take and key", 3*sec, func() bool { return len(rec.Calls("AcquireLease")) > acq && inter.Input() == "x" })
}

// The other client taking the lease over drops this pane to watching, with
// the reason inline, never silently.
func TestEmbeddedTerminalLeaseTakenOverDropsToWatching(t *testing.T) {
	h, rec, oc := startOther(t, synthOtherTUI, func(o *tui.Options) { o.LeaseIdle = 2 * time.Second })
	selectSession(t, h, "ses_synth-a")
	waitTerm(t, h, "ses_synth-a", true)
	h.Key("enter")
	inter := waitTerm(t, h, "ses_synth-a", false)
	h.WaitFor("lease", 3*sec, func() bool { return h.M.TermFocused() && len(rec.Calls("AcquireLease")) == 1 })
	oc.hold(true) // synth-server takes it over; our next renewal is refused
	h.WaitFor("dropped to watching", 5*sec, func() bool {
		return !h.M.TermFocused() && inter.Closed() && h.Contains("synth-server took over")
	})
	waitTerm(t, h, "ses_synth-a", true)
}

// startingAttach refuses attaches while the run is starting, like a node.
type startingAttach struct {
	contracts.API
	mu       sync.Mutex
	starting bool
	sid      string // count only this session's attempts ("" = all)
	refused  int
}

func (s *startingAttach) set(v bool) {
	s.mu.Lock()
	s.starting = v
	s.mu.Unlock()
}

func (s *startingAttach) AttachSession(ctx context.Context, req contracts.AttachRequest) (contracts.AttachDescriptor, error) {
	s.mu.Lock()
	st := s.starting
	if st && (s.sid == "" || req.SessionID == s.sid) {
		s.refused++
	}
	s.mu.Unlock()
	if st {
		return contracts.AttachDescriptor{}, contracts.Errorf(contracts.ErrConflict, "run is starting (not live)").WithRef(contracts.KindRun, "run_synth")
	}
	return s.API.AttachSession(ctx, req)
}

// A run that is still starting shows a calm "starting…" (never "could not
// embed") and attaches by itself once it is live.
func TestEmbeddedTerminalRetriesWhileStarting(t *testing.T) {
	var sa *startingAttach
	h, _ := start(t, 160, 40, func(a contracts.API) contracts.API {
		// Only the watched session's attempts count: the sessions passed on
		// the way to it (slow under -race) each make their own first attempts.
		sa = &startingAttach{API: tuitest.NewEmbeddable(a), starting: true, sid: "ses_synth-a"}
		return tuitest.NewRecorder(sa)
	}, func(o *tui.Options) { o.StartingRetry = 20 * time.Millisecond })
	selectSession(t, h, "ses_synth-a")
	h.WaitFor("starting shown", 3*sec, func() bool { return h.Contains("starting…") })
	h.WaitFor("retried", 3*sec, func() bool { sa.mu.Lock(); defer sa.mu.Unlock(); return sa.refused >= 3 })
	time.Sleep(300 * time.Millisecond)
	sa.mu.Lock()
	n := sa.refused
	sa.mu.Unlock()
	if n > 30 {
		t.Fatalf("%d attach attempts while starting: no backoff", n)
	}
	if v := h.View(); strings.Contains(v, "could not embed") {
		t.Fatalf("starting shown as an error:\n%s", v)
	}
	sa.set(false)
	waitTerm(t, h, "ses_synth-a", true)
	h.WaitFor("live", 3*sec, func() bool { return !h.Contains("starting…") })
}
