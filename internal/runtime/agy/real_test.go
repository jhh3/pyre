//go:build !windows

package agy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// TestRealAgy drives the real agy (signed in on this machine) through the
// adapter on a scratch tmux server (`tmux -L agy-test`) in a scratch dir:
// launch with an initial prompt, a delivered prompt with transcript
// evidence, idle detection, MCP tools in the session (a synthetic stdio
// server that reports the session env), interrupt, /exit, and a native
// resume by conversation id with a chosen model. It never touches a
// running pyred or hub. Skipped unless PYRE_REAL_AGY=1;
// PYRE_REAL_AGY_DIR is the scratch dir (default a temp dir) and
// PYRE_REAL_AGY_MCP an MCP server command to register (optional).
func TestRealAgy(t *testing.T) {
	if os.Getenv("PYRE_REAL_AGY") != "1" {
		t.Skip("set PYRE_REAL_AGY=1 to drive the real agy")
	}
	bin, err := exec.LookPath("agy")
	if err != nil {
		t.Skip("agy not installed")
	}
	exe, _ := os.Executable()
	scratch := os.Getenv("PYRE_REAL_AGY_DIR")
	if scratch == "" {
		scratch = t.TempDir()
	}
	work := filepath.Join(scratch, fmt.Sprintf("work-%d", time.Now().Unix()))
	_ = os.MkdirAll(work, 0o755)
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("tmux-%d", os.Getuid()), "agy-test") // = tmux -L agy-test
	_ = os.MkdirAll(filepath.Dir(sock), 0o700)
	o := Options{NodeID: "node_live", TmuxSocket: sock, AgyBin: bin, WorkspaceRoot: filepath.Join(scratch, "ws"),
		HookCommand:    []string{"/usr/bin/env", "PYRE_TEST_ROLE=hook", exe},
		TrustWorkspace: func(cwd string) bool { return strings.HasPrefix(cwd, scratch) },
		DeliverTimeout: 30 * time.Second, StartupWait: 30 * time.Second, StopWait: 15 * time.Second}
	if m := os.Getenv("PYRE_REAL_AGY_MCP"); m != "" {
		o.MCPCommand = strings.Fields(m)
	}
	a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	t.Cleanup(func() { _ = a.srv.KillServer(context.Background()) })
	pr, _ := a.Probe(ctx)
	t.Logf("probe: available=%v version=%s reason=%q models=%d efforts=%v", pr.Available, pr.Version, pr.Reason, len(pr.Models), pr.Efforts)
	if !pr.Available {
		t.Fatalf("agy not available: %s", pr.Reason)
	}
	screen := func(h contracts.RunHandle) string {
		s, _ := a.srv.CapturePane(ctx, h.TmuxTarget, tmux.CaptureOptions{Join: true})
		var keep []string
		for _, l := range strings.Split(s, "\n") {
			if strings.TrimSpace(l) != "" {
				keep = append(keep, strings.TrimRight(l, " "))
			}
		}
		if len(keep) > 8 {
			keep = keep[len(keep)-8:]
		}
		return strings.Join(keep, "\n")
	}
	wait := func(h contracts.RunHandle, want contracts.RunState, d time.Duration) contracts.Observation {
		t.Helper()
		end := time.Now().Add(d)
		var obs contracts.Observation
		for time.Now().Before(end) {
			obs, _ = a.Observe(ctx, h, "")
			if obs.State == want {
				return obs
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("state %s not reached; last %s (%s)\n%s", want, obs.State, obs.StateReason, screen(h))
		return obs
	}
	stamp := fmt.Sprint(time.Now().Unix())
	launch := func(runID string, l contracts.ResolvedLaunch) contracts.RunHandle {
		t.Helper()
		runID += "_" + stamp // a fresh state dir per run, as pyred gives
		l.RunID, l.SessionID, l.Harness, l.Cwd = runID, "ses_01LIVEAGY0000000000000000", contracts.HarnessAgy, work
		l.StateDir = filepath.Join(scratch, "runs", runID)
		if l.Env == nil {
			l.Env = map[string]string{}
		}
		l.Env["PYRE_SESSION_ID"] = l.SessionID
		h, err := a.Launch(ctx, l)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("launched %s: native=%+v data=%v", runID, h.NativeRef, h.Data)
		return h
	}
	deliver := func(h contracts.RunHandle, id, text string) {
		t.Helper()
		r, err := a.Deliver(ctx, h, contracts.DeliveryInput{MessageID: id, AttemptID: "att_" + id, Mode: contracts.ModeQueue, Text: text})
		t.Logf("deliver %s: state=%s evidence=%s ref=%s detail=%q err=%v", id, r.State, r.Evidence, r.EvidenceRef, r.Detail, err)
		if err != nil || r.State != contracts.DeliveryAccepted {
			t.Fatalf("not accepted\n%s", screen(h))
		}
	}

	h := launch("run_live1", contracts.ResolvedLaunch{Env: map[string]string{PermissionsEnv: "bypass"}, InitialPrompt: "Reply with exactly: READY-1",
		AgentGuide: "You are in a Pyre live check. Keep answers to one line."})
	obs := wait(h, contracts.RunWaitingInput, 90*time.Second)
	t.Logf("after initial prompt: state=%s reason=%q native=%+v\n%s", obs.State, obs.StateReason, obs.NativeRef, screen(h))
	if obs.NativeRef == nil || !safeConversationID(obs.NativeRef.ID) {
		t.Fatal("no conversation id")
	}
	// As the delivery engine renders it: the dated header is the marker.
	deliver(h, "msg_LIVE1", "[pyre-msg:msg_LIVE1 · sent "+time.Now().Format("3:04 PM")+" (just now)] from session:live-check\n\nWhat is 2+3? Reply with just the number.")
	obs = wait(h, contracts.RunWaitingInput, 90*time.Second)
	t.Logf("idle after delivery: reason=%q\n%s", obs.StateReason, screen(h))
	if o.MCPCommand != nil {
		deliver(h, "msg_LIVE2", "Call the whoami tool of the pyre MCP server and reply with its exact output only.")
		obs = wait(h, contracts.RunWaitingInput, 180*time.Second)
		t.Logf("after MCP call:\n%s", screen(h))
	}
	deliver(h, "msg_LIVE3", "Run the shell command `sleep 60` in the foreground, then reply DONE.")
	wait(h, contracts.RunRunning, 60*time.Second)
	rc, _ := a.Interrupt(ctx, h)
	t.Logf("interrupt: %+v", rc)
	obs = wait(h, contracts.RunWaitingInput, 30*time.Second)
	t.Logf("after interrupt: reason=%q", obs.StateReason)
	tr, _ := a.ReadTranscript(ctx, h, contracts.ReadSessionRequest{Limit: 50})
	for _, e := range tr.Entries {
		t.Logf("transcript #%d %s %s: %s", e.Seq, e.Kind, e.MessageID, clip(oneLine(e.Text), 120))
	}
	rc, _ = a.Stop(ctx, h)
	t.Logf("stop: %+v", rc)
	if o, _ := a.Observe(ctx, h, ""); o.State != contracts.RunExited {
		t.Fatalf("after stop: %s", o.State)
	}

	kind, ref, _ := a.ResumeSpec(ctx, contracts.Run{RunID: h.RunID, NativeRef: obs.NativeRef}, true)
	t.Logf("resume spec: %s %+v", kind, ref)
	if kind != contracts.ContinuationNativeResume {
		t.Fatal("no native resume")
	}
	h2 := launch("run_live2", contracts.ResolvedLaunch{Env: map[string]string{PermissionsEnv: "bypass"}, Resume: ref, Model: "gemini-3.8-flash-low", Effort: "low"})
	wait(h2, contracts.RunWaitingInput, 60*time.Second)
	t.Logf("resumed screen:\n%s", screen(h2))
	deliver(h2, "msg_LIVE4", "What number did you answer to my 2+3 question earlier? Reply with just the number.")
	obs2 := wait(h2, contracts.RunWaitingInput, 90*time.Second)
	t.Logf("after resume delivery: native=%+v\n%s", obs2.NativeRef, screen(h2))
	if obs2.NativeRef == nil || obs2.NativeRef.ID != ref.ID {
		t.Fatalf("resumed conversation %+v, want %s", obs2.NativeRef, ref.ID)
	}
	rc, _ = a.Stop(ctx, h2)
	t.Logf("stop resumed: %+v", rc)
}
