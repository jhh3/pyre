package tui_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

const sec = time.Second

func ctx() context.Context { return context.Background() }

func runOf(t *testing.T, api contracts.API, sid string) string {
	t.Helper()
	sv, err := api.GetSession(ctx(), contracts.GetRequest{ID: sid})
	if err != nil || sv.Run == nil {
		t.Fatalf("session %s: %v", sid, err)
	}
	return sv.Run.RunID
}

// selectSession moves the cursor to the row titled title.
func selectSession(t *testing.T, h *tuitest.Harness, sid string) {
	t.Helper()
	for i := 0; i < 30 && h.M.SelectedSession() != sid; i++ {
		h.Key("down")
	}
	if h.M.SelectedSession() != sid {
		for i := 0; i < 30 && h.M.SelectedSession() != sid; i++ {
			h.Key("up")
		}
	}
	if h.M.SelectedSession() != sid {
		t.Fatalf("could not select %s; view:\n%s", sid, h.View())
	}
}

func TestStatusesUseSymbolAndText(t *testing.T) {
	h, _ := start(t, 160, 32, nil, nil)
	h.Key(".") // show ended sessions too
	h.Settle()
	v := h.View()
	for _, want := range []string{"● working", "○ idle", "! approval", "◌ stale", "– exited", "◔ starting"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing status %q in view:\n%s", want, v)
		}
	}
}

func TestEveryScreenFitsAtAllWidths(t *testing.T) {
	tours := [][]string{
		nil, {"2"}, {"2", "enter"}, {"2", "enter", "right"}, {"2", "enter", "right", "right"},
		{"2", "enter", "right", "right", "enter"}, {"2", "enter", "left"}, {"2", "enter", "left", "left"},
		{":"}, {"?"}, {"m"}, {"n"}, {"n", "enter"}, {"n", "enter", "enter"},
		{":", "M", "a", "c", "h", "enter"}, {":", "A", "c", "c", "o", "u", "n", "t", "s", "enter"},
		{":", "C", "o", "n", "n", "enter"}, {":", "E", "n", "v", "enter"}, {"M"},
	}
	for _, w := range []int{60, 100, 160} {
		for i, tour := range tours {
			t.Run(fmt.Sprintf("w%d-tour%d", w, i), func(t *testing.T) {
				t.Parallel() // independent harnesses; the time is all Settle waits
				h, _ := start(t, w, 28, nil, nil)
				h.Key(tour...)
				h.Settle()
				lines := strings.Split(h.View(), "\n")
				if len(lines) != 28 {
					t.Errorf("w=%d tour=%v: %d lines, want 28", w, tour, len(lines))
				}
				for i, l := range lines {
					if n := tuitestWidth(l); n > w {
						t.Errorf("w=%d tour=%v line %d is %d wide: %q", w, tour, i, n, l)
					}
				}
				h.Close()
			})
		}
	}
}

func TestFilterAndSearch(t *testing.T) {
	h, _ := start(t, 100, 30, nil, nil)
	h.Key("/")
	h.Type("linux")
	h.Key("enter")
	v := h.View()
	if !strings.Contains(v, "worker") || strings.Contains(v, "deploy prep") {
		t.Fatalf("filter by machine failed:\n%s", v)
	}
	if h.M.Filter() != "linux" {
		t.Fatalf("filter = %q", h.M.Filter())
	}
	// Projects share the filter; esc clears it.
	h.Key("esc")
	if h.M.Filter() != "" || !h.Contains("deploy prep") {
		t.Fatalf("esc did not clear filter:\n%s", h.View())
	}
	// Search a status word.
	h.Key("/")
	h.Type("approval")
	h.Key("enter")
	if !h.Contains("deploy prep") || h.Contains("coordinator") {
		t.Fatalf("status search failed:\n%s", h.View())
	}
}

func TestSelectionStableUnderEventBurst(t *testing.T) {
	var b *mock.Backend
	h, api := start(t, 100, 30, func(a contracts.API) contracts.API { b = a.(*mock.Backend); return a }, func(o *tui.Options) {
		o.Debounce = 60 * time.Millisecond
	})
	_ = h
	// Restart with a watcher (start() built the harness before b was known).
	h.Close()
	opts := tuitest.Options()
	opts.Clock = b.Clock()
	opts.Watch = tui.PollWatcher(b)
	opts.Debounce = 60 * time.Millisecond
	h = tuitest.New(t, api, opts, 100, 30)
	h.WaitFor("sessions", 5*sec, func() bool { return h.M.Loaded("sessions") })
	h.Settle()

	target := "ses_synth-b" // "worker", near the bottom
	selectSession(t, h, target)
	h.Settle()

	runs := []string{runOf(t, api, "ses_synth-a"), runOf(t, api, "ses_synth-fb"), runOf(t, api, "ses_synth-standalone")}
	states := []contracts.RunState{contracts.RunRunning, contracts.RunWaitingInput}
	const burst = 300
	for i := 0; i < burst; i++ {
		if err := b.SetRunState(runs[i%len(runs)], states[i%2], fmt.Sprintf("burst %d", i)); err != nil {
			t.Fatal(err)
		}
		if i%50 == 0 {
			h.Key("k", "j") // keep navigating during the burst
		}
	}
	// A new session sorting before the target shifts its index.
	if _, err := api.StartSession(ctx(), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_burst-new"},
		NodeID: "node_synth-mac", Harness: contracts.HarnessClaude, Title: "aaa first", Cwd: "/Users/synth/scratch",
		AccountPolicy: contracts.AccountPolicy{Mode: contracts.AccountEligible}}); err != nil {
		t.Fatal(err)
	}
	h.WaitFor("events applied", 10*sec, func() bool {
		return h.M.Stats().Events >= burst && h.Contains("aaa first")
	})
	h.Settle()
	if got := h.M.SelectedSession(); got != target {
		t.Fatalf("selection moved to %s during burst", got)
	}
	st := h.M.Stats()
	t.Logf("events=%d refreshes=%d loads=%d", st.Events, st.Refreshes, st.Loads)
	if st.Refreshes > st.Events/10 {
		t.Errorf("bursts not coalesced: %d refreshes for %d events", st.Refreshes, st.Events)
	}
	if !strings.Contains(h.View(), "› ! needs you  worker") {
		t.Errorf("cursor row is not the target:\n%s", h.View())
	}

	// When the selected session disappears, the cursor stays in place
	// (neighbour), rather than jumping to the top.
	idx := indexOf(h.View(), "worker")
	arch := true
	if _, err := api.UpdateSession(ctx(), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_arch"}, SessionID: target, Archived: &arch}); err != nil {
		t.Fatal(err)
	}
	h.WaitFor("archived session gone", 5*sec, func() bool { return !h.Contains("worker ") })
	if got := h.M.SelectedSession(); got == "" || got == target {
		t.Fatalf("selection after removal = %q", got)
	}
	if nidx := cursorLine(h.View()); nidx < idx-1 {
		t.Errorf("cursor jumped from line %d to %d", idx, nidx)
	}
}

func indexOf(view, s string) int {
	for i, l := range strings.Split(view, "\n") {
		if strings.Contains(l, s) {
			return i
		}
	}
	return -1
}

func cursorLine(view string) int {
	for i, l := range strings.Split(view, "\n") {
		if strings.HasPrefix(l, "›") {
			return i
		}
	}
	return -1
}

func TestSlowAPIDoesNotBlockNavigation(t *testing.T) {
	var slow *tuitest.Slow
	h, _ := start(t, 100, 30, func(a contracts.API) contracts.API {
		slow = tuitest.NewSlow(a, 0)
		return slow
	}, func(o *tui.Options) { o.LoadTimeout = 400 * time.Millisecond })

	slow.SetDelay(5 * time.Second) // every read now exceeds the timeout
	t0 := time.Now()
	h.Key("down", "down", "down", "I", "down", "P", "S", "up", "/")
	h.Type("work")
	h.Key("enter")
	if d := time.Since(t0); d > 300*time.Millisecond {
		t.Fatalf("navigation blocked for %s with a slow hub", d)
	}
	if !h.Contains("migration work") {
		t.Fatalf("list not usable while loads are slow:\n%s", h.View())
	}
	if h.Contains("loading preview") {
		t.Errorf("a read that just started should not say loading yet (300 ms):\n%s", h.View())
	}
	h.WaitFor("loading state", 2*sec, func() bool { return h.Contains("loading preview") })
	// A forced refresh times out: previous data stays, labelled.
	h.Key("ctrl+r")
	h.WaitFor("timeout label", 3*sec, func() bool { return h.Contains("hub unreachable") })
	if !h.Contains("migration work") {
		t.Fatalf("data lost after timeout:\n%s", h.View())
	}
	if !h.Contains("showing data from") {
		t.Errorf("stale data not labelled:\n%s", h.View())
	}
	// Recovery.
	slow.SetDelay(0)
	h.Key("ctrl+r")
	h.WaitFor("recovered", 3*sec, func() bool {
		return !h.Contains("unreachable") && (h.Contains("○ synced") || h.Contains("● live"))
	})
}

func TestOfflineNodeShownHonestly(t *testing.T) {
	var b *mock.Backend
	h, _ := start(t, 160, 32, func(a contracts.API) contracts.API { b = a.(*mock.Backend); return a }, nil)
	if err := b.SetNodeConnected("node_synth-linux", false); err != nil {
		t.Fatal(err)
	}
	h.Key("ctrl+r") // r reads the transcript; ctrl+r (or <leader>r) refreshes
	h.WaitFor("offline", 3*sec, func() bool { return h.Contains("◌ offline") })
	selectSession(t, h, "ses_synth-b")
	h.Settle()
	v := h.View()
	if !strings.Contains(v, "The run may still be alive.") {
		t.Errorf("offline preview should not claim the run ended:\n%s", v)
	}
	if strings.Contains(v, "✗ failed") {
		t.Errorf("offline must not be shown as failed:\n%s", v)
	}
}

func TestAttachReturnsToSameListFilterSelection(t *testing.T) {
	rec := (*tuitest.Recorder)(nil)
	h, _ := start(t, 100, 30, func(a contracts.API) contracts.API { rec = tuitest.NewRecorder(a); return rec }, nil)
	h.Key("/")
	h.Type("claude")
	h.Key("enter")
	selectSession(t, h, "ses_synth-a")
	before := h.View()
	h.Key("F") // full-screen attach (enter types into the embedded terminal)
	h.WaitFor("attach executed", 3*sec, func() bool { return len(h.Attaches()) == 1 })
	h.Settle()
	d := h.Attaches()[0]
	if d.Mechanism != contracts.AttachSSHTmux || len(d.Command) == 0 {
		t.Fatalf("descriptor = %+v", d)
	}
	if got := writableAttaches(rec); len(got) != 1 || got[0].SessionID != "ses_synth-a" {
		t.Fatalf("writable attach calls = %+v", got)
	}
	if h.M.ActiveView() != "Sessions" || h.M.Filter() != "claude" || h.M.SelectedSession() != "ses_synth-a" {
		t.Fatalf("after detach: view=%s filter=%q sel=%s", h.M.ActiveView(), h.M.Filter(), h.M.SelectedSession())
	}
	if cursorLine(before) != cursorLine(h.View()) {
		t.Errorf("cursor row changed across attach")
	}
	if !h.Contains("detached from coordinator") {
		t.Errorf("missing detach notice:\n%s", h.View())
	}
}

// writableAttaches are the recorded attach requests that ask for a
// writable terminal (the embedded pane's read-only views are left out).
func writableAttaches(rec *tuitest.Recorder) []contracts.AttachRequest {
	var out []contracts.AttachRequest
	for _, c := range rec.Calls("AttachSession") {
		if r := c.Request.(contracts.AttachRequest); !r.ReadOnly {
			out = append(out, r)
		}
	}
	return out
}

func TestApprovalDoublePressUsesSameOperationID(t *testing.T) {
	var rec *tuitest.Recorder
	var flaky *flakyAnswers
	h, api := start(t, 100, 30, func(a contracts.API) contracts.API {
		flaky = &flakyAnswers{API: a}
		rec = tuitest.NewRecorder(flaky)
		return rec
	}, nil)
	selectSession(t, h, "ses_synth-apr")
	h.Key("a") // answer from the session (there is no Inbox view)
	h.Settle()
	if !h.Contains("Allow once") {
		t.Fatalf("approval not shown:\n%s", h.View())
	}
	// The first attempt is lost in transit (hub unavailable): the UI says
	// the outcome is unknown. Pressing the same option again retries with
	// the SAME operation id, so the hub can never apply it twice.
	flaky.failNext(1)
	h.Key("1")
	h.WaitFor("unknown outcome", 3*sec, func() bool { return h.Contains("Outcome unknown") })
	h.Settle()
	// Rapid double press while the retry is in flight: one request only.
	flaky.setDelay(300 * time.Millisecond)
	h.Key("1", "1", "1")
	h.WaitFor("answer result", 3*sec, func() bool { return h.Contains("Answer submitted") || h.Contains("Answer took effect") })
	h.Settle()
	calls := rec.Calls("AnswerApproval")
	if len(calls) != 2 {
		t.Fatalf("expected 2 answer calls (lost + retry), got %d", len(calls))
	}
	for _, c := range calls {
		r := c.Request.(contracts.AnswerApprovalRequest)
		if c.OperationID != calls[0].OperationID {
			t.Fatalf("operation ids differ: %s vs %s", c.OperationID, calls[0].OperationID)
		}
		if r.ApprovalID == "" || r.RunID == "" || r.OptionID != "allow" {
			t.Fatalf("answer not addressed by (approval_id, run_id): %+v", r)
		}
	}
	ap, err := api.GetApproval(ctx(), contracts.GetRequest{ID: calls[0].Request.(contracts.AnswerApprovalRequest).ApprovalID})
	if err != nil || ap.State != contracts.ApprovalResolved {
		t.Fatalf("approval state = %v, %v", ap.State, err)
	}
	// Once resolved, answer controls are withdrawn: more presses send nothing.
	h.Key("1", "2")
	h.Settle()
	if n := len(rec.Calls("AnswerApproval")); n != 2 {
		t.Fatalf("answered a resolved approval (%d calls)", n)
	}
}

// flakyAnswers fails the next n AnswerApproval calls with a retryable
// error WITHOUT forwarding them, and can add latency.
type flakyAnswers struct {
	contracts.API
	mu    sync.Mutex
	fail  int
	delay time.Duration
}

func (f *flakyAnswers) failNext(n int) { f.mu.Lock(); f.fail = n; f.mu.Unlock() }

func (f *flakyAnswers) setDelay(d time.Duration) { f.mu.Lock(); f.delay = d; f.mu.Unlock() }

func (f *flakyAnswers) AnswerApproval(ctx context.Context, req contracts.AnswerApprovalRequest) (contracts.Approval, error) {
	f.mu.Lock()
	fail, d := f.fail > 0, f.delay
	if fail {
		f.fail--
	}
	f.mu.Unlock()
	time.Sleep(d)
	if fail {
		return contracts.Approval{}, contracts.Errorf(contracts.ErrUnavailable, "synthetic: connection reset")
	}
	return f.API.AnswerApproval(ctx, req)
}

func TestWithdrawnApprovalDisappears(t *testing.T) {
	b, clk := tuitest.NewMock(t)
	opts := tuitest.Options()
	opts.Clock = clk
	opts.Watch = tui.PollWatcher(b)
	h := tuitest.New(t, b, opts, 100, 30)
	h.WaitFor("loaded", 5*sec, func() bool { return h.M.Loaded("attention") })
	h.Key("ctrl+p") // the finder lists what needs you (there is no Inbox view)
	h.Type("Synthetic rm")
	run := runOf(t, b, "ses_synth-a")
	ap, err := b.RequestApproval(run, mock.ApprovalSpec{Title: "Synthetic rm -rf build/", Body: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	h.WaitFor("new approval visible", 5*sec, func() bool { return h.Contains("Approve: Synthetic rm") })
	if _, err := b.ResolveApprovalNatively(ap.ApprovalID, "allow"); err != nil {
		t.Fatal(err)
	}
	h.WaitFor("approval withdrawn", 5*sec, func() bool { return !h.Contains("Approve: Synthetic rm") })
}

func TestComposeSendShowsDeliveryFacts(t *testing.T) {
	var rec *tuitest.Recorder
	h, _ := start(t, 100, 30, func(a contracts.API) contracts.API { rec = tuitest.NewRecorder(a); return rec }, nil)
	selectSession(t, h, "ses_synth-b")
	h.Key("m")
	if !h.Contains("steer unavailable") {
		t.Errorf("steer should be shown unavailable for claude:\n%s", h.View())
	}
	h.Type("Please review the synthetic report")
	h.Key("ctrl+s")
	h.WaitFor("messages view", 3*sec, func() bool { return h.M.ActiveView() == "Messages" })
	h.Settle()
	calls := rec.Calls("SendMessage")
	if len(calls) != 1 {
		t.Fatalf("send calls = %d", len(calls))
	}
	r := calls[0].Request.(contracts.SendMessageRequest)
	if r.Recipient.ID != "ses_synth-b" || r.Mode != contracts.ModeQueue || r.OperationID == "" {
		t.Fatalf("request = %+v", r)
	}
	v := h.View()
	for _, want := range []string{"Stored", "Runtime", "Ack", "Please review the synthetic report"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q:\n%s", want, v)
		}
	}
}

func TestIndeterminateDeliveryNeedsExplicitReconcile(t *testing.T) {
	var rec *tuitest.Recorder
	h, _ := start(t, 160, 34, func(a contracts.API) contracts.API { rec = tuitest.NewRecorder(a); return rec }, nil)
	selectSession(t, h, "ses_synth-b")
	h.Key("H")
	h.Settle()
	for i := 0; i < 12 && !h.Contains("may have accepted"); i++ {
		h.Key("down")
	}
	if !h.Contains("may have accepted") {
		t.Fatalf("indeterminate message not found:\n%s", h.View())
	}
	h.Key("R")
	if !h.Contains("Pyre never retries this automatically") {
		t.Fatalf("reconcile overlay:\n%s", h.View())
	}
	h.Key("d")
	if !h.Contains("DUPLICATE RISK") {
		t.Fatalf("duplicate risk warning missing:\n%s", h.View())
	}
	if len(rec.Calls("ReissueMessage")) != 0 {
		t.Fatal("reissued before confirmation")
	}
	h.Key("y")
	h.WaitFor("reissue", 3*sec, func() bool { return len(rec.Calls("ReissueMessage")) == 1 })
	if r := rec.Calls("ReissueMessage")[0].Request.(contracts.ReissueMessageRequest); !r.AcceptDuplicateRisk || r.MessageID != "msg_synth-indeterminate" {
		t.Fatalf("reissue request = %+v", r)
	}
	if len(rec.Calls("SendMessage")) != 0 {
		t.Fatal("an automatic send happened")
	}
}

func TestInterruptRequiresConfirmation(t *testing.T) {
	var rec *tuitest.Recorder
	h, _ := start(t, 100, 30, func(a contracts.API) contracts.API { rec = tuitest.NewRecorder(a); return rec }, nil)
	selectSession(t, h, "ses_synth-a")
	h.Key("!")
	if !h.M.OverlayOpen() || !h.Contains("Interrupt") {
		t.Fatalf("no confirmation:\n%s", h.View())
	}
	h.Key("n")
	if len(rec.Calls("InterruptSession")) != 0 {
		t.Fatal("interrupt sent without confirmation")
	}
	h.Key("!", "y")
	h.WaitFor("interrupt sent", 3*sec, func() bool { return len(rec.Calls("InterruptSession")) == 1 })
	h.Settle()
	if !h.Contains("accepted=true") {
		t.Errorf("receipt not shown:\n%s", h.View())
	}
}

func TestUnsupportedActionsShowReason(t *testing.T) {
	h, _ := start(t, 160, 32, nil, nil)
	h.Key(".")                           // ended sessions are hidden by default
	selectSession(t, h, "ses_synth-fbx") // exited run
	h.Settle()
	if !h.Contains("The run has ended.") {
		t.Fatalf("the terminal pane should say the run ended:\n%s", h.View())
	}
	h.Key("i") // details live behind i
	if !h.Contains("Unavailable") || !h.Contains("attach, interrupt, stop, steer — no live run") {
		t.Fatalf("missing unavailable reason:\n%s", h.View())
	}
	h.Key("esc")
	h.Key("enter")
	if !h.Contains("attach unavailable: no live run") {
		t.Fatalf("missing toast:\n%s", h.View())
	}
}

func TestLaunchFlow(t *testing.T) {
	var rec *tuitest.Recorder
	h, _ := start(t, 100, 30, func(a contracts.API) contracts.API { rec = tuitest.NewRecorder(a); return rec }, nil)
	h.Key("n", "ctrl+a") // ^a in the new-session panel: the step-by-step launch
	h.Settle()
	if !h.Contains("synth-mac") {
		t.Fatalf("launch machines:\n%s", h.View())
	}
	// choose synth-mac (second row), claude, directory → scratch → use
	for h.M.ActiveView() == "Launch" && !strings.Contains(currentRow(h.View()), "synth-mac") {
		h.Key("down")
	}
	h.Key("enter") // machine
	h.Key("enter") // claude
	h.Key("enter") // choose a directory
	// The directory listing loads asynchronously; under load a fixed settle
	// can return before it arrives, so wait for the entries themselves.
	h.WaitFor("directory listing", 5*sec, func() bool { return h.Contains("scratch/") })
	h.Key("down", "down", "enter") // scratch/
	h.WaitFor("scratch listing", 5*sec, func() bool { return h.Contains("/Users/synth/scratch") })
	h.Key("enter") // [use] /Users/synth/scratch
	h.WaitFor("review", 5*sec, func() bool { return h.Contains("Review & start") })
	if !h.Contains("Review & start") || !h.Contains("/Users/synth/scratch") || !h.Contains("never crosses providers") {
		t.Fatalf("summary:\n%s", h.View())
	}
	h.Key("t")
	h.Type("tui launch")
	h.Key("enter", "enter", "enter") // finish title edit, start, repeated enter
	h.WaitFor("started", 3*sec, func() bool { return h.M.ActiveView() == "Sessions" && h.Contains("tui launch") })
	h.Settle()
	calls := rec.Calls("StartSession")
	if len(calls) == 0 {
		t.Fatal("no start call")
	}
	for _, c := range calls {
		if c.OperationID != calls[0].OperationID {
			t.Fatal("repeated enter used a new operation id")
		}
	}
	r := calls[0].Request.(contracts.StartSessionRequest)
	if r.NodeID != "node_synth-mac" || r.Harness != contracts.HarnessClaude || r.Cwd != "/Users/synth/scratch" || r.AccountPolicy.Mode != contracts.AccountEligible || r.Title != "tui launch" {
		t.Fatalf("start request = %+v", r)
	}
	sel, _ := rec.GetSession(ctx(), contracts.GetRequest{ID: h.M.SelectedSession()})
	if sel.Session.Title != "tui launch" {
		t.Fatalf("new session not selected: %q", sel.Session.Title)
	}
}

func currentRow(view string) string {
	for _, l := range strings.Split(view, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "›") {
			return l
		}
	}
	return ""
}

func TestProjectHomeAndReview(t *testing.T) {
	var rec *tuitest.Recorder
	h, _ := start(t, 160, 34, func(a contracts.API) contracts.API { rec = tuitest.NewRecorder(a); return rec }, nil)
	h.Key("2", "enter")
	h.Settle()
	if !h.Contains("Constraint: keep API stable.") {
		t.Fatalf("brief not shown:\n%s", h.View())
	}
	h.Key("right", "right")
	h.Settle()
	h.Key("enter")
	h.Settle()
	if !h.Contains("HTML is active content") {
		t.Fatalf("html version should not render inline:\n%s", h.View())
	}
	h.Key("down") // v1 markdown
	h.Settle()
	if !h.Contains("v1 report.md") || !h.Contains("Comments on v1 (1)") {
		t.Fatalf("v1 review:\n%s", h.View())
	}
	h.Key("c")
	h.Type("synthetic: table 2 needs units")
	h.Key("enter")
	h.WaitFor("comment", 3*sec, func() bool { return len(rec.Calls("CreateComment")) == 1 })
	c := rec.Calls("CreateComment")[0].Request.(contracts.CreateCommentRequest)
	if c.Target.Kind != contracts.KindArtifactVersion || c.Target.ID == "" {
		t.Fatalf("comment target = %+v", c.Target)
	}
	h.WaitFor("comment listed", 3*sec, func() bool { return h.Contains("table 2 needs units") })
}

func TestFinderReviewOpensArtifact(t *testing.T) {
	h, _ := start(t, 160, 34, nil, nil)
	h.Key("ctrl+p")
	h.Type("ready for review")
	h.Settle()
	h.Key("enter")
	h.WaitFor("artifact view", 3*sec, func() bool { return h.M.ActiveView() == "Artifact" })
	h.Settle()
	if !h.Contains("Synthetic test report") {
		t.Fatalf("artifact:\n%s", h.View())
	}
	h.Key("esc")
	h.Key("esc")
	if h.M.ActiveView() != "Sessions" {
		t.Errorf("back navigation ended at %s", h.M.ActiveView())
	}
}

func TestManagementViews(t *testing.T) {
	h, _ := start(t, 160, 34, nil, nil)
	h.Key(":")
	h.Type("accounts")
	h.Key("enter")
	h.Settle()
	v := h.View()
	for _, want := range []string{"Anthropic", "at limit", "ready", "5-hour", "weekly", "in 1h 19m", "in use", "Credentials never leave"} {
		if !strings.Contains(v, want) {
			t.Errorf("accounts view missing %q:\n%s", want, v)
		}
	}
	h.Key("esc", ":")
	h.Type("machines")
	h.Key("enter")
	h.Settle()
	if !h.Contains("codex executable not found") && !h.Contains("✗codex") {
		t.Errorf("machines should show harness availability:\n%s", h.View())
	}
	h.Key("esc", ":")
	h.Type("environments")
	h.Key("enter")
	h.Settle()
	if !h.Contains("drift") {
		t.Errorf("env view:\n%s", h.View())
	}
}

// tuitestWidth is the display width of a rendered line.
func tuitestWidth(s string) int { return ansi.StringWidth(s) }

func TestWatchResetRequiredResyncsFromSnapshotCursor(t *testing.T) {
	b, clk := tuitest.NewMock(t)
	var mu sync.Mutex
	var cursors []int64
	poll := tui.PollWatcher(b)
	watch := func(ctx context.Context, after int64, fn func(contracts.Event) error) error {
		mu.Lock()
		cursors = append(cursors, after)
		first := len(cursors) == 1
		mu.Unlock()
		if first {
			return contracts.Errorf(contracts.ErrResetRequired, "cursor older than retention").WithDetail("snapshot_cursor", float64(7))
		}
		return poll(ctx, after, fn)
	}
	opts := tuitest.Options()
	opts.Clock = clk
	opts.Watch = watch
	opts.WatchRetry = 50 * time.Millisecond
	h := tuitest.New(t, b, opts, 100, 30)
	h.WaitFor("watch restarted from snapshot cursor", 5*sec, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(cursors) >= 2
	})
	mu.Lock()
	got := cursors[1]
	mu.Unlock()
	if got != 7 {
		t.Fatalf("resumed after cursor %d, want snapshot cursor 7", got)
	}
	// The UI keeps working and re-snapshotted.
	h.WaitFor("sessions", 5*sec, func() bool { return h.Contains("coordinator") })
}

func TestLaunchTitleAcceptsAnyKeys(t *testing.T) {
	h, _ := start(t, 100, 30, nil, nil)
	h.Key("n", "ctrl+a", "enter", "enter", "enter")
	h.Settle()
	h.Key("enter")
	h.Settle()
	h.Key("t")
	h.Type("SPI: ?q/")
	if h.M.ActiveView() != "Launch" || !h.Contains("SPI: ?q/") {
		t.Fatalf("global keys hijacked the title field:\n%s", h.View())
	}
}

func TestDocumentEditPublishesAndPreservesConflicts(t *testing.T) {
	// The TUI keeps the local draft (os.CreateTemp) after a conflict by
	// design; keep it inside the test's own temp dir.
	drafts := t.TempDir()
	t.Setenv("TMPDIR", drafts)
	var api contracts.API
	var competing bool
	edit := func(path string) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if competing {
			// Another writer publishes while our editor is open.
			docs, _ := api.ListDocuments(ctx(), contracts.ListDocumentsRequest{})
			for _, d := range docs.Items {
				if d.Slug == "brief" {
					_, err := api.ProposeRevision(ctx(), contracts.ProposeRevisionRequest{Mutation: contracts.Mutation{OperationID: "op_competing"},
						DocumentID: d.DocumentID, BaseRevisionID: d.HeadRevisionID, Content: "competing edit\n"})
					if err != nil {
						return err
					}
				}
			}
		}
		return os.WriteFile(path, append(b, []byte("\nSynthetic addition from the TUI.\n")...), 0o600)
	}
	h, a := start(t, 160, 34, nil, func(o *tui.Options) { o.Edit = edit })
	api = a
	h.Key("2", "enter", "right")
	h.Settle()
	h.Key("e")
	h.WaitFor("published", 3*sec, func() bool { return h.Contains("published r") })
	h.Settle()
	if !h.Contains("Synthetic addition from the TUI.") {
		t.Fatalf("new revision not shown:\n%s", h.View())
	}
	competing = true
	h.Key("e")
	h.WaitFor("conflict", 3*sec, func() bool { return h.Contains("Conflict:") })
	if !h.Contains("preserved as proposal prp_") {
		t.Fatalf("conflict must name the preserved proposal:\n%s", h.View())
	}
	if kept, _ := filepath.Glob(filepath.Join(drafts, "pyre-draft-*.md")); len(kept) != 1 {
		t.Fatalf("conflict must keep exactly one local draft, found %v", kept)
	}
}

// A hung machine (found with a SIGSTOPped pyred on a real fleet): its
// preview must say it is waiting for that machine, not "loading" forever,
// while navigation keeps working.
func TestHungNodePreviewSaysWaiting(t *testing.T) {
	var slow *tuitest.Slow
	h, _ := start(t, 160, 34, func(a contracts.API) contracts.API {
		slow = tuitest.NewSlow(a, 0)
		slow.Only = map[string]bool{"ReadSession": true}
		return slow
	}, func(o *tui.Options) { o.LoadTimeout = 30 * time.Second })
	slow.SetDelay(time.Minute)
	h.Key("down", "down")
	h.WaitFor("waiting label", 8*sec, func() bool { return h.Contains("no answer for") })
	if !h.Contains("waiting for synth-") {
		t.Errorf("should name the machine:\n%s", h.View())
	}
	before := h.M.SelectedSession()
	h.Key("down")
	if h.M.SelectedSession() == before {
		t.Fatal("navigation blocked by a hung preview")
	}
	slow.SetDelay(0)
}
