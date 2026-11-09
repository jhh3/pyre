//go:build linux

package memguard

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/jhh3/pyre/internal/sysstats"
)

// Supported reports whether the guard can run on this platform.
func Supported() bool { return true }

// procOS reads a /proc tree (Root: "/proc", or a fixture in tests).
type procOS struct {
	Root     string
	UID      int
	PageSize int64
	mem      *sysstats.Sampler
}

// NewOS returns the machine's process and memory view.
func NewOS() OS { return NewProcOS("/proc") }

// NewProcOS reads processes and memory from root (tests: a fake /proc).
func NewProcOS(root string) OS {
	s := sysstats.NewSampler()
	s.ProcRoot = root
	return &procOS{Root: root, UID: os.Getuid(), PageSize: int64(os.Getpagesize()), mem: s}
}

func selfPID() int { return os.Getpid() }

func (o *procOS) Memory(ctx context.Context) sysstats.Host { return o.mem.Memory(ctx) }

// Table lists this user's processes (stat, plus VmSwap from status).
func (o *procOS) Table(ctx context.Context) (map[int]Proc, error) {
	ents, err := os.ReadDir(o.Root)
	if err != nil {
		return nil, err
	}
	m := make(map[int]Proc, len(ents))
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if p, ok := o.read(pid); ok {
			m[pid] = p
		}
	}
	return m, nil
}

func (o *procOS) read(pid int) (Proc, bool) {
	dir := filepath.Join(o.Root, strconv.Itoa(pid))
	st, err := os.Stat(dir)
	if err != nil {
		return Proc{}, false
	}
	if s, ok := st.Sys().(*syscall.Stat_t); !ok || int(s.Uid) != o.UID {
		return Proc{}, false
	}
	b, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return Proc{}, false
	}
	p, err := parseStat(b, o.PageSize)
	if err != nil || p.PID != pid {
		return Proc{}, false
	}
	if sb, err := os.ReadFile(filepath.Join(dir, "status")); err == nil {
		if rss, ok := statusKB(sb, "VmRSS"); ok {
			p.RSS = rss << 10
		}
		if sw, ok := statusKB(sb, "VmSwap"); ok {
			p.Swap = sw << 10
		}
	}
	return p, true
}

// parseStat parses /proc/<pid>/stat: after the last ')' come state(0)
// ppid(1) ... starttime(19) vsize(20) rss(21, pages).
func parseStat(b []byte, pageSize int64) (Proc, error) {
	open := bytes.IndexByte(b, '(')
	end := bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return Proc{}, errors.New("malformed stat")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b[:open])))
	if err != nil {
		return Proc{}, err
	}
	f := strings.Fields(string(b[end+1:]))
	if len(f) < 22 {
		return Proc{}, errors.New("short stat")
	}
	ppid, _ := strconv.Atoi(f[1])
	start, _ := strconv.ParseUint(f[19], 10, 64)
	rss, _ := strconv.ParseInt(f[21], 10, 64)
	return Proc{PID: pid, PPID: ppid, Name: string(b[open+1 : end]), Start: start, RSS: rss * pageSize, Zombie: f[0] == "Z" || f[0] == "X"}, nil
}

func statusKB(b []byte, key string) (int64, bool) {
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok || k != key {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			return 0, false
		}
		n, err := strconv.ParseInt(f[0], 10, 64)
		return n, err == nil
	}
	return 0, false
}

// RunEnv reads PYRE_RUN_ID from pid's own environment.
func (o *procOS) RunEnv(pid int) string {
	env, err := os.ReadFile(filepath.Join(o.Root, strconv.Itoa(pid), "environ"))
	if err != nil {
		return ""
	}
	for _, kv := range bytes.Split(env, []byte{0}) {
		if v, ok := bytes.CutPrefix(kv, []byte("PYRE_RUN_ID=")); ok {
			return string(v)
		}
	}
	return ""
}

// Lookup re-reads p; a zombie (exited, not yet reaped) counts as gone.
func (o *procOS) Lookup(p Proc) (Proc, bool) {
	cur, ok := o.read(p.PID)
	if !ok || cur.Start != p.Start || cur.Zombie {
		return Proc{}, false
	}
	return cur, true
}

// Signal opens a pidfd, re-checks the start time through /proc, and
// signals through the pidfd, so a PID reused after the check is never hit.
func (o *procOS) Signal(p Proc, sig syscall.Signal) error {
	if o.Root != "/proc" {
		return errors.New("memguard: signals only on the real /proc")
	}
	fd, err := unix.PidfdOpen(p.PID, 0)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return errors.New("process exited")
		}
		if !errors.Is(err, unix.ENOSYS) {
			return err
		}
		// Kernel < 5.3: check, then kill (a small reuse window).
		if _, ok := o.Lookup(p); !ok {
			return errors.New("process exited")
		}
		return syscall.Kill(p.PID, sig)
	}
	defer unix.Close(fd)
	if _, ok := o.Lookup(p); !ok {
		return errors.New("process exited")
	}
	return unix.PidfdSendSignal(fd, sig, nil, 0)
}

// Describe reads the command line and, from the process or its nearest
// ancestor that has them, PYRE_RUN_ID and PYRE_SESSION_ID.
func (o *procOS) Describe(p Proc) (cmd, runID, sessionID string) {
	dir := filepath.Join(o.Root, strconv.Itoa(p.PID))
	if b, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
		cmd = strings.TrimSpace(strings.ReplaceAll(string(bytes.TrimRight(b, "\x00")), "\x00", " "))
	}
	if len(cmd) > 512 {
		cmd = cmd[:512]
	}
	pid := p.PID
	for i := 0; i < 64 && pid > 1; i++ {
		d := filepath.Join(o.Root, strconv.Itoa(pid))
		if env, err := os.ReadFile(filepath.Join(d, "environ")); err == nil {
			for _, kv := range bytes.Split(env, []byte{0}) {
				if v, ok := bytes.CutPrefix(kv, []byte("PYRE_RUN_ID=")); ok {
					runID = string(v)
				} else if v, ok := bytes.CutPrefix(kv, []byte("PYRE_SESSION_ID=")); ok {
					sessionID = string(v)
				}
			}
			if sessionID != "" {
				return cmd, runID, sessionID
			}
			runID = ""
		}
		q, ok := o.read(pid)
		if !ok {
			break
		}
		pid = q.PPID
	}
	return cmd, "", ""
}
