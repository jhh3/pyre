package supervisor

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseLimit(t *testing.T) {
	const ram = 30 << 30
	for _, tc := range []struct {
		in   string
		want uint64
		set  bool
		err  bool
	}{
		{"65%", uint64(0.65 * float64(ram)), true, false},
		{"16G", 16 << 30, true, false},
		{"512M", 512 << 20, true, false},
		{"512MB", 512 << 20, true, false},
		{"1048576", 1 << 20, true, false},
		{"0", 0, true, false},
		{"0%", 0, true, false},
		{"off", 0, false, false},
		{"", 0, false, false},
		{"infinity", 0, false, false},
		{"150%", 0, false, true},
		{"lots", 0, false, true},
		{"-1G", 0, false, true},
	} {
		got, set, err := ParseLimit(tc.in, ram)
		if (err != nil) != tc.err || got != tc.want || set != tc.set {
			t.Errorf("%q => %d %v %v, want %d %v err=%v", tc.in, got, set, err, tc.want, tc.set, tc.err)
		}
	}
	if err := (RunScopeLimits{MemoryMax: "lots"}).Validate(); err == nil {
		t.Fatal("bad spec validated")
	}
	if !(RunScopeLimits{MemoryMax: "off", SwapMax: "25%"}).Off() || (RunScopeLimits{MemoryMax: "65%"}).Off() {
		t.Fatal("Off")
	}
}

// The incident node: 30 GiB RAM, 8 GiB swap.
func TestRunScopeResolveAndArgs(t *testing.T) {
	l := RunScopeLimits{MemoryMax: DefaultRunMemoryMax, SwapMax: DefaultRunSwapMax, MemoryHigh: "off"}
	sc, err := l.Resolve(30<<30, 8<<30)
	if err != nil {
		t.Fatal(err)
	}
	if sc.MemoryMax != uint64(0.65*float64(30<<30)) || sc.SwapMax != 2<<30 || sc.MemoryHigh != 0 || sc.SwapZero {
		t.Fatalf("resolved %+v", sc)
	}
	sc.Busctl, sc.Slice, sc.OOMPolicy = "/usr/bin/busctl", DefaultRunsSlice, true
	if d := sc.Describe(); d != "MemoryMax=19.5G MemorySwapMax=2.0G OOMPolicy=continue" {
		t.Fatalf("describe %q", d)
	}
	args := strings.Join(sc.StartArgs("pyre-run-run_x-1.scope", 4242, "Pyre run run_x"), " ")
	want := "/usr/bin/busctl --user --quiet --timeout=5 call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager StartTransientUnit ssa(sv)a(sa(sv)) " +
		"pyre-run-run_x-1.scope fail 7 PIDs au 1 4242 Slice s pyre-runs.slice Description s Pyre run run_x CollectMode s inactive-or-failed " +
		"MemoryMax t 20937965568 MemorySwapMax t 2147483648 OOMPolicy s continue 0"
	if args != want {
		t.Fatalf("args\n%s\nwant\n%s", args, want)
	}
	// An explicit zero swap is sent; "off" sends nothing.
	z, _ := RunScopeLimits{MemoryMax: "1G", SwapMax: "0"}.Resolve(8<<30, 0)
	if !z.SwapZero || !strings.Contains(strings.Join(z.StartArgs("u.scope", 1, "d"), " "), "MemorySwapMax t 0") {
		t.Fatalf("swap 0: %+v", z)
	}
	n, _ := RunScopeLimits{MemoryMax: "1G", SwapMax: "off"}.Resolve(8<<30, 8<<30)
	if strings.Contains(strings.Join(n.StartArgs("u.scope", 1, "d"), " "), "MemorySwapMax") {
		t.Fatal("swap off still sent")
	}
	u := RunScopeUnit("run_01ABC", time.Unix(1, 5))
	if !strings.HasPrefix(u, "pyre-run-run_01ABC-") || !strings.HasSuffix(u, ".scope") || u == RunScopeUnit("run_01ABC", time.Unix(1, 6)) {
		t.Fatalf("unit %q", u)
	}
}

func TestSystemdVersion(t *testing.T) {
	for in, want := range map[string]int{`s "259.5-0ubuntu3pop0"`: 259, `s "252"`: 252, "": 0, `s "x"`: 0} {
		if got := systemdVersion(in); got != want {
			t.Errorf("%q => %d, want %d", in, got, want)
		}
	}
}

// fakeRunScope installs the scope seams for one test.
func fakeRunScope(t *testing.T, haveBusctl bool, version string, verErr error, delegated bool) {
	t.Helper()
	oldLook, oldVer, oldDel, oldMem := lookPath, busctlVersion, memoryDelegated, readMeminfo
	lookPath = func(name string) (string, error) {
		if name == "busctl" && !haveBusctl {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + name, nil
	}
	busctlVersion = func(context.Context, string) (string, error) { return version, verErr }
	memoryDelegated = func() bool { return delegated }
	readMeminfo = func() ([]byte, error) { return []byte("MemTotal: 31457280 kB\nSwapTotal: 8388608 kB\n"), nil }
	runScopeMu.Lock()
	runScopeCache.key, runScopeCache.at = "", time.Time{}
	runScopeMu.Unlock()
	t.Cleanup(func() {
		lookPath, busctlVersion, memoryDelegated, readMeminfo = oldLook, oldVer, oldDel, oldMem
		runScopeMu.Lock()
		runScopeCache.key, runScopeCache.at = "", time.Time{}
		runScopeMu.Unlock()
	})
}

func TestDecideRunScope(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("per-run scopes are Linux-only")
	}
	ctx := context.Background()
	l := RunScopeLimits{MemoryMax: "65%", SwapMax: "25%"}
	svc := map[string]string{"INVOCATION_ID": "abc"}

	// Not a systemd service: no run-server scope, so none per run either.
	fakeScopeEnv(t, map[string]string{}, userSvcCgroup, true, nil)
	fakeRunScope(t, true, `s "259"`, nil, true)
	if d := DecideRunScope(ctx, l); d.Use || !strings.Contains(d.Reason, "no run-server scope") {
		t.Fatalf("outside a service: %+v", d)
	}

	// No user bus: falls back with the reason.
	fakeScopeEnv(t, svc, userSvcCgroup, true, nil)
	fakeRunScope(t, true, "", errors.New("Failed to connect to bus: No such file or directory"), true)
	if d := DecideRunScope(ctx, l); d.Use || !strings.Contains(d.Reason, "not reachable on the user bus") {
		t.Fatalf("no bus: %+v", d)
	}
	fakeRunScope(t, false, `s "259"`, nil, true)
	if d := DecideRunScope(ctx, l); d.Use || d.Reason != "busctl not found" {
		t.Fatalf("no busctl: %+v", d)
	}
	fakeRunScope(t, true, `s "259"`, nil, false)
	if d := DecideRunScope(ctx, l); d.Use || !strings.Contains(d.Reason, "memory controller") {
		t.Fatalf("not delegated: %+v", d)
	}
	if d := DecideRunScope(ctx, RunScopeLimits{MemoryMax: "off", SwapMax: "25%"}); d.Use || d.Reason != "per-run limits off" {
		t.Fatalf("off: %+v", d)
	}

	// Usable; OOMPolicy only from systemd 253.
	fakeRunScope(t, true, `s "259.5-0ubuntu3"`, nil, true)
	d := DecideRunScope(ctx, l)
	if !d.Use || !d.Scope.OOMPolicy || d.Scope.Busctl != "/usr/bin/busctl" || d.Scope.Slice != DefaultRunsSlice || d.Scope.MemoryMax != uint64(0.65*float64(30<<30)) || d.Scope.SwapMax != 2<<30 {
		t.Fatalf("usable: %+v", d)
	}
	fakeRunScope(t, true, `s "249.11"`, nil, true)
	if d := DecideRunScope(ctx, l); !d.Use || d.Scope.OOMPolicy {
		t.Fatalf("systemd 249: %+v", d)
	}
}
