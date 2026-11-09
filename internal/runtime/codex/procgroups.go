package codex

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
)

// Process-group membership (decision I62): which processes are still in a
// process group, read from the process table. Codex kills a held exec's
// whole group (killpg) when its thread is cleaned or unloaded, so a group
// with live members is live work that kill would end.

// procMember is one live process in a group.
type procMember struct {
	PID int
	Cmd string // command line, clipped (for logs and the audit)
}

// memberCmdMax clips a member's command line.
const memberCmdMax = 160

// parsePS reads `ps -A -o pid=,pgid=,stat=,command=` output into the live
// members of the wanted groups (zombies are not live).
func parsePS(out []byte, want map[int]bool) map[int][]procMember {
	res := map[int][]procMember{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		pg, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil || !want[pg] || strings.HasPrefix(f[2], "Z") {
			continue
		}
		res[pg] = append(res[pg], procMember{PID: pid, Cmd: clipCmd(strings.Join(f[3:], " "))})
	}
	return res
}

// wantGroups is the set of real process groups among pgids (0 and 1 are
// never an exec's group).
func wantGroups(pgids []int) map[int]bool {
	want := map[int]bool{}
	for _, g := range pgids {
		if g > 1 {
			want[g] = true
		}
	}
	return want
}

func clipCmd(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > memberCmdMax {
		s = s[:memberCmdMax] + "…"
	}
	return s
}
