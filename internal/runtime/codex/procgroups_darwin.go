package codex

import (
	"context"
	"os/exec"
	"time"
)

// groupMembers returns the live members of each wanted process group,
// from ps (macOS has no /proc). ok is false when ps fails.
func groupMembers(pgids []int) (map[int][]procMember, bool) {
	want := wantGroups(pgids)
	if len(want) == 0 {
		return map[int][]procMember{}, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-ww", "-o", "pid=,pgid=,stat=,command=").Output()
	if err != nil || len(out) == 0 {
		return nil, false
	}
	return parsePS(out, want), true
}
