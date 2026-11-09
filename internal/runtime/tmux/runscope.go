package tmux

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jhh3/pyre/internal/supervisor"
)

// Per-run memory scopes (Linux; supervisor/runscope.go, decision I66).
//
// When pyred configures limits (SetRunScopeLimits) and the user manager can
// take them (supervisor.DecideRunScope), every pane this package starts
// runs through the launcher with scope records in its launch file: the
// launcher moves its own PID into a new transient scope with those limits
// (joinRunScope) and then execs the program in place. It fails open: the
// program runs either way, and a launch that could not be scoped is logged
// to <socket dir>/RunScopeLog.

// RunScopeLog is the launcher's log of scope placements, next to the socket.
const RunScopeLog = "run-scopes.log"

// maxRunScopeLog bounds RunScopeLog; a longer log is started afresh.
const maxRunScopeLog = 256 << 10

var runScopeLimits atomic.Pointer[supervisor.RunScopeLimits]

// SetRunScopeLimits configures per-run scopes for every Server in this
// process (pyred sets it once at start-up; nil turns them off).
func SetRunScopeLimits(l *supervisor.RunScopeLimits) { runScopeLimits.Store(l) }

// decideRunScope is replaced by tests.
var decideRunScope = supervisor.DecideRunScope

// runScope returns the scope a new pane gets, or nil.
func (s *Server) runScope(ctx context.Context) *supervisor.RunScope {
	l := runScopeLimits.Load()
	if l == nil || runtime.GOOS != "linux" {
		return nil
	}
	d := decideRunScope(ctx, *l)
	if !d.Use {
		return nil
	}
	sc := d.Scope
	return &sc
}

// Launch-file scope records: 'G' the unit name and the session name
// (tab-separated), 'K' key=value fields of the scope (encodeScope /
// decodeScopeField).
func encodeScope(unit, name string, sc *supervisor.RunScope) []byte {
	if sc == nil {
		return nil
	}
	var b bytes.Buffer
	rec := func(t byte, v string) {
		b.WriteByte(t)
		b.WriteString(v)
		b.WriteByte(0)
	}
	rec('G', unit+"\t"+name)
	kv := func(k, v string) { rec('K', k+"="+v) }
	kv("busctl", sc.Busctl)
	kv("slice", sc.Slice)
	kv("max", strconv.FormatUint(sc.MemoryMax, 10))
	kv("high", strconv.FormatUint(sc.MemoryHigh, 10))
	kv("swap", strconv.FormatUint(sc.SwapMax, 10))
	if sc.SwapZero {
		kv("swapzero", "1")
	}
	if sc.OOMPolicy {
		kv("oom", "1")
	}
	return b.Bytes()
}

func decodeScopeField(sc *supervisor.RunScope, body string) error {
	k, v, ok := strings.Cut(body, "=")
	if !ok {
		return fmt.Errorf("bad scope record")
	}
	num := func() (uint64, error) { return strconv.ParseUint(v, 10, 64) }
	var err error
	switch k {
	case "busctl":
		sc.Busctl = v
	case "slice":
		sc.Slice = v
	case "max":
		sc.MemoryMax, err = num()
	case "high":
		sc.MemoryHigh, err = num()
	case "swap":
		sc.SwapMax, err = num()
	case "swapzero":
		sc.SwapZero = v == "1"
	case "oom":
		sc.OOMPolicy = v == "1"
	default:
		return fmt.Errorf("bad scope field %q", k)
	}
	return err
}

// joinRunScopeTimeout bounds the StartTransientUnit call and the wait for
// the move (a launch never waits longer than this for its scope; tests
// shorten it).
var joinRunScopeTimeout = 5 * time.Second

// runBusctl runs one busctl call (replaced by tests).
var runBusctl = func(ctx context.Context, argv []string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var errb bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &errb, n: 4 << 10}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// selfCgroup reads this process's cgroup (replaced by tests).
var selfCgroup = func() string { return supervisor.ProcCgroup(os.Getpid()) }

// joinRunScope moves this process into a new scope (the launcher, before it
// execs the pane's program) and waits until the move is visible. On a user
// manager that rejects OOMPolicy= on scopes it retries without it. The
// returned note says what happened, for the log.
func joinRunScope(unit, desc string, sc supervisor.RunScope) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), joinRunScopeTimeout)
	defer cancel()
	pid := os.Getpid()
	err := runBusctl(ctx, sc.StartArgs(unit, pid, desc))
	note := ""
	if err != nil && sc.OOMPolicy && ctx.Err() == nil {
		sc.OOMPolicy = false
		if err2 := runBusctl(ctx, sc.StartArgs(unit, pid, desc)); err2 == nil {
			err, note = nil, " (without OOMPolicy=continue: "+oneLineErr(err)+")"
		}
	}
	if err != nil {
		return "", err
	}
	for {
		if strings.HasSuffix(selfCgroup(), "/"+unit) {
			return "scope " + unit + " " + sc.Describe() + note, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("scope %s started but this process did not move into it within %s", unit, joinRunScopeTimeout)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func oneLineErr(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// logRunScope appends one line to dir/RunScopeLog (best effort, private).
func logRunScope(dir, line string) {
	p := filepath.Join(dir, RunScopeLog)
	if st, err := os.Lstat(p); err == nil && (st.Size() > maxRunScopeLog || !st.Mode().IsRegular()) {
		if !st.Mode().IsRegular() {
			return
		}
		_ = os.Remove(p)
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(time.Now().UTC().Format(time.RFC3339) + " " + line + "\n")
}
