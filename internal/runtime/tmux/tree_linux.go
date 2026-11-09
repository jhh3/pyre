//go:build linux

package tmux

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// platformListProcs reads /proc/<pid>/stat for every process.
func platformListProcs() ([]procInfo, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make([]procInfo, 0, len(ents))
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		if p, ok := parseProcStat(pid, b); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// parseProcStat parses "pid (comm) state ppid pgrp session ... starttime"
// (comm may hold spaces and ")"; starttime is field 22).
func parseProcStat(pid int, b []byte) (procInfo, bool) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return procInfo{}, false
	}
	f := strings.Fields(string(b[i+1:])) // f[0] is field 3 (state)
	if len(f) < 20 {
		return procInfo{}, false
	}
	ppid, e1 := strconv.Atoi(f[1])
	pgid, e2 := strconv.Atoi(f[2])
	sid, e3 := strconv.Atoi(f[3])
	if e1 != nil || e2 != nil || e3 != nil {
		return procInfo{}, false
	}
	return procInfo{pid: pid, ppid: ppid, pgid: pgid, sid: sid, start: f[19], zombie: f[0] == "Z" || f[0] == "X"}, true
}

// platformProcCgroup is the process's cgroup v2 path ("" unknown).
func platformProcCgroup(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
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

func platformSignal(pid int, force bool) error {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(pid, sig)
}
