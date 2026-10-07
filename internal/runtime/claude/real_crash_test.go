package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// TestRealClaudeCrashWindowAndQueue: crash before Enter with a multi-line
// draft (reconcile must clear it on the real Ink TUI), crash after Enter
// (reconcile finds evidence), and paste while busy (native queue enqueue
// evidence, later consumed receipt). Skipped unless PYRE_REAL_HARNESS=1.
func TestRealClaudeCrashWindowAndQueue(t *testing.T) {
	if os.Getenv("PYRE_REAL_HARNESS") != "1" {
		t.Skip("set PYRE_REAL_HARNESS=1")
	}
	cwd := os.Getenv("PYRE_REAL_CWD")
	if cwd == "" {
		t.Fatal("PYRE_REAL_CWD required")
	}
	dir, _ := os.MkdirTemp("", "pr")
	defer os.RemoveAll(dir)
	ctx := context.Background()
	o := realOptions(t, dir)
	crashAt := ""
	o.testHook = func(p string) error {
		if p == crashAt {
			return errors.New("simulated crash at " + p)
		}
		return nil
	}
	a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	defer a.srv.KillServer(ctx)
	fresh := func() *Adapter { o2 := o; o2.testHook = nil; b, _ := New(o2); return b }
	h, err := a.Launch(ctx, contracts.ResolvedLaunch{RunID: "run_realc", SessionID: "ses_c", Cwd: cwd, StateDir: filepath.Join(dir, "run"), Args: realArgs})
	if err != nil {
		t.Fatal(err)
	}
	waitIdle := func(ad *Adapter) contracts.Observation {
		t.Helper()
		for i := 0; i < 240; i++ {
			obs, _ := ad.Observe(ctx, h, "")
			if obs.State == contracts.RunWaitingInput {
				return obs
			}
			time.Sleep(500 * time.Millisecond)
		}
		s, _ := ad.srv.CapturePane(ctx, h.TmuxTarget, tmux.CaptureOptions{})
		t.Fatalf("not idle:\n%s", s)
		return contracts.Observation{}
	}
	waitIdle(a)

	// Crash before Enter with a three-line draft.
	crashAt = "after_paste"
	_, err = a.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_cw1", AttemptID: "dlv_cw1",
		Text: "Line one of a synthetic test.\nLine two.\nLine three: reply with the single word ok."})
	t.Logf("deliver with crash before Enter: err=%v", err)
	b := fresh()
	si, _ := b.screen(ctx, h.TmuxTarget)
	t.Logf("draft visible after crash: %v (lines=%d)", strings.Contains(si.InputText(), "[pyre-msg:msg_cw1]"), len(si.InputLines))
	r, _ := b.Reconcile(ctx, h, "msg_cw1")
	t.Logf("reconcile after crash before Enter: state=%s detail=%q", r.State, r.Detail)
	if r.State != contracts.DeliveryRejected {
		t.Fatal("expected rejected (draft cleared)")
	}
	si, _ = b.screen(ctx, h.TmuxTarget)
	if !si.InputEmpty() {
		t.Fatalf("input not empty after clear: %q", si.InputLines)
	}
	r, _ = b.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_cw1", AttemptID: "dlv_cw1b",
		Text: "Line one of a synthetic test.\nLine two.\nLine three: reply with the single word ok."})
	t.Logf("redeliver after clear: state=%s evidence=%s", r.State, r.Evidence)
	if r.State != contracts.DeliveryAccepted {
		t.Fatal("redeliver")
	}
	waitIdle(b)

	// Crash after Enter.
	crashAt = "after_enter"
	_, err = a.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_cw2", AttemptID: "dlv_cw2", Text: "Reply with the single word ok."})
	t.Logf("deliver with crash after Enter: err=%v", err)
	c := fresh()
	var r2 contracts.DeliveryResult
	for i := 0; i < 40; i++ {
		r2, _ = c.Reconcile(ctx, h, "msg_cw2")
		if r2.State == contracts.DeliveryAccepted {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("reconcile after crash after Enter: state=%s evidence=%s", r2.State, r2.Evidence)
	if r2.State != contracts.DeliveryAccepted {
		t.Fatal("expected accepted")
	}
	obs0 := waitIdle(c)

	// Busy paste: native queue.
	r, _ = c.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_long", AttemptID: "dlv_long",
		Text: "Write the integers from 1 to 150 separated by spaces, no other text."})
	t.Logf("deliver long turn: state=%s", r.State)
	busy := false
	for i := 0; i < 40 && !busy; i++ {
		obs, _ := c.Observe(ctx, h, "")
		busy = obs.State == contracts.RunRunning
		time.Sleep(100 * time.Millisecond)
	}
	r, _ = c.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_queued", AttemptID: "dlv_q", Text: "Reply with the single word second."})
	t.Logf("deliver while busy (was running=%v): state=%s evidence=%s", busy, r.State, r.Evidence)
	if r.State != contracts.DeliveryAccepted {
		t.Fatal("busy delivery")
	}
	waitIdle(c)
	time.Sleep(2 * time.Second)
	waitIdle(c)
	obs, _ := c.Observe(ctx, h, obs0.NextCursor)
	kinds := []string{}
	for _, rc := range obs.Receipts {
		if rc.MessageID == "msg_queued" {
			kinds = append(kinds, string(rc.Evidence)+"("+strings.SplitN(rc.Detail, "=", 2)[0]+")")
		}
	}
	t.Logf("receipts for msg_queued: %v", kinds)
	_, _ = c.Stop(ctx, h)
}
