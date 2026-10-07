//go:build unix

package hive

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ExecRunner runs background subprocesses at nice BackgroundNice (an
// absolute value) and leaves interactive ones at the caller's niceness (one
// tiny `sh` + `ps` each). The caller is not assumed to run at nice 0: the
// GitHub macOS runner starts jobs at nice -10.
func TestExecRunnerLowersBackgroundPriority(t *testing.T) {
	nice := func(bg bool) int {
		t.Helper()
		res, err := ExecRunner{}.Run(context.Background(), Cmd{Path: "/bin/sh", Args: []string{"-c", "sleep 0.1; ps -o nice= -p $$"},
			Timeout: 5 * time.Second, Background: bg})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("sh: %v exit %d %s", err, res.ExitCode, res.Stderr)
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(res.Stdout)))
		if err != nil {
			t.Fatalf("ps output %q", res.Stdout)
		}
		return n
	}
	fg, bg := nice(false), nice(true)
	if fg >= 10 {
		t.Skipf("test process already runs at nice %d", fg)
	}
	if bg != BackgroundNice {
		t.Fatalf("background nice %d (interactive %d); want %d", bg, fg, BackgroundNice)
	}
}
