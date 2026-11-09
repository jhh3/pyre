package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Per-run memory scopes (Linux, decision I66).
//
// The tmux server scope (scope.go) holds every pane, so one runaway tool
// process could take the whole node: the slice's MemoryHigh only throttles,
// and the kernel's choice under node-wide pressure is swap thrash first. With
// per-run scopes each pane process moves itself, before it execs the
// harness, into its own transient scope in the runs slice:
//
//	pyre-run-<session>-<suffix>.scope  MemoryMax, MemorySwapMax, OOMPolicy=continue
//
// so a run (the harness and everything it spawns) that reaches its cap is
// OOM-killed inside its own scope: the kernel picks the largest process
// there (the runaway tool, not the harness), and OOMPolicy=continue keeps
// systemd from stopping the rest of the scope (the default for a scope is
// stop, checked on systemd 259: the harness got SIGTERM too). Codex's shared
// app-server pane gets one scope: its threads' commands share that cap.
//
// The move is done by the pane launcher itself (runtime/tmux execenv.go):
// it calls the user manager's StartTransientUnit with its own PID through
// busctl, waits until /proc/self/cgroup names the new scope, then execs the
// harness in place (same PID, same process group). It fails open: without
// a user bus, or when the call fails, the harness runs unscoped where tmux
// put it, and the reason is appended to <socket dir>/run-scopes.log.
//
// Agents' own detached jobs leave by design: `systemd-run --user --unit`
// runs the job as a unit of its own (outside the run's scope and process
// tree; capped only by what it sets), and `systemd-run --user --scope`
// moves a command to a scope of its own (out of the run's cap, still in its
// process tree). `setsid -f` leaves the run's process tree but stays in its
// scope, so the run's cap still covers it.

// RunScopeLimits are the per-run limits as configured (pyred flags). A
// Spec is "65%" (of RAM, resp. swap), a size ("16G", "512M", bytes), or
// "" / "off" (no limit).
type RunScopeLimits struct {
	MemoryMax  string
	MemoryHigh string
	SwapMax    string
}

// Default per-run limits.
const (
	DefaultRunMemoryMax = "65%"
	DefaultRunSwapMax   = "25%"
)

// RunScope is a decided per-run scope configuration (bytes; 0 = unset).
type RunScope struct {
	Busctl     string
	Slice      string
	MemoryMax  uint64
	MemoryHigh uint64
	SwapMax    uint64
	SwapZero   bool // SwapMax is an explicit 0 (no swap)
	OOMPolicy  bool // the user manager takes OOMPolicy= on scopes (systemd >= 253)
	Reason     string
}

// RunScopeDecision says whether runs get their own scopes.
type RunScopeDecision struct {
	Use    bool
	Scope  RunScope
	Reason string
}

// ParseLimit converts a limit spec to bytes against total (RAM or swap
// bytes). ok is false for "", "off", "none", "0%"-free forms meaning no
// limit; err for a malformed spec.
func ParseLimit(spec string, total uint64) (bytes uint64, set bool, err error) {
	s := strings.TrimSpace(strings.ToLower(spec))
	switch s {
	case "", "off", "none", "no", "infinity", "max":
		return 0, false, nil
	}
	if p, ok := strings.CutSuffix(s, "%"); ok {
		f, err := strconv.ParseFloat(p, 64)
		if err != nil || f < 0 || f > 100 {
			return 0, false, fmt.Errorf("bad percentage %q", spec)
		}
		return uint64(f / 100 * float64(total)), true, nil
	}
	mult := uint64(1)
	for _, u := range []struct {
		suf string
		m   uint64
	}{{"k", 1 << 10}, {"m", 1 << 20}, {"g", 1 << 30}, {"t", 1 << 40}} {
		if v, ok := strings.CutSuffix(strings.TrimSuffix(s, "b"), u.suf); ok {
			s, mult = v, u.m
			break
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, false, fmt.Errorf("bad size %q (want a percentage such as 65%%, a size such as 16G, or off)", spec)
	}
	return uint64(f * float64(mult)), true, nil
}

// Validate checks the specs parse.
func (l RunScopeLimits) Validate() error {
	for name, v := range map[string]string{"memory max": l.MemoryMax, "memory high": l.MemoryHigh, "swap max": l.SwapMax} {
		if _, _, err := ParseLimit(v, 1<<30); err != nil {
			return fmt.Errorf("run %s: %w", name, err)
		}
	}
	return nil
}

// Off reports whether no limit is set at all (per-run scopes disabled).
func (l RunScopeLimits) Off() bool {
	_, a, _ := ParseLimit(l.MemoryMax, 1)
	_, b, _ := ParseLimit(l.MemoryHigh, 1)
	return !a && !b
}

// Resolve converts the specs against the machine's RAM and swap (bytes).
func (l RunScopeLimits) Resolve(memTotal, swapTotal uint64) (RunScope, error) {
	var sc RunScope
	var err error
	if sc.MemoryMax, _, err = ParseLimit(l.MemoryMax, memTotal); err != nil {
		return sc, err
	}
	if sc.MemoryHigh, _, err = ParseLimit(l.MemoryHigh, memTotal); err != nil {
		return sc, err
	}
	var set bool
	if sc.SwapMax, set, err = ParseLimit(l.SwapMax, swapTotal); err != nil {
		return sc, err
	}
	sc.SwapZero = set && sc.SwapMax == 0
	return sc, nil
}

// Describe is a one-line summary ("MemoryMax=19.5G MemorySwapMax=2.0G").
func (sc RunScope) Describe() string {
	var parts []string
	if sc.MemoryMax > 0 {
		parts = append(parts, "MemoryMax="+HumanBytes(sc.MemoryMax))
	}
	if sc.MemoryHigh > 0 {
		parts = append(parts, "MemoryHigh="+HumanBytes(sc.MemoryHigh))
	}
	if sc.SwapMax > 0 || sc.SwapZero {
		parts = append(parts, "MemorySwapMax="+HumanBytes(sc.SwapMax))
	}
	if sc.OOMPolicy {
		parts = append(parts, "OOMPolicy=continue")
	}
	return strings.Join(parts, " ")
}

// HumanBytes formats bytes as "19.5G", "512M".
func HumanBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return strconv.FormatFloat(float64(b)/(1<<30), 'f', 1, 64) + "G"
	case b >= 1<<20:
		return strconv.FormatUint(b>>20, 10) + "M"
	default:
		return strconv.FormatUint(b, 10)
	}
}

// Properties are the StartTransientUnit properties of a run scope for pid
// (busctl argument form, after the property count).
func (sc RunScope) Properties(pid int, desc string) [][]string {
	props := [][]string{
		{"PIDs", "au", "1", strconv.Itoa(pid)},
		{"Slice", "s", sc.Slice},
		{"Description", "s", desc},
		{"CollectMode", "s", "inactive-or-failed"},
	}
	if sc.MemoryMax > 0 {
		props = append(props, []string{"MemoryMax", "t", strconv.FormatUint(sc.MemoryMax, 10)})
	}
	if sc.MemoryHigh > 0 {
		props = append(props, []string{"MemoryHigh", "t", strconv.FormatUint(sc.MemoryHigh, 10)})
	}
	if sc.SwapMax > 0 || sc.SwapZero {
		props = append(props, []string{"MemorySwapMax", "t", strconv.FormatUint(sc.SwapMax, 10)})
	}
	if sc.OOMPolicy {
		props = append(props, []string{"OOMPolicy", "s", "continue"})
	}
	return props
}

// StartArgs is the busctl argv that starts unit holding pid.
func (sc RunScope) StartArgs(unit string, pid int, desc string) []string {
	props := sc.Properties(pid, desc)
	args := []string{sc.Busctl, "--user", "--quiet", "--timeout=5", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
		"org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))", unit, "fail", strconv.Itoa(len(props))}
	for _, p := range props {
		args = append(args, p...)
	}
	return append(args, "0")
}

// RunScopeUnit is the scope name for a pane of session name: unique per
// launch, since a respawned pane may start before systemd has collected
// the previous scope.
func RunScopeUnit(name string, now time.Time) string {
	return "pyre-run-" + name + "-" + strconv.FormatInt(now.UnixNano()%(1<<40), 36) + ".scope"
}

// Test seams.
var (
	readMeminfo   = func() ([]byte, error) { return os.ReadFile("/proc/meminfo") }
	busctlVersion = func(ctx context.Context, busctl string) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, busctl, "--user", "--timeout=5", "get-property", "org.freedesktop.systemd1",
			"/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", "Version").Output()
		return string(out), err
	}
	// memoryDelegated reports whether the user manager's cgroup subtree
	// has the memory controller (unknown counts as yes).
	memoryDelegated = func() bool {
		ok, cg := UserServiceCgroup()
		if !ok {
			cg = ProcCgroup(os.Getpid())
		}
		i := strings.Index(cg, ".service/")
		j := strings.Index(cg, "/user@")
		if j < 0 || i < j {
			return true
		}
		b, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", cg[:i+len(".service")], "cgroup.subtree_control"))
		if err != nil {
			return true
		}
		return strings.Contains(" "+strings.TrimSpace(string(b))+" ", " memory ")
	}
)

var (
	runScopeMu    sync.Mutex
	runScopeCache struct {
		key string
		d   RunScopeDecision
		at  time.Time
	}
)

// DecideRunScope decides whether runs get their own scopes: only where the
// run server itself is placed in a scope (DecideScope: a systemd user
// service, or PYRE_SYSTEMD_SCOPE=on), with limits set, busctl installed,
// the user manager reachable on the user bus and the memory controller
// delegated. The answer is cached (a failure is retried after 30 s). The
// probe runs outside the cache lock and ignores the caller's cancellation
// (a cancelled caller must not cache "not reachable").
func DecideRunScope(ctx context.Context, l RunScopeLimits) RunScopeDecision {
	if l.Off() {
		return RunScopeDecision{Reason: "per-run limits off"}
	}
	if d, ok := CachedRunScope(l); ok {
		return d
	}
	d := decideRunScope(context.WithoutCancel(ctx), l)
	runScopeMu.Lock()
	runScopeCache.key, runScopeCache.d, runScopeCache.at = runScopeKey(l), d, time.Now()
	runScopeMu.Unlock()
	return d
}

func runScopeKey(l RunScopeLimits) string { return l.MemoryMax + "|" + l.MemoryHigh + "|" + l.SwapMax }

// CachedRunScope returns the cached decision for l without probing (ok
// false when there is none, or a failure is due for a retry). Status
// reports use it so they never wait on the user bus.
func CachedRunScope(l RunScopeLimits) (RunScopeDecision, bool) {
	if l.Off() {
		return RunScopeDecision{Reason: "per-run limits off"}, true
	}
	runScopeMu.Lock()
	defer runScopeMu.Unlock()
	if c := runScopeCache; c.key == runScopeKey(l) && !c.at.IsZero() && (c.d.Use || time.Since(c.at) < probeRetry) {
		return c.d, true
	}
	return RunScopeDecision{}, false
}

func decideRunScope(ctx context.Context, l RunScopeLimits) RunScopeDecision {
	sd := DecideScope(ctx)
	if !sd.Use {
		return RunScopeDecision{Reason: "no run-server scope (" + sd.Reason + ")"}
	}
	busctl, err := lookPath("busctl")
	if err != nil {
		return RunScopeDecision{Reason: "busctl not found"}
	}
	out, err := busctlVersion(ctx, busctl)
	if err != nil {
		return RunScopeDecision{Reason: fmt.Sprintf("systemd user manager not reachable on the user bus: %v", err)}
	}
	if !memoryDelegated() {
		return RunScopeDecision{Reason: "the memory controller is not delegated to the systemd user manager"}
	}
	b, err := readMeminfo()
	if err != nil {
		return RunScopeDecision{Reason: "read /proc/meminfo: " + err.Error()}
	}
	mem, swap := meminfoTotals(b)
	sc, err := l.Resolve(mem, swap)
	if err != nil {
		return RunScopeDecision{Reason: err.Error()}
	}
	sc.Busctl, sc.Slice = busctl, sd.Slice
	sc.OOMPolicy = systemdVersion(out) >= 253
	return RunScopeDecision{Use: true, Scope: sc, Reason: "per-run scopes in " + sd.Slice + ": " + sc.Describe()}
}

// meminfoTotals returns MemTotal and SwapTotal in bytes.
func meminfoTotals(b []byte) (mem, swap uint64) {
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		n, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			continue
		}
		switch k {
		case "MemTotal":
			mem = n << 10
		case "SwapTotal":
			swap = n << 10
		}
	}
	return mem, swap
}

// systemdVersion parses busctl's `s "259.5-0ubuntu3"` (0 when unknown).
func systemdVersion(out string) int {
	s := strings.TrimSpace(out)
	s = strings.TrimPrefix(s, "s ")
	s = strings.Trim(s, `"`)
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	n, _ := strconv.Atoi(s[:end])
	return n
}
