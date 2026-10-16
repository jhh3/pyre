package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeLog(t *testing.T, e *env) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(e.o.StateRoot, "codex", homeKey(e.home), "fake-config.log"))
	return string(b)
}

// Stop unloads the thread (archive + unarchive): unsubscribing alone left it
// loaded with its MCP servers in the real app-server.
func TestStopUnloadsThread(t *testing.T) {
	e := newEnv(t, nil)
	h1 := e.launch(e.a, "run_u1", e.home, nil, nil)
	h2 := e.launch(e.a, "run_u2", e.home, nil, nil) // keeps the app-server alive after the first stop
	_ = h2
	if _, err := e.a.Stop(context.Background(), h1); err != nil {
		t.Fatal(err)
	}
	log := fakeLog(t, e)
	tid := h1.NativeRef.ID
	if !strings.Contains(log, "thread/archive") || !strings.Contains(log, "thread/unarchive") || !strings.Contains(log, tid) {
		t.Fatalf("stop did not unload thread %s: %q", tid, log)
	}
}

// The sweeper unloads a loaded thread no live run owns only when it was
// unowned at the previous sweep too, and never an owned one.
func TestSweepUnloadsLeakedThreads(t *testing.T) {
	e := newEnv(t, nil)
	live := e.launch(e.a, "run_s1", e.home, nil, nil)
	leaked := e.launch(e.a, "run_s2", e.home, nil, nil)
	// Simulate a thread left loaded by an earlier pyred (no run owns it).
	e.a.mu.Lock()
	hs := e.a.homes[homeKey(e.home)]
	e.a.mu.Unlock()
	if hs == nil {
		for _, h := range e.a.homes {
			hs = h
		}
	}
	hs.unregister(leaked.NativeRef.ID)

	seen := e.a.sweepLoaded(context.Background(), map[string]bool{})
	if strings.Contains(fakeLog(t, e), "thread/archive") {
		t.Fatal("unloaded on the first sweep (a launching run could own it)")
	}
	if !seen[leaked.NativeRef.ID] || seen[live.NativeRef.ID] {
		t.Fatalf("unowned set = %v", seen)
	}
	e.a.sweepLoaded(context.Background(), seen)
	log := fakeLog(t, e)
	if !strings.Contains(log, leaked.NativeRef.ID) || !strings.Contains(log, "thread/archive") {
		t.Fatalf("leaked thread not unloaded on the second sweep: %q", log)
	}
	if strings.Contains(log, `"threadId":"`+live.NativeRef.ID+`"`) && strings.Contains(log, "archive") {
		for _, l := range strings.Split(log, "\n") {
			if strings.Contains(l, "archive") && strings.Contains(l, live.NativeRef.ID) {
				t.Fatalf("live thread unloaded: %s", l)
			}
		}
	}
}
