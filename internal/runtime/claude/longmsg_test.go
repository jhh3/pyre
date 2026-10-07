package claude

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// A long draft in an 80-column pane, captured from real Claude Code 2.1.289
// on server (rc4 smoke defect, synthetic content): the box shows only the last
// six rows, so the marker line "[pyre-msg:…] from owner:" is out of view.
const realLongDraft80 = `  1 more notice hidden
                                                         ctrl+g to edit in Vim
────────────────────────────────────────────────────────────────────────────────
❯ Synthetic Pyre delivery check base400 (400 chars). No work is needed: do not
  run tools or commands, and reply in this terminal with just the word ok.
  The rest is filler. Filler sentence 1 of the synthetic base400 check,
  deliberately long. Filler sentence 2 of the synthetic base400 check,
  deliberately long. Filler sentence 3 of the synthetic base400 check,
  deliberately long. Filler sentence 4 o end
────────────────────────────────────────────────────────────────────────────────
  ⚠ status line · 1:31 PM PT
  ⏸ manual mode on · 1 monitor`

const longDraftBody = "Synthetic Pyre delivery check base400 (400 chars). No work is needed: do not run tools or commands, " +
	"and reply in this terminal with just the word ok. The rest is filler. Filler sentence 1 of the synthetic base400 check, " +
	"deliberately long. Filler sentence 2 of the synthetic base400 check, deliberately long. Filler sentence 3 of the synthetic " +
	"base400 check, deliberately long. Filler sentence 4 o end"

func TestPasteVisibleLongDraftTail(t *testing.T) {
	marker := "[pyre-msg:msg_long1]"
	text := marker + " from owner:\n" + longDraftBody
	si := parseScreen(realLongDraft80, "")
	if !si.HasInputBox || si.InputEmpty() || si.RuleWidth != 80 {
		t.Fatalf("parse: %+v", si)
	}
	if si.markerOnInput(marker, true) {
		t.Fatal("marker should be out of view in this capture")
	}
	if got := si.pasteVisible(marker, text); got != "tail" {
		t.Fatalf("pasteVisible = %q, want tail", got)
	}
	// Someone else's text after Pyre's paste: not the end of the message.
	other := strings.Replace(realLongDraft80, "Filler sentence 4 o end", "Filler sentence 4 o end and more typing", 1)
	if got := parseScreen(other, "").pasteVisible(marker, text); got != "" {
		t.Fatalf("foreign suffix recognised as %q", got)
	}
	// A different message with the same filler but another ending.
	if got := si.pasteVisible(marker, text+" extra"); got != "" {
		t.Fatalf("different text recognised as %q", got)
	}
	// A word changed in the middle of the visible rows.
	mid := strings.Replace(realLongDraft80, "Filler sentence 2", "Filler sentence 9", 1)
	if got := parseScreen(mid, "").pasteVisible(marker, text); got != "" {
		t.Fatalf("altered draft recognised as %q", got)
	}
	// The marker and the collapsed placeholder still count.
	if got := parseScreen(realDraft, "").pasteVisible("[pyre-msg:msg_calib1]", "x"); got != "marker" {
		t.Fatalf("marker: %q", got)
	}
	if got := parseScreen(realLongPaste, "").pasteVisible("[pyre-msg:x]", "x"); got != "collapsed" {
		t.Fatalf("collapsed: %q", got)
	}
	// An empty box (placeholder) is never a paste.
	rule := strings.Repeat("─", 80)
	if got := parseScreen(realIdle, rule+"\n"+realIdleEscLine+"\n"+rule).pasteVisible(marker, text); got != "" {
		t.Fatalf("placeholder: %q", got)
	}
}

func TestTailMatchRules(t *testing.T) {
	norm := normDraft("[pyre-msg:m] from owner:\n" + longDraftBody)
	short := normDraft("hi")
	cases := []struct {
		vis, norm string
		want      bool
	}{
		{"", norm, false},
		{norm, norm, true},                       // whole text visible
		{norm[len(norm)-40:], norm, true},        // a tail of minTailMatch characters
		{norm[len(norm)-39:], norm, false},       // too short to identify the message
		{short, short, true},                     // a short text, fully visible
		{"x" + norm[len(norm)-60:], norm, false}, // extra character before the tail
		{norm[len(norm)-60:] + "x", norm, false}, // extra character after it
	}
	for i, c := range cases {
		if got := tailMatch(c.vis, c.norm); got != c.want {
			t.Errorf("case %d: tailMatch=%v want %v", i, got, c.want)
		}
	}
	// NBSP (Claude's prompt padding) and wrapping whitespace are ignored.
	if normDraft("a b \n\tc") != "abc" {
		t.Fatal("normDraft")
	}
	long := strings.Repeat("é", 2000)
	if tl := draftTail(long); len(tl) > maxDraftTail || !strings.HasSuffix(long, tl) || !strings.HasPrefix(tl, "é") {
		t.Fatalf("draftTail cut a rune or kept too much: %d", len(tl))
	}
}

func TestLastAttemptsCarriesTailAndMode(t *testing.T) {
	dir := t.TempDir()
	a := &Adapter{o: Options{Now: time.Now}}
	h := contracts.RunHandle{RunID: "run_x", StateDir: dir}
	_ = a.journal(h, journalRec{MessageID: "m1", AttemptID: "a1", State: jAttempting, Mode: "paste", Tail: "tail1"})
	_ = a.journal(h, journalRec{MessageID: "m1", AttemptID: "a1", State: jIndeterminate, Detail: "x"})
	_ = a.journal(h, journalRec{MessageID: "m2", AttemptID: "a2", State: jAttempting, Mode: "paste", Tail: "tail2"})
	_ = a.journal(h, journalRec{MessageID: "m2", AttemptID: "a3", State: jAttempting, Mode: "stop_hook"})
	m := lastAttempts(dir)
	if r := m["m1"]; r.State != jIndeterminate || r.Mode != "paste" || r.Tail != "tail1" || r.AttemptID != "a1" {
		t.Fatalf("m1 %+v", r)
	}
	if r := m["m2"]; r.Mode != "stop_hook" || r.Tail != "" || r.AttemptID != "a3" {
		t.Fatalf("a new attempt must not inherit the previous one: %+v", r)
	}
}

func paneSize(t *testing.T, a *Adapter, h contracts.RunHandle) (int, int) {
	t.Helper()
	p, err := a.srv.Pane(context.Background(), h.TmuxTarget)
	if err != nil {
		t.Fatal(err)
	}
	return p.Width, p.Height
}

// narrowPane leaves the run window the way a detached 80x24 client does.
func narrowPane(t *testing.T, a *Adapter, h contracts.RunHandle) {
	t.Helper()
	ctx := context.Background()
	if _, err := a.srv.Run(ctx, nil, "resize-window", "-t", tmux.PaneTarget(h.TmuxTarget), "-x", "80", "-y", "23"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.srv.Run(ctx, nil, "set-option", "-w", "-u", "-t", tmux.PaneTarget(h.TmuxTarget), "window-size"); err != nil {
		t.Fatal(err)
	}
	if w, _ := paneSize(t, a, h); w != 80 {
		t.Fatalf("narrow pane width %d", w)
	}
}

func longText(n int) string {
	var b strings.Builder
	for i := 1; b.Len() < n; i++ {
		b.WriteString("Synthetic filler sentence " + strings.Repeat("x", i%7) + " for the long message check. ")
	}
	return b.String()[:n]
}

func screenText(t *testing.T, a *Adapter, h contracts.RunHandle) string {
	s, _ := a.srv.CapturePane(context.Background(), h.TmuxTarget, tmux.CaptureOptions{})
	return s
}

// The rc4 defect: a narrow pane left by a detached client. Pyre restores the
// launch size before pasting, so the marker stays in view.
func TestFakeLongMessageNarrowPaneRestored(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	h := f.launch("run_long1", map[string]string{"FAKE_DRAFT_ROWS": "6"}, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	narrowPane(t, f.a, h)
	text := longText(600)
	r := deliver(t, f.a, h, "msg_long1", text, contracts.ModeQueue)
	if r.State != contracts.DeliveryAccepted {
		t.Fatalf("deliver: %+v\n%s", r, screenText(t, f.a, h))
	}
	if w, hh := paneSize(t, f.a, h); w != launchWidth || hh != launchHeight {
		t.Fatalf("pane %dx%d, want the launch size back", w, hh)
	}
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	if n := countUserRecords(t, f.a, h, "msg_long1"); n != 1 {
		t.Fatalf("transcript records %d, want 1", n)
	}
	si, _ := f.a.screen(context.Background(), h.TmuxTarget)
	if !si.InputEmpty() {
		t.Fatalf("leftover draft %q", si.InputLines)
	}
}

// With a client still attached the pane cannot be resized: the paste is
// recognised by its visible tail. A read-only 80x24 viewer keeps the pane
// narrow without holding the input lease.
func TestFakeLongMessageNarrowViewerTailMatch(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	h := f.launch("run_long2", map[string]string{"FAKE_DRAFT_ROWS": "6"}, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	ctx := context.Background()
	ro, err := f.a.Attach(ctx, h, contracts.AttachCommand{RunID: h.RunID, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	outer, _ := tmux.NewServer("", filepath.Join(f.dir, "o.sock"))
	defer outer.KillServer(ctx)
	if err := outer.NewSession(ctx, tmux.SessionSpec{Name: "viewer", Argv: ro.Command, Width: 80, Height: 24}); err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		if w, _ := paneSize(t, f.a, h); w == 80 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if w, _ := paneSize(t, f.a, h); w != 80 {
		t.Fatalf("viewer did not narrow the pane: %d", w)
	}
	r := deliver(t, f.a, h, "msg_long2", longText(700), contracts.ModeQueue)
	if r.State != contracts.DeliveryAccepted {
		t.Fatalf("deliver: %+v\n%s", r, screenText(t, f.a, h))
	}
	if w, _ := paneSize(t, f.a, h); w != 80 {
		t.Fatalf("pane resized under an attached viewer: %d", w)
	}
	var how string
	for _, rec := range readJournal(h.StateDir) {
		if rec.MessageID == "msg_long2" && rec.State == jSubmitted {
			how = rec.Detail
		}
	}
	if !strings.Contains(how, "tail") {
		t.Fatalf("submitted record %q, want recognition by tail", how)
	}
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	if n := countUserRecords(t, f.a, h, "msg_long2"); n != 1 {
		t.Fatalf("transcript records %d, want 1", n)
	}
}

// Text that lands after Pyre's paste (someone else) is never submitted: the
// result is indeterminate with the action for a human, and nothing reaches
// the transcript.
func TestFakeForeignTextAfterPasteNotSubmitted(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	h := f.launch("run_long3", map[string]string{"FAKE_DRAFT_ROWS": "2", "FAKE_PASTE_SUFFIX": " plus text typed by someone else"}, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	r := deliver(t, f.a, h, "msg_long3", longText(500), contracts.ModeQueue)
	if r.State != contracts.DeliveryIndeterminate || !strings.Contains(r.Detail, "Enter not pressed") ||
		!strings.Contains(r.Detail, "Needs a human") || !strings.Contains(r.Detail, "pyre resolve-delivery msg_long3 rejected") {
		t.Fatalf("deliver: %+v", r)
	}
	time.Sleep(500 * time.Millisecond)
	if n := countUserRecords(t, f.a, h, "msg_long3"); n != 0 {
		t.Fatalf("foreign text submitted: %d records", n)
	}
	si, _ := f.a.screen(context.Background(), h.TmuxTarget)
	if si.InputEmpty() {
		t.Fatal("the unrecognised text must be left for a human, not cleared")
	}
	// Reconcile cannot identify it either: it stays indeterminate and is
	// not cleared.
	rr, err := f.a.Reconcile(context.Background(), h, "msg_long3")
	if err != nil || rr.State != contracts.DeliveryIndeterminate {
		t.Fatalf("reconcile: %+v %v", rr, err)
	}
	if si, _ := f.a.screen(context.Background(), h.TmuxTarget); si.InputEmpty() {
		t.Fatal("reconcile cleared text it could not identify")
	}
	// The next message is held behind the draft, not pasted over it.
	r2 := deliver(t, f.a, h, "msg_long3b", "short", contracts.ModeQueue)
	if r2.State != contracts.DeliveryQueued || !strings.Contains(r2.Detail, "unsent draft") {
		t.Fatalf("next delivery: %+v", r2)
	}
}

// A human who attaches between the paste and Enter: Pyre does not press
// Enter, the run state says what to do, and once the human leaves without
// acting, Reconcile recognises the draft by its tail (marker out of view)
// and clears it (never submitted; safe to retry).
func TestFakeHumanAttachesBeforeEnter(t *testing.T) {
	t.Parallel()
	var f *fakeEnv
	var outer *tmux.Server
	var run contracts.RunHandle
	ctx := context.Background()
	f = newFakeEnv(t, func(o *Options) {
		o.testHook = func(p string) error {
			if p != "after_paste" {
				return nil
			}
			d, err := f.a.Attach(ctx, run, contracts.AttachCommand{RunID: run.RunID})
			if err != nil {
				return err
			}
			outer, _ = tmux.NewServer("", filepath.Join(f.dir, "o.sock"))
			if err := outer.NewSession(ctx, tmux.SessionSpec{Name: "human", Argv: d.Command, Width: 200, Height: 50}); err != nil {
				return err
			}
			for i := 0; i < 60; i++ {
				cs, _ := f.a.srv.ListClients(ctx, "run_long4")
				if tmux.WritableClients(cs) == 1 {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			return nil
		}
	})
	h := f.launch("run_long4", map[string]string{"FAKE_DRAFT_ROWS": "2"}, nil)
	run = h
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	r := deliver(t, f.a, h, "msg_long4", longText(500), contracts.ModeQueue)
	if r.State != contracts.DeliveryIndeterminate || !strings.Contains(r.Detail, "human attached") || !strings.Contains(r.Detail, "Enter not pressed") {
		t.Fatalf("deliver: %+v", r)
	}
	if n := countUserRecords(t, f.a, h, "msg_long4"); n != 0 {
		t.Fatalf("submitted under a human: %d", n)
	}
	// The human leaves without touching the draft.
	_ = outer.KillServer(ctx)
	for i := 0; i < 60; i++ {
		cs, _ := f.a.srv.ListClients(ctx, h.TmuxTarget)
		if len(cs) == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	obs := f.waitObs(f.a, h, 5*time.Second, func(o contracts.Observation) bool {
		return strings.Contains(o.StateReason, "needs human") && strings.Contains(o.StateReason, "msg_long4")
	})
	if obs.State != contracts.RunWaitingInput || !strings.Contains(obs.StateReason, "Ctrl-C") {
		t.Fatalf("observation: %s %q", obs.State, obs.StateReason)
	}
	// The next delivery names the leftover draft instead of "not Pyre's".
	r2 := deliver(t, f.a, h, "msg_long4b", "short", contracts.ModeQueue)
	if r2.State != contracts.DeliveryQueued || !strings.Contains(r2.Detail, "Pyre's unsent draft of msg_long4") {
		t.Fatalf("next delivery: %+v", r2)
	}
	f.opts.testHook = nil
	a2 := f.restart()
	rr, err := a2.Reconcile(ctx, h, "msg_long4")
	if err != nil || rr.State != contracts.DeliveryRejected || !strings.Contains(rr.Detail, "cleared") {
		t.Fatalf("reconcile: %+v %v", rr, err)
	}
	if si, _ := a2.screen(ctx, h.TmuxTarget); !si.InputEmpty() {
		t.Fatalf("draft not cleared: %q", si.InputLines)
	}
	r3 := deliver(t, a2, h, "msg_long4b", "short", contracts.ModeQueue)
	if r3.State != contracts.DeliveryAccepted {
		t.Fatalf("after clearing: %+v", r3)
	}
}
