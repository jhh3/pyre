package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Automatic stops spare live process groups (decision I62). The fake
// app-server runs FAKE_KEEP commands as real processes in their own groups
// and, like Codex, kills every group its thread holds (exited commands'
// too) on clean and unload.

// procAlive: pid exists and is not a zombie.
func procAlive(pid int) bool {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	st, _, ok := parseProcStat(b)
	return ok && st != 'Z' && st != 'X'
}

func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for !cond() {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// keepChild returns FAKE_KEEP <name>'s child and process group once Pyre
// recorded the group and (exited) the top shell has exited.
func keepChild(t *testing.T, e *env, h contracts.RunHandle, name string, exited bool) (child, pgid int) {
	t.Helper()
	file := filepath.Join(e.home, "keep-"+name+".child")
	waitUntil(t, 5*time.Second, "keep child pid file", func() bool {
		b, err := os.ReadFile(file)
		child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && child > 0
	})
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(child), "stat"))
	if err != nil {
		t.Fatal(err)
	}
	_, pgid, _ = parseProcStat(b)
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	hs, _ := e.a.homeServer(e.home)
	waitUntil(t, 5*time.Second, "group recorded", func() bool {
		hs.groupsMu.Lock()
		defer hs.groupsMu.Unlock()
		for _, g := range hs.groupsLocked(h.NativeRef.ID) {
			if g.PGID == pgid {
				return true
			}
		}
		return false
	})
	if exited {
		waitUntil(t, 5*time.Second, "top shell exit", func() bool { return !procAlive(pgid) })
	}
	if !procAlive(child) {
		t.Fatal("keep child not running")
	}
	return child, pgid
}

func cleanedOrUnloaded(t *testing.T, e *env, tid string) bool {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(e.home, "fake-cleans.log"))
	if strings.Contains(string(b), tid) {
		return true
	}
	for _, l := range strings.Split(fakeLog(t, e), "\n") {
		if strings.HasPrefix(l, "thread/archive ") && strings.Contains(l, tid) {
			return true
		}
	}
	return false
}

func automationsOf(r *automations, action string) []contracts.NodeAutomation {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []contracts.NodeAutomation
	for _, a := range r.got {
		if a.Action == action {
			out = append(out, a)
		}
	}
	return out
}

func turnCount(t *testing.T, e *env, tid string) int {
	t.Helper()
	hs, _ := e.a.homeServer(e.home)
	var resp turnsListResponse
	if err := hs.connected().call(context.Background(), "thread/turns/list", turnsListParams{ThreadID: tid}, &resp); err != nil {
		t.Fatal(err)
	}
	return len(resp.Data)
}

// An archive-when-done stop (StopSparing) of a run whose exited exec left a
// live child in its process group ends the run but keeps the thread loaded:
// no clean, no unload, the child survives, nothing wakes the thread, and the
// deferral survives a pyred restart. Once the child exits, the thread is
// unloaded.
func TestCodexAutoStopSparesLiveGroup(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	ctx := context.Background()
	h := e.launch(e.a, "run_sp1", e.home, nil, nil)
	tid := h.NativeRef.ID
	e.wait(e.a, h, 5*time.Second, idle)
	// r: exits at once, leaving its child in its group (the render);
	// w: still running at the stop, exits after it.
	dlv(t, e.a, h, "msg_sp1", "render FAKE_KEEP=1000:r FAKE_KEEP=4000:w", contracts.ModeQueue)
	child, pgid := keepChild(t, e, h, "r", true)
	wchild, wpg := keepChild(t, e, h, "w", false)
	waitNotices(t, e.a, h, 1, 5*time.Second) // r's exit woke the agent (I52)
	e.wait(e.a, h, 5*time.Second, idle)

	rc, err := e.a.StopSparing(ctx, h)
	if err != nil || rc.Outcome != contracts.OutcomeSucceeded {
		t.Fatalf("%+v %v", rc, err)
	}
	for _, want := range []string{"thread kept loaded", "2 background process group(s)", fmt.Sprintf("pgid %d", pgid), "keep-r"} {
		if !strings.Contains(rc.Detail, want) {
			t.Errorf("receipt lacks %q: %s", want, rc.Detail)
		}
	}
	turns := turnCount(t, e, tid)
	waitUntil(t, 8*time.Second, "w's top shell exits", func() bool { return !procAlive(wpg) })
	time.Sleep(500 * time.Millisecond)
	if !procAlive(child) {
		t.Fatal("the preserved child was killed by an automatic stop")
	}
	if cleanedOrUnloaded(t, e, tid) {
		t.Fatal("thread cleaned or unloaded while its groups had live processes")
	}
	if n := turnCount(t, e, tid); n != turns {
		t.Fatalf("a turn started on the deferred thread (%d -> %d turns)", turns, n)
	}
	if obs, _ := e.a.Observe(ctx, h, ""); obs.State != contracts.RunExited {
		t.Fatalf("run state %s", obs.State)
	}
	if r, _ := e.a.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_sp2", AttemptID: "dlv_sp2", Mode: contracts.ModeQueue, Text: "hi"}); r.State != contracts.DeliveryRejected {
		t.Fatalf("delivery to the stopped run: %+v", r)
	}
	waitUntil(t, 2*time.Second, "stop_deferred audit", func() bool { return len(automationsOf(rec, contracts.AutomationStopDeferred)) == 1 })
	if d := automationsOf(rec, contracts.AutomationStopDeferred)[0]; d.RunID != "run_sp1" || !strings.Contains(d.Detail, strconv.Itoa(pgid)) {
		t.Fatalf("stop_deferred automation: %+v", d)
	}
	hs, _ := e.a.homeServer(e.home)
	if _, err := os.Stat(filepath.Join(hs.refsDir(), deferRef(tid))); err != nil {
		t.Fatalf("no app-server ref for the deferral: %v", err)
	}

	// A pyred restart keeps the deferral (and re-subscribes the thread).
	a2 := e.restart()
	a2.findDeferred()
	a2.checkDeferred(ctx)
	if cleanedOrUnloaded(t, e, tid) || !procAlive(child) {
		t.Fatal("restarted adapter unloaded the deferred thread")
	}
	hs2, _ := a2.homeServer(e.home)
	if !hs2.isDeferred(tid) {
		t.Fatal("deferral lost across a restart")
	}

	// One group empties: still deferred. Both empty: the next check unloads
	// the thread.
	_ = syscall.Kill(child, syscall.SIGKILL)
	waitUntil(t, 3*time.Second, "child exit", func() bool { return !procAlive(child) })
	a2.checkDeferred(ctx)
	if cleanedOrUnloaded(t, e, tid) || !procAlive(wchild) {
		t.Fatal("unloaded while w's group still had a live process")
	}
	_ = syscall.Kill(wchild, syscall.SIGKILL)
	waitUntil(t, 3*time.Second, "w child exit", func() bool { return !procAlive(wchild) })
	a2.checkDeferred(ctx)
	if !cleanedOrUnloaded(t, e, tid) {
		t.Fatal("thread not unloaded once its groups were empty")
	}
	if hs2.isDeferred(tid) {
		t.Fatal("deferral kept after the unload")
	}
	if _, err := os.Stat(filepath.Join(hs.refsDir(), deferRef(tid))); err == nil {
		t.Fatal("deferral ref kept after the unload")
	}
}

// The deferral is capped: past the limit the thread is unloaded and what
// is left in its groups is killed.
func TestCodexAutoStopDeferralCap(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	ctx := context.Background()
	h := e.launch(e.a, "run_sp3", e.home, nil, nil)
	tid := h.NativeRef.ID
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_sp3", "render FAKE_KEEP=800:r", contracts.ModeQueue)
	child, _ := keepChild(t, e, h, "r", true)
	waitNotices(t, e.a, h, 1, 5*time.Second)
	e.wait(e.a, h, 5*time.Second, idle)
	if rc, _ := e.a.StopSparing(ctx, h); !strings.Contains(rc.Detail, "thread kept loaded") {
		t.Fatalf("not deferred: %s", rc.Detail)
	}
	e.a.checkDeferred(ctx) // within the limit
	if cleanedOrUnloaded(t, e, tid) {
		t.Fatal("unloaded before the limit")
	}
	e.a.deferMax = time.Millisecond
	e.a.checkDeferred(ctx)
	if !cleanedOrUnloaded(t, e, tid) {
		t.Fatal("thread still loaded past the limit")
	}
	waitUntil(t, 3*time.Second, "child killed at the limit", func() bool { return !procAlive(child) })
	waitUntil(t, 2*time.Second, "stop_resumed audit", func() bool { return len(automationsOf(rec, contracts.AutomationStopResumed)) == 1 })
	if d := automationsOf(rec, contracts.AutomationStopResumed)[0]; !strings.Contains(d.Detail, "the limit") {
		t.Fatalf("stop_resumed automation: %+v", d)
	}
}

// A stop the owner or an agent asked for still cleans and unloads the
// thread, killing the group, and says so.
func TestCodexExplicitStopKillsGroups(t *testing.T) {
	t.Parallel()
	e, _ := newWakeEnv(t, nil)
	ctx := context.Background()
	h := e.launch(e.a, "run_sp4", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_sp4", "render FAKE_KEEP=800:r", contracts.ModeQueue)
	child, pgid := keepChild(t, e, h, "r", true)
	waitNotices(t, e.a, h, 1, 5*time.Second)
	e.wait(e.a, h, 5*time.Second, idle)
	rc, err := e.a.Stop(ctx, h)
	if err != nil || rc.Outcome != contracts.OutcomeSucceeded {
		t.Fatalf("%+v %v", rc, err)
	}
	if !strings.Contains(rc.Detail, "1 background process group(s) were killed") || !strings.Contains(rc.Detail, fmt.Sprintf("pgid %d", pgid)) {
		t.Fatalf("receipt: %s", rc.Detail)
	}
	if procAlive(child) {
		t.Fatal("explicit stop left the group's child running")
	}
	if !cleanedOrUnloaded(t, e, h.NativeRef.ID) {
		t.Fatal("explicit stop did not unload the thread")
	}
}

// The idle-thread sweep defers a thread no run owns the same way.
func TestCodexSweepSparesLiveGroup(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	ctx := context.Background()
	h := e.launch(e.a, "run_sp5", e.home, nil, nil)
	tid := h.NativeRef.ID
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_sp5", "render FAKE_KEEP=800:r", contracts.ModeQueue)
	child, _ := keepChild(t, e, h, "r", true)
	waitNotices(t, e.a, h, 1, 5*time.Second)
	e.wait(e.a, h, 5*time.Second, idle)
	hs, _ := e.a.homeServer(e.home)
	hs.unregister(tid) // left loaded by an earlier pyred: no run owns it
	seen := e.a.sweepLoaded(ctx, map[string]bool{})
	e.a.sweepLoaded(ctx, seen)
	if cleanedOrUnloaded(t, e, tid) || !procAlive(child) {
		t.Fatal("sweep unloaded a thread whose group had a live process")
	}
	if !hs.isDeferred(tid) {
		t.Fatal("sweep did not defer the thread")
	}
	waitUntil(t, 2*time.Second, "stop_deferred audit", func() bool { return len(automationsOf(rec, contracts.AutomationStopDeferred)) == 1 })
	e.a.sweepLoaded(ctx, seen) // a deferred thread is owned: never swept
	if cleanedOrUnloaded(t, e, tid) {
		t.Fatal("deferred thread swept")
	}
	_ = syscall.Kill(child, syscall.SIGKILL)
	waitUntil(t, 3*time.Second, "child exit", func() bool { return !procAlive(child) })
	e.a.checkDeferred(ctx)
	if !cleanedOrUnloaded(t, e, tid) || hs.isDeferred(tid) {
		t.Fatal("thread not unloaded once its group was empty")
	}
}

// The group record survives in <dir>/execgroups and forgets empty groups.
func TestCodexLiveGroupsRecord(t *testing.T) {
	t.Parallel()
	e, _ := newWakeEnv(t, nil)
	ctx := context.Background()
	h := e.launch(e.a, "run_sp6", e.home, nil, nil)
	tid := h.NativeRef.ID
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_sp6", "render FAKE_KEEP=600:r", contracts.ModeQueue)
	child, pgid := keepChild(t, e, h, "r", true)
	hs, _ := e.a.homeServer(e.home)
	b, err := os.ReadFile(filepath.Join(hs.groupsDir(), tid+".json"))
	var gs []execGroup
	if err != nil || json.Unmarshal(b, &gs) != nil || len(gs) != 1 || gs[0].PGID != pgid || gs[0].ProcessID == "" || !strings.Contains(gs[0].Command, "keep-r") {
		t.Fatalf("record %s %v", b, err)
	}
	live, ok := hs.liveGroups(ctx, hs.connected(), tid)
	if !ok || len(live) != 1 || len(live[0].Members) != 1 || live[0].Members[0].PID != child || !strings.Contains(live[0].Members[0].Cmd, "sleep 600") {
		t.Fatalf("live %+v %v", live, ok)
	}
	_ = syscall.Kill(child, syscall.SIGKILL)
	waitUntil(t, 3*time.Second, "child exit", func() bool { return !procAlive(child) })
	if live, ok := hs.liveGroups(ctx, hs.connected(), tid); !ok || len(live) != 0 {
		t.Fatalf("live after exit %+v", live)
	}
	if _, err := os.Stat(filepath.Join(hs.groupsDir(), tid+".json")); err == nil {
		t.Fatal("empty group still recorded")
	}
	_, _ = e.a.Stop(ctx, h)
}

func TestCodexParseProcGroups(t *testing.T) {
	st, pg, ok := parseProcStat([]byte("4242 (we (ird) name) S 1 3350785 3350785 0 -1 4194560"))
	if !ok || st != 'S' || pg != 3350785 {
		t.Fatalf("%c %d %v", st, pg, ok)
	}
	if _, _, ok := parseProcStat([]byte("garbage")); ok {
		t.Fatal("garbage parsed")
	}
	out := []byte("  101   100 S    render-cli --frames 7165\n  102   100 Z    <defunct>\n  103   200 S    other\n")
	m := parsePS(out, map[int]bool{100: true})
	if len(m) != 1 || len(m[100]) != 1 || m[100][0].PID != 101 || m[100][0].Cmd != "render-cli --frames 7165" {
		t.Fatalf("%+v", m)
	}
	d := describeGroups([]liveGroup{{execGroup: execGroup{PGID: 100, Command: "zsh -lc 'bash render.sh'"}, Members: m[100]}})
	if d != "pgid 100 (exec `zsh -lc 'bash render.sh'`): 101 render-cli --frames 7165" {
		t.Fatalf("describe: %q", d)
	}
}

// Decision I67: a guarded stop (a proactive account move) refuses while the
// thread's exec groups have live processes, stops nothing, and goes ahead
// once they exited.
func TestCodexGuardedStopRefusesLiveGroup(t *testing.T) {
	t.Parallel()
	e, _ := newWakeEnv(t, nil)
	ctx := context.Background()
	h := e.launch(e.a, "run_sp9", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_sp9", "render FAKE_KEEP=800:g", contracts.ModeQueue)
	child, pgid := keepChild(t, e, h, "g", true)
	waitNotices(t, e.a, h, 1, 5*time.Second)
	e.wait(e.a, h, 5*time.Second, idle)
	if got := e.a.BackgroundWork(ctx, h); len(got) != 1 || !strings.Contains(got[0], fmt.Sprintf("pgid %d", pgid)) {
		t.Fatalf("background work %q", got)
	}
	rc, err := e.a.StopUnlessBackground(ctx, h)
	if tasks := contracts.BackgroundRefusal(err); len(tasks) != 1 || rc.Outcome != contracts.OutcomeFailed {
		t.Fatalf("guarded stop %+v %v", rc, err)
	}
	if !procAlive(child) || cleanedOrUnloaded(t, e, h.NativeRef.ID) {
		t.Fatal("a refused stop touched the group or the thread")
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	waitUntil(t, 5*time.Second, "group gone", func() bool { return !procAlive(child) })
	waitUntil(t, 5*time.Second, "no background work", func() bool { return len(e.a.BackgroundWork(ctx, h)) == 0 })
	if rc, err := e.a.StopUnlessBackground(ctx, h); err != nil || rc.Outcome != contracts.OutcomeSucceeded {
		t.Fatalf("guarded stop without live groups %+v %v", rc, err)
	}
}
