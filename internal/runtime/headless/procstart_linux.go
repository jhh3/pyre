package headless

import (
	"os"
	"strconv"
	"strings"
)

// procStart is a process's start time (clock ticks since boot, field 22 of
// /proc/<pid>/stat), "" when unknown: a pid recorded with its start time is
// never mistaken for a later process that reused the number.
func procStart(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')') // the command name may hold spaces
	if i < 0 {
		return ""
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return ""
	}
	return f[19] // field 22 overall; fields after ")" start at field 3
}
