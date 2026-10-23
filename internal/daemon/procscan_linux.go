package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
