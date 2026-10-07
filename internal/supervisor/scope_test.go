package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeScopeEnv installs a fake environment, cgroup, systemd-run lookup and
// probe for one test.
func fakeScopeEnv(t *testing.T, env map[string]string, cgroup string, haveRun bool, probe error) *int {
	t.Helper()
	calls := 0
	oldEnv, oldCg, oldProbe, oldLook := ScopeEnv, procSelfCgroup, probeScope, lookPath
	ScopeEnv = func(k string) string { return env[k] }
	procSelfCgroup = func() ([]byte, error) { return []byte(cgroup), nil }
	probeScope = func(context.Context, string, string) error { calls++; return probe }
	lookPath = func(string) (string, error) {
		if haveRun {
			return "/usr/bin/systemd-run", nil
		}
		return "", errors.New("not found")
	}
	probeMu.Lock()
	probeCache = map[probeKey]probeResult{}
	probeMu.Unlock()
	t.Cleanup(func() { ScopeEnv, procSelfCgroup, probeScope, lookPath = oldEnv, oldCg, oldProbe, oldLook })
	return &calls
}

const userSvcCgroup = "0::/user.slice/user-1000.slice/user@1000.service/app.slice/pyred.service\n"

func TestScopeModeParsing(t *testing.T) {
	for in, want := range map[string]string{"": ScopeAuto, "auto": ScopeAuto, "REQUIRE": ScopeRequire, "on": ScopeOn, "1": ScopeOn, "off": ScopeOff, "0": ScopeOff, "bogus": ScopeAuto} {
		fakeScopeEnv(t, map[string]string{"PYRE_SYSTEMD_SCOPE": in}, "", false, nil)
		if got := ScopeMode(); got != want {
			t.Fatalf("%q => %q, want %q", in, got, want)
		}
	}
}

func TestDecideScope(t *testing.T) {
	if runtime.GOOS != "linux" {
		fakeScopeEnv(t, map[string]string{"INVOCATION_ID": "x", "PYRE_SYSTEMD_SCOPE": "require"}, userSvcCgroup, true, nil)
		if d := DecideScope(context.Background()); d.Use || d.Required || d.Reason != "not Linux" {
			t.Fatalf("non-Linux: %+v", d)
		}
		return
	}
	ctx := context.Background()
	svc := map[string]string{"INVOCATION_ID": "abc"}

	calls := fakeScopeEnv(t, svc, userSvcCgroup, true, nil)
	d := DecideScope(ctx)
	if !d.Use || d.Slice != DefaultRunsSlice || d.SystemdRun != "/usr/bin/systemd-run" {
		t.Fatalf("user service: %+v", d)
	}
	DecideScope(ctx)
	if *calls != 1 {
		t.Fatalf("successful probe should be cached, got %d calls", *calls)
	}

	fakeScopeEnv(t, map[string]string{}, "0::/user.slice/user-1000.slice/session-3.scope\n", true, nil)
	if d := DecideScope(ctx); d.Use || d.Required {
		t.Fatalf("interactive shell must not use a scope in auto mode: %+v", d)
	}

	fakeScopeEnv(t, map[string]string{"INVOCATION_ID": "x"}, "0::/system.slice/pyred.service\n", true, nil)
	if d := DecideScope(ctx); d.Use {
		t.Fatalf("system service (no user manager) must not use --user scopes: %+v", d)
	}

	fakeScopeEnv(t, map[string]string{"INVOCATION_ID": "x", "PYRE_SYSTEMD_SCOPE": "require"}, userSvcCgroup, false, nil)
	if d := DecideScope(ctx); d.Use || !d.Required || !strings.Contains(d.Reason, "systemd-run not found") {
		t.Fatalf("require without systemd-run: %+v", d)
	}

	calls = fakeScopeEnv(t, svc, userSvcCgroup, true, errors.New("Failed to connect to bus"))
	if d := DecideScope(ctx); d.Use || d.Required || !strings.Contains(d.Reason, "Failed to connect to bus") {
		t.Fatalf("probe failure in auto mode falls back: %+v", d)
	}
	DecideScope(ctx)
	if *calls != 1 {
		t.Fatalf("failed probe is cached for probeRetry, got %d calls", *calls)
	}
	probeMu.Lock()
	for k, r := range probeCache {
		r.at = time.Now().Add(-2 * probeRetry)
		probeCache[k] = r
	}
	probeMu.Unlock()
	DecideScope(ctx)
	if *calls != 2 {
		t.Fatalf("failed probe should be retried after probeRetry, got %d calls", *calls)
	}

	fakeScopeEnv(t, map[string]string{"PYRE_SYSTEMD_SCOPE": "on", "PYRE_SYSTEMD_SLICE": "pyre-test-runs.slice"}, "", true, nil)
	if d := DecideScope(ctx); !d.Use || d.Slice != "pyre-test-runs.slice" {
		t.Fatalf("on + slice override: %+v", d)
	}

	fakeScopeEnv(t, map[string]string{"INVOCATION_ID": "x", "PYRE_SYSTEMD_SCOPE": "off"}, userSvcCgroup, true, nil)
	if d := DecideScope(ctx); d.Use || d.Required {
		t.Fatalf("off: %+v", d)
	}
}

func TestScopeWrapAndNames(t *testing.T) {
	d := ScopeDecision{SystemdRun: "/usr/bin/systemd-run", Slice: "pyre-runs.slice"}
	got := strings.Join(d.Wrap("pyre-tmux-x.scope", []string{"/usr/bin/tmux", "-S", "/s/pyre.sock", "start-server"}), " ")
	want := "/usr/bin/systemd-run --user --scope --quiet --collect --slice=pyre-runs.slice --unit=pyre-tmux-x.scope -- /usr/bin/tmux -S /s/pyre.sock start-server"
	if got != want {
		t.Fatalf("wrap:\n got %s\nwant %s", got, want)
	}
	a, b := ScopeUnitName("/a/tmux/pyre.sock"), ScopeUnitName("/b/tmux/pyre.sock")
	if a == b || !strings.HasPrefix(a, "pyre-tmux-") || !strings.HasSuffix(a, ".scope") || a != ScopeUnitName("/a/tmux/pyre.sock") {
		t.Fatalf("unit names %q %q", a, b)
	}
	if !UnitExists("Failed to start transient scope unit: Unit pyre-tmux-x.scope already exists.") || UnitExists("Failed to connect to bus") {
		t.Fatal("UnitExists")
	}
}

func TestPlacementRecord(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "pyre.sock")
	RecordPlacement(sock, "systemd scope pyre-tmux-x.scope in pyre-runs.slice")
	if got := ReadPlacement(sock); !strings.HasSuffix(got, "systemd scope pyre-tmux-x.scope in pyre-runs.slice") {
		t.Fatalf("placement %q", got)
	}
	if st, _ := os.Stat(filepath.Join(dir, PlacementFile)); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
}
