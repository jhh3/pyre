package codex

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// groupMembers returns the live members of each wanted process group,
// from /proc. ok is false when /proc cannot be read.
func groupMembers(pgids []int) (map[int][]procMember, bool) {
	want := wantGroups(pgids)
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false
	}
	res := map[int][]procMember{}
	if len(want) == 0 {
		return res, true
	}
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		dir := filepath.Join("/proc", e.Name())
		b, err := os.ReadFile(filepath.Join(dir, "stat"))
		if err != nil {
			continue // exited meanwhile
		}
		st, pg, ok := parseProcStat(b)
		if !ok || !want[pg] || st == 'Z' || st == 'X' {
			continue
		}
		pid, _ := strconv.Atoi(e.Name())
		cl, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
		cl = bytes.ReplaceAll(bytes.TrimRight(cl, "\x00"), []byte{0}, []byte{' '})
		res[pg] = append(res[pg], procMember{PID: pid, Cmd: clipCmd(string(cl))})
	}
	return res, true
}

// parseProcStat returns the state and process group of a Linux
// /proc/<pid>/stat line ("pid (comm) S ppid pgrp ..."; comm may hold
// spaces and parentheses, so fields are counted after the last ')').
func parseProcStat(b []byte) (state byte, pgrp int, ok bool) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, 0, false
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 3 || len(f[0]) == 0 {
		return 0, 0, false
	}
	pg, err := strconv.Atoi(f[2])
	if err != nil {
		return 0, 0, false
	}
	return f[0][0], pg, true
}
