//go:build darwin

package memguard

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jhh3/pyre/internal/sysstats"
)

// Supported reports whether the guard can run on this platform.
func Supported() bool { return true }

// psOS reads the process table with ps (macOS has no /proc).
type psOS struct {
	mem *sysstats.Sampler
	uid int
}

// NewOS returns the machine's process and memory view.
func NewOS() OS { return &psOS{mem: sysstats.NewSampler(), uid: os.Getuid()} }

func selfPID() int { return os.Getpid() }

func (o *psOS) Memory(ctx context.Context) sysstats.Host { return o.mem.Memory(ctx) }

func ps(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C") // lstart in English (parsePS)
	return cmd.Output()
}

// Table lists this user's processes: pid, ppid, rss (KiB), start time
// (lstart, as a reuse guard) and the command name.
func (o *psOS) Table(ctx context.Context) (map[int]Proc, error) {
	out, err := ps(ctx, "-U", strconv.Itoa(o.uid), "-o", "pid=,ppid=,rss=,lstart=,comm=")
	if err != nil && len(out) == 0 {
		return nil, err
	}
	return parsePS(out), nil
}

func (o *psOS) Lookup(p Proc) (Proc, bool) {
	out, err := ps(context.Background(), "-p", strconv.Itoa(p.PID), "-o", "pid=,ppid=,rss=,lstart=,comm=")
	if err != nil && len(out) == 0 {
		return Proc{}, false
	}
	cur, ok := parsePS(out)[p.PID]
	if !ok || (p.Start != 0 && cur.Start != p.Start) {
		return Proc{}, false
	}
	return cur, true
}

// Signal re-checks the process (start time) and signals it; macOS has no
// pidfd, so a reuse between the check and the kill is possible but needs
// the PID space to wrap in that instant.
func (o *psOS) Signal(p Proc, sig syscall.Signal) error {
	if p.Start == 0 {
		return errors.New("process start time unknown; not signalled (a reused pid cannot be ruled out)")
	}
	if _, ok := o.Lookup(p); !ok {
		return errors.New("process exited")
	}
	return syscall.Kill(p.PID, sig)
}

// RunEnv reads PYRE_RUN_ID from pid's environment (`ps -E`).
func (o *psOS) RunEnv(pid int) string {
	out, err := ps(context.Background(), "-E", "-ww", "-p", strconv.Itoa(pid), "-o", "command=")
	if err != nil {
		return ""
	}
	for _, tok := range strings.Fields(string(out)) {
		if v, ok := strings.CutPrefix(tok, "PYRE_RUN_ID="); ok {
			return v
		}
	}
	return ""
}

// Describe reads the command line and PYRE_RUN_ID / PYRE_SESSION_ID from
// `ps -E` (environment after the command line).
func (o *psOS) Describe(p Proc) (cmd, runID, sessionID string) {
	if out, err := ps(context.Background(), "-ww", "-p", strconv.Itoa(p.PID), "-o", "command="); err == nil {
		cmd = strings.TrimSpace(string(out))
	}
	if len(cmd) > 512 {
		cmd = cmd[:512]
	}
	pid := p.PID
	for i := 0; i < 64 && pid > 1; i++ {
		out, err := ps(context.Background(), "-E", "-ww", "-p", strconv.Itoa(pid), "-o", "ppid=,command=")
		if err != nil || len(out) == 0 {
			break
		}
		f := strings.Fields(string(out))
		if len(f) == 0 {
			break
		}
		run, sess := "", ""
		for _, tok := range f[1:] {
			if v, ok := strings.CutPrefix(tok, "PYRE_RUN_ID="); ok {
				run = v
			} else if v, ok := strings.CutPrefix(tok, "PYRE_SESSION_ID="); ok {
				sess = v
			}
		}
		if sess != "" {
			return cmd, run, sess
		}
		next, err := strconv.Atoi(f[0])
		if err != nil {
			break
		}
		pid = next
	}
	return cmd, "", ""
}
