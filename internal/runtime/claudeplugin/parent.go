package claudeplugin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/osutil"
)

// ParentWatch returns an Alive func for WatchOptions: it reports false
// once the process that started the watcher is gone. Claude Code 2.1.289
// runs a monitor as `<shell> -c "… eval '<cmd>' … && pwd -P >| …"`, so the
// watcher's parent is that shell, which outlives a killed Claude (it is
// reparented, not killed). The check therefore records the parent and the
// grandparent at start and fails when either link changes: the watcher was
// reparented, or its shell was. It costs a getppid per call plus, on macOS,
// one `ps` for the grandparent link (Linux reads /proc).
func ParentWatch() func() bool {
	p := os.Getppid()
	g, err := ppidOf(p)
	if err != nil {
		g = -1 // unknown: only the direct link is checked
	}
	return func() bool {
		if q := os.Getppid(); q != p || q == 1 {
			return false
		}
		if osutil.SignalZero(p) != nil {
			return false
		}
		if g <= 0 {
			return true
		}
		h, err := ppidOf(p)
		if err != nil {
			return true // transient (ps unavailable): keep running
		}
		return h == g && h != 1
	}
}

// ppidOf returns the parent pid of pid.
func ppidOf(pid int) (int, error) {
	if runtime.GOOS == "windows" {
		return osutil.ParentPID(pid)
	}
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return 0, err
		}
		// pid (comm) state ppid …; comm may contain spaces and parentheses.
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			return 0, fmt.Errorf("unparsable /proc/%d/stat", pid)
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 2 {
			return 0, fmt.Errorf("unparsable /proc/%d/stat", pid)
		}
		return strconv.Atoi(f[1])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-o", "ppid=", "-p", strconv.Itoa(pid))
	cmd.Env = []string{"LC_ALL=C", "PATH=/bin:/usr/bin"}
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}
