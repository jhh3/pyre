//go:build !linux

package claude

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// fileHolder returns a process that has path open (lsof; 0 when none is
// seen or lsof is missing).
func fileHolder(path string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-t", "--", path).Output()
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]))
	return pid
}
