//go:build !windows

package headless

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// fakeBase is a harness with a canned transcript.
type fakeBase struct{}

func (fakeBase) Probe(ctx context.Context) (contracts.ProbeResult, error) {
	return contracts.ProbeResult{Harness: "synth", Adapter: "synth-base", Available: true, Capabilities: []string{contracts.CapInputQueue}}, nil
}
func (fakeBase) ReadTranscript(ctx context.Context, h contracts.RunHandle, req contracts.ReadSessionRequest) (contracts.ReadSessionResponse, error) {
	return contracts.ReadSessionResponse{Screen: "a terminal", Entries: []contracts.TranscriptEntry{{Seq: 1, Kind: "user", Text: "synthetic"}}}, nil
}
func (fakeBase) TranscriptFiles(h contracts.RunHandle) (string, []string) {
	return "synth", []string{"/synthetic/t.jsonl"}
}
func (fakeBase) ResumeSpec(ctx context.Context, prev contracts.Run, same bool) (contracts.ContinuationKind, *contracts.NativeRef, error) {
	return contracts.ContinuationHandoff, nil, nil
}

type recorder struct {
	found []string // FindNative results to hand out
	asked int
	known map[string]string // run id -> native id told to NativeKnown
}

func newTestAdapter(r *recorder) *Adapter {
	r.known = map[string]string{}
	return New(Spec{Name: "synth-exec", Harness: contracts.HarnessCodex, Base: fakeBase{}, Child: "synth", NativeKind: "synth_thread",
		Home:        func(l contracts.ResolvedLaunch) string { return l.ProfileDir },
		ValidNative: func(id string) bool { return strings.HasPrefix(id, "thr_") },
		FindNative: func(st State) string {
			r.asked++
			if len(r.found) == 0 {
				return ""
			}
			id := r.found[0]
			r.found = r.found[1:]
			return id
		},
		NativeKnown: func(runID, dir string, st State, id string) { r.known[runID] = id }})
}

func launch(t *testing.T, x *Adapter, runID string, resume *contracts.NativeRef) contracts.RunHandle {
	t.Helper()
	h, err := x.Launch(context.Background(), contracts.ResolvedLaunch{RunID: runID, SessionID: "ses_synth", Cwd: t.TempDir(),
		StateDir: filepath.Join(t.TempDir(), runID), ProfileDir: "/synthetic/home", Resume: resume})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The native id of an ended run: the one the CLI saw (when valid), else
// the resumed conversation, else the harness's lookup — which is asked only
// for a new conversation.
func TestExitedNativeID(t *testing.T) {
	r := &recorder{found: []string{"thr_found"}}
	x := newTestAdapter(r)
	ctx := context.Background()
	code := 0

	h := launch(t, x, "run_seen", nil)
	_ = x.Claimed(h, os.Getpid())
	if err := x.Exited(h, &code, "", "thr_seen"); err != nil {
		t.Fatal(err)
	}
	if obs, _ := x.Observe(ctx, h, ""); obs.NativeRef == nil || obs.NativeRef.Kind != "synth_thread" || obs.NativeRef.ID != "thr_seen" || r.known["run_seen"] != "thr_seen" {
		t.Fatalf("seen: %+v %v", obs.NativeRef, r.known)
	}

	h = launch(t, x, "run_resumed", &contracts.NativeRef{Kind: "synth_thread", ID: "thr_old"})
	if h.NativeRef == nil || h.NativeRef.ID != "thr_old" || r.known["run_resumed"] != "thr_old" {
		t.Fatalf("resume handle %+v %v", h.NativeRef, r.known)
	}
	_ = x.Claimed(h, os.Getpid())
	_ = x.Exited(h, &code, "", "not-a-thread") // invalid: ignored
	if obs, _ := x.Observe(ctx, h, ""); obs.NativeRef == nil || obs.NativeRef.ID != "thr_old" || r.asked != 0 {
		t.Fatalf("resumed: %+v asked %d", obs.NativeRef, r.asked)
	}

	h = launch(t, x, "run_other_kind", &contracts.NativeRef{Kind: "claude_session", ID: "thr_wrongkind"})
	if h.NativeRef != nil {
		t.Fatalf("another harness's conversation taken as resume: %+v", h.NativeRef)
	}

	h = launch(t, x, "run_new", nil)
	_ = x.Claimed(h, os.Getpid())
	_ = x.Exited(h, nil, "SIGTERM", "")
	obs, _ := x.Observe(ctx, h, "")
	if obs.State != contracts.RunExited || obs.ExitCode != nil || obs.StateReason != "headless (pyre run): killed by SIGTERM" ||
		obs.NativeRef == nil || obs.NativeRef.ID != "thr_found" || r.asked != 1 {
		t.Fatalf("new: %+v asked %d", obs, r.asked)
	}
	// A second exit report changes nothing.
	_ = x.Exited(h, &code, "", "thr_late")
	if obs, _ := x.Observe(ctx, h, ""); obs.NativeRef.ID != "thr_found" || obs.ExitCode != nil {
		t.Fatalf("re-exit: %+v", obs)
	}
}

// Phases, claim rules, stop before and after the start, a vanished CLI,
// and the delegated reads (no terminal, the headless capabilities).
func TestLifecycle(t *testing.T) {
	x := newTestAdapter(&recorder{})
	ctx := context.Background()
	if pr, _ := x.Probe(ctx); pr.Adapter != "synth-exec" || contracts.HasCap(pr.Capabilities, contracts.CapInputQueue) || !contracts.HasCap(pr.Capabilities, contracts.CapStop) {
		t.Fatalf("probe %+v", pr)
	}
	h := launch(t, x, "run_l", nil)
	if h.Adapter != "synth-exec" || h.Harness != contracts.HarnessCodex {
		t.Fatalf("handle %+v", h)
	}
	if obs, _ := x.Observe(ctx, h, ""); obs.State != contracts.RunStarting {
		t.Fatalf("pending %+v", obs)
	}
	if err := x.Claimed(h, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := x.Claimed(h, os.Getpid()); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("second claim: %v", err)
	}
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if err := x.Started(h, child.Process.Pid, false); err != nil {
		t.Fatal(err)
	}
	if obs, _ := x.Observe(ctx, h, ""); obs.State != contracts.RunRunning {
		t.Fatalf("running %+v", obs)
	}
	if _, err := x.Stop(ctx, h); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = child.Process.Kill()
		t.Fatal("stop did not end the child")
	}
	resp, _ := x.ReadTranscript(ctx, h, contracts.ReadSessionRequest{})
	if resp.Screen != "" || len(resp.Entries) != 1 {
		t.Fatalf("read %+v", resp)
	}
	if _, err := x.Deliver(ctx, h, contracts.DeliveryInput{}); !contracts.IsCode(err, contracts.ErrUnsupported) {
		t.Fatalf("deliver: %v", err)
	}

	// Stopped before the child started: the start report is refused.
	h2 := launch(t, x, "run_s", nil)
	_ = x.Claimed(h2, os.Getpid())
	_, _ = x.Stop(ctx, h2)
	if err := x.Started(h2, os.Getpid(), false); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("start after stop: %v", err)
	}

	// A CLI gone without reporting ends the run; an unclaimed run ends
	// after ClaimTimeout.
	gone := exec.Command("true")
	_ = gone.Run()
	h3 := launch(t, x, "run_g", nil)
	_ = x.Claimed(h3, gone.Process.Pid)
	if obs, _ := x.Observe(ctx, h3, ""); obs.State != contracts.RunExited || !strings.Contains(obs.StateReason, "without reporting") {
		t.Fatalf("vanished %+v", obs)
	}
	defer func(d time.Duration) { ClaimTimeout = d }(ClaimTimeout)
	ClaimTimeout = 0
	h4 := launch(t, x, "run_u", nil)
	time.Sleep(time.Millisecond)
	if obs, _ := x.Observe(ctx, h4, ""); obs.State != contracts.RunExited || !strings.Contains(obs.StateReason, "never took the launch") {
		t.Fatalf("unclaimed %+v", obs)
	}
	if _, err := x.Recover(ctx, contracts.RunHandle{RunID: "run_none", StateDir: t.TempDir()}); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("recover without state: %v", err)
	}
}
