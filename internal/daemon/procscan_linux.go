package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func ownedBy(st os.FileInfo, uid int) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(s.Uid) == uid
}

// processCwds lists the working directories of this user's processes
// (W-lifecycle: a worktree a process works in is never removed).
func processCwds() []string {
	var out []string
	forEachProc(func(pid int, dir string) {
		if cwd, err := os.Readlink(filepath.Join(dir, "cwd")); err == nil {
			out = append(out, cwd)
		}
	})
	return out
}

// openPathsUnder lists what this user's processes hold under roots: open
// files, working directories and executables (/proc/<pid>/{fd/*,cwd,exe}).
// ok is false when /proc cannot be read.
func openPathsUnder(roots []string) (paths []string, ok bool) {
	if _, err := os.ReadDir("/proc"); err != nil {
		return nil, false
	}
	add := func(link string) {
		if p, err := os.Readlink(link); err == nil && underAny(p, roots) {
			paths = append(paths, strings.TrimSuffix(p, " (deleted)"))
		}
	}
	forEachProc(func(pid int, dir string) {
		add(filepath.Join(dir, "cwd"))
		add(filepath.Join(dir, "exe"))
		fds, err := os.ReadDir(filepath.Join(dir, "fd"))
		if err != nil {
			return
		}
		for _, fd := range fds {
			add(filepath.Join(dir, "fd", fd.Name()))
		}
	})
	return paths, true
}

// sessionProcesses lists processes still running in one of the session's
// terminal sessions (sids: the pane process IDs its runs were launched as,
// each a session leader) whose environment names sessionID as
// PYRE_SESSION_ID: what its runs left running. Daemons a run started (tmux
// server, adb, gradle, language servers) detach into their own terminal
// session and are never matched.
func sessionProcesses(sessionID string, sids []int) []int {
	want := []byte("PYRE_SESSION_ID=" + sessionID)
	self := os.Getpid()
	var pids []int
	forEachProc(func(pid int, dir string) {
		if pid == self || !inSessions(pid, sids) {
			return
		}
		env, err := os.ReadFile(filepath.Join(dir, "environ"))
		if err != nil {
			return
		}
		for _, kv := range bytes.Split(env, []byte{0}) {
			if bytes.Equal(kv, want) {
				pids = append(pids, pid)
				return
			}
		}
	})
	return pids
}

// leftRunning lists, per session in scan, this user's live processes the
// session started: their own environment names it as PYRE_SESSION_ID, or
// their cgroup is the scope of one of its runs (pyre-run-<run_id>-<suffix>
// .scope, supervisor runscope.go; a setsid -f job stays in it). Processes
// started after the session's leftScan.before (its run's exit path, such
// as a SessionEnd hook's detached helper), in skip (just stopped), zombies
// and pyred itself are left out. One pass over /proc for all sessions; at
// most contracts.LeftRunningMax listed per session, all counted. Nothing
// is signalled.
func leftRunning(scan map[string]leftScan, skip map[int]bool) map[string]leftProcs {
	if len(scan) == 0 {
		return nil
	}
	byRun := map[string]string{}
	for sid, s := range scan {
		for _, id := range s.runs {
			byRun[id] = sid
		}
	}
	clock := newProcClock()
	self := os.Getpid()
	out := map[string]leftProcs{}
	forEachProc(func(pid int, dir string) {
		if pid == self || skip[pid] || procZombie(dir) {
			return
		}
		sid := ""
		if env, err := os.ReadFile(filepath.Join(dir, "environ")); err == nil {
			for _, kv := range bytes.Split(env, []byte{0}) {
				if v, ok := bytes.CutPrefix(kv, []byte("PYRE_SESSION_ID=")); ok {
					if _, want := scan[string(v)]; want {
						sid = string(v)
					}
					break
				}
			}
		}
		if sid == "" {
			if cg, err := os.ReadFile(filepath.Join(dir, "cgroup")); err == nil {
				sid = byRun[runScopeRun(string(cg))]
			}
		}
		if sid == "" || clock.startedAfter(dir, scan[sid].before) {
			return
		}
		lp := out[sid]
		lp.count++
		if len(lp.procs) < contracts.LeftRunningMax {
			lp.procs = append(lp.procs, contracts.LeftProcess{PID: pid, Command: procCommand(dir)})
		}
		out[sid] = lp
	})
	return out
}

// clockTicks is USER_HZ, the unit of /proc/<pid>/stat starttime (100 on
// every Linux architecture Pyre ships for).
const clockTicks = 100

// procClock maps wall-clock times onto the boot-relative clock of
// /proc/<pid>/stat starttime: /proc/uptime (CLOCK_BOOTTIME, 10 ms) read
// once, against the wall clock at that moment. Exact to about 10 ms, where
// /proc/stat btime is whole seconds.
type procClock struct {
	wall   time.Time
	uptime time.Duration
	ok     bool
}

func newProcClock() procClock {
	b, err := os.ReadFile("/proc/uptime")
	now := time.Now()
	if err != nil {
		return procClock{}
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return procClock{}
	}
	sec, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return procClock{}
	}
	return procClock{wall: now, uptime: time.Duration(sec * float64(time.Second)), ok: true}
}

// startedAfter reports whether the process at dir started after t. It is
// false when t is zero or either time cannot be read (the process counts).
func (c procClock) startedAfter(dir string, t time.Time) bool {
	if t.IsZero() || !c.ok {
		return false
	}
	start, ok := procStartTicks(dir)
	if !ok {
		return false
	}
	at := c.uptime - c.wall.Sub(t) // t on the boot clock
	return time.Duration(start)*time.Second/clockTicks > at
}

// procStartTicks is the process's starttime (/proc/<pid>/stat field 22:
// clock ticks after boot).
func procStartTicks(dir string) (uint64, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return 0, false
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(string(b[i+1:])) // f[0] is field 3 (state)
	if len(f) < 20 {
		return 0, false
	}
	v, err := strconv.ParseUint(f[19], 10, 64)
	return v, err == nil
}

// runScopeRun is the run ID of the per-run scope a /proc/<pid>/cgroup file
// names ("" when none): pyre-run-<run_id>-<suffix>.scope.
func runScopeRun(cgroup string) string {
	const pre = "/pyre-run-"
	i := strings.LastIndex(cgroup, pre)
	if i < 0 {
		return ""
	}
	unit := cgroup[i+len(pre):]
	if j := strings.IndexAny(unit, "/\n"); j >= 0 {
		unit = unit[:j]
	}
	unit, ok := strings.CutSuffix(unit, ".scope")
	if !ok {
		return ""
	}
	if k := strings.LastIndexByte(unit, '-'); k > 0 {
		return unit[:k]
	}
	return ""
}

// procZombie: the process exited and was not reaped yet.
func procZombie(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return true // gone
	}
	i := bytes.LastIndexByte(b, ')')
	return i < 0 || i+2 >= len(b) || b[i+2] == 'Z' || b[i+2] == 'X'
}

// procCommand is the process's command line, redacted and clipped.
func procCommand(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return ""
	}
	return leftCommand(strings.ReplaceAll(string(bytes.TrimRight(b, "\x00")), "\x00", " "))
}

// getsid reads pid's terminal session ID from /proc/<pid>/stat (the field
// after state, ppid and pgrp; the command name before them may hold spaces).
func getsid(pid int) (int, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, os.ErrInvalid
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 4 {
		return 0, os.ErrInvalid
	}
	return strconv.Atoi(f[3])
}

func forEachProc(fn func(pid int, dir string)) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	uid := os.Getuid()
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join("/proc", e.Name())
		if st, err := os.Stat(dir); err != nil || !ownedBy(st, uid) {
			continue
		}
		fn(pid, dir)
	}
}

// leftCommandMax bounds a listed command line (runes).
const leftCommandMax = 80

// leftCommand is a command line as listed in LeftRunning: redacted like
// the memory guard's (it reaches the parent session) and clipped.
func leftCommand(cmd string) string {
	cmd = guardRedact(strings.Join(strings.Fields(cmd), " "))
	if r := []rune(cmd); len(r) > leftCommandMax {
		cmd = string(r[:leftCommandMax]) + "…"
	}
	return cmd
}
