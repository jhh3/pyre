package supervisor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Systemd transient scopes for the Pyre tmux server (Linux).
//
// Under a systemd user service the Pyre tmux server (and so every pane:
// agents, shells, Codex app-servers) is started in its own transient scope
// inside a dedicated slice:
//
//	systemd-run --user --scope --quiet --collect --slice=pyre-runs.slice \
//	    --unit=pyre-tmux-<hash>.scope -- tmux -S <sock> -f <conf> start-server
//
// Restarting or stopping pyred.service then signals only pyred's own cgroup,
// so pyred can use the default KillMode=control-group and modest
// MemoryMax/CPUQuota without affecting runs; the slice carries the limits
// that protect the host from runs. A scope changes cgroup placement only:
// the tmux server is still forked from pyred and inherits its environment,
// rlimits, NoNewPrivileges flag and mount namespace, so pyred's unit must not
// use namespace sandboxing or NoNewPrivileges.
//
// PYRE_SYSTEMD_SCOPE selects the behaviour:
//
//	auto (default)  use a scope when running as a systemd user service and
//	                systemd-run works; otherwise start tmux directly
//	require         as auto, but fail instead of falling back (set by the
//	                installed pyred.service, whose KillMode would otherwise
//	                kill runs on restart)
//	on              try a scope even outside a systemd service; fall back
//	off             never use a scope (legacy KillMode=process units)
//
// PYRE_SYSTEMD_SLICE overrides the slice (default pyre-runs.slice).

// Scope modes.
const (
	ScopeAuto    = "auto"
	ScopeRequire = "require"
	ScopeOn      = "on"
	ScopeOff     = "off"
)

// DefaultRunsSlice is the slice that holds the Pyre tmux server scopes.
const DefaultRunsSlice = "pyre-runs.slice"

// ScopeDecision says whether and how to start a server in a scope.
type ScopeDecision struct {
	Use        bool   // start in a transient scope
	Required   bool   // failing to do so is an error (mode require)
	SystemdRun string // systemd-run executable
	Slice      string
	Reason     string // why a scope is (not) used
}

// ScopeEnv is the environment lookup used by DecideScope (tests replace it).
var ScopeEnv = os.Getenv

// procSelfCgroup is read to detect a systemd user service (tests replace it).
var procSelfCgroup = func() ([]byte, error) { return os.ReadFile("/proc/self/cgroup") }

// probeScope runs a no-op command in a transient scope (tests replace it).
var probeScope = func(ctx context.Context, systemdRun, slice string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, systemdRun, "--user", "--scope", "--quiet", "--collect", "--slice="+slice, "--", "true")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(errb.String()))
	}
	return nil
}

var lookPath = exec.LookPath

// ScopeMode returns the normalized PYRE_SYSTEMD_SCOPE value.
func ScopeMode() string {
	switch strings.ToLower(strings.TrimSpace(ScopeEnv("PYRE_SYSTEMD_SCOPE"))) {
	case "", ScopeAuto:
		return ScopeAuto
	case ScopeRequire, "required":
		return ScopeRequire
	case ScopeOn, "1", "true", "yes":
		return ScopeOn
	case ScopeOff, "0", "false", "no":
		return ScopeOff
	default:
		return ScopeAuto
	}
}

// UserServiceCgroup reports whether this process runs inside a unit of a
// systemd user manager (user@UID.service) and returns its cgroup path.
func UserServiceCgroup() (bool, string) {
	if runtime.GOOS != "linux" || ScopeEnv("INVOCATION_ID") == "" {
		return false, ""
	}
	b, err := procSelfCgroup()
	if err != nil {
		return false, ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		// cgroup v2: "0::/user.slice/user-1000.slice/user@1000.service/app.slice/pyred.service"
		_, path, ok := strings.Cut(l, "::")
		if !ok {
			continue
		}
		if strings.Contains(path, "/user@") && strings.Contains(path, ".service/") {
			return true, path
		}
	}
	return false, ""
}

type probeKey struct{ bin, slice string }

type probeResult struct {
	err error
	at  time.Time
}

// A failed probe is retried after this long (the user bus may come up after
// pyred at boot); a successful one is kept for the process lifetime.
const probeRetry = 30 * time.Second

var (
	probeMu    sync.Mutex
	probeCache = map[probeKey]probeResult{}
)

// DecideScope decides how to start a tmux server. The probe result is
// cached per process.
func DecideScope(ctx context.Context) ScopeDecision {
	mode := ScopeMode()
	slice := strings.TrimSpace(ScopeEnv("PYRE_SYSTEMD_SLICE"))
	if slice == "" {
		slice = DefaultRunsSlice
	}
	d := ScopeDecision{Slice: slice, Required: mode == ScopeRequire}
	if mode == ScopeOff {
		d.Required = false
		d.Reason = "PYRE_SYSTEMD_SCOPE=off"
		return d
	}
	if runtime.GOOS != "linux" {
		d.Required = false
		d.Reason = "not Linux"
		return d
	}
	if mode == ScopeAuto {
		if ok, _ := UserServiceCgroup(); !ok {
			d.Reason = "not running as a systemd user service"
			return d
		}
	}
	bin, err := lookPath("systemd-run")
	if err != nil {
		d.Reason = "systemd-run not found"
		return d
	}
	d.SystemdRun = bin
	k := probeKey{bin, slice}
	probeMu.Lock()
	r, done := probeCache[k]
	if !done || (r.err != nil && time.Since(r.at) > probeRetry) {
		r = probeResult{err: probeScope(ctx, bin, slice), at: time.Now()}
		probeCache[k] = r
	}
	probeMu.Unlock()
	perr := r.err
	if perr != nil {
		d.Reason = "systemd-run --user --scope failed: " + perr.Error()
		return d
	}
	d.Use = true
	d.Reason = "transient scope in " + slice
	return d
}

// ScopeUnitName is the stable scope name for a tmux socket.
func ScopeUnitName(socket string) string {
	sum := sha256.Sum256([]byte(socket))
	return "pyre-tmux-" + hex.EncodeToString(sum[:])[:12] + ".scope"
}

// Wrap returns argv prefixed with the systemd-run scope invocation.
func (d ScopeDecision) Wrap(unit string, argv []string) []string {
	out := []string{d.SystemdRun, "--user", "--scope", "--quiet", "--collect", "--slice=" + d.Slice, "--unit=" + unit, "--"}
	return append(out, argv...)
}

// UnitExists reports whether systemd-run failed because the unit name is
// taken (for example by a scope still holding orphaned panes).
func UnitExists(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "already exists") || strings.Contains(s, "already loaded")
}

// ProcCgroup returns the cgroup v2 path of pid ("" when unknown, e.g. on
// macOS).
func ProcCgroup(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if p, ok := strings.CutPrefix(l, "0::"); ok {
			return p
		}
	}
	return ""
}

// PlacementFile is written next to the tmux socket and records where the
// server was started (diagnostics; no secrets).
const PlacementFile = "placement"

// RecordPlacement writes one line describing the server placement.
func RecordPlacement(socket, line string) {
	p := filepath.Join(filepath.Dir(socket), PlacementFile)
	_ = os.WriteFile(p, []byte(time.Now().UTC().Format(time.RFC3339)+" "+line+"\n"), 0o600)
}

// ReadPlacement returns the recorded placement line, or "".
func ReadPlacement(socket string) string {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(socket), PlacementFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
