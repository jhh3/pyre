//go:build linux

package tmux

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// groupAlive reports whether process group pgid has a member that is not a
// zombie. kill(-pgid, 0) also succeeds for zombies, which stay until their
// parent (or, for orphans, the reaper) collects them; on a CI runner that
// took longer than TerminateSession waits. /proc tells the states apart.
func groupAlive(pgid int) bool {
	if syscall.Kill(-pgid, 0) != nil {
		return false
	}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return true
	}
	want := strconv.Itoa(pgid)
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// pid (comm) state ppid pgrp ...; comm may contain spaces and ")".
		i := bytes.LastIndexByte(b, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(string(b[i+1:]))
		if len(f) >= 3 && f[2] == want && f[0] != "Z" && f[0] != "X" {
			return true
		}
	}
	return false
}
