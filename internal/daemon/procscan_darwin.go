package daemon

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// processCwds lists the working directories of this user's processes
// (lsof; W-lifecycle).
func processCwds() []string {
	out := psRun("lsof", "-a", "-d", "cwd", "-u", strconv.Itoa(os.Getuid()), "-Fn")
	var cwds []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if l := sc.Text(); strings.HasPrefix(l, "n") {
			cwds = append(cwds, l[1:])
		}
	}
	return cwds
}

// openPathsUnder lists what this user's processes hold under roots (lsof:
// open files, working directories, executables). ok is false without lsof
// or when it printed nothing (it always lists at least itself).
func openPathsUnder(roots []string) (paths []string, ok bool) {
	if _, err := exec.LookPath("lsof"); err != nil {
		return nil, false
	}
	out := psRun("lsof", "-n", "-P", "-w", "-u", strconv.Itoa(os.Getuid()), "-Fn")
	if len(out) == 0 {
		return nil, false
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		if l := sc.Text(); strings.HasPrefix(l, "n") && underAny(l[1:], roots) {
			paths = append(paths, l[1:])
		}
	}
	return paths, true
}

// sessionProcesses lists this user's processes in one of the session's
// terminal sessions (sids) whose environment names sessionID as
// PYRE_SESSION_ID (ps -E shows the environment after the command line, so
// a command line that spells the variable out also matches; the
// terminal-session check keeps that to the session's own processes).
func sessionProcesses(sessionID string, sids []int) []int {
	want := "PYRE_SESSION_ID=" + sessionID
	out := psRun("ps", "-E", "-ww", "-o", "pid=,command=", "-U", strconv.Itoa(os.Getuid()))
	self := os.Getpid()
	var pids []int
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil || pid == self || !inSessions(pid, sids) {
			continue
		}
		for _, tok := range f[1:] {
			if tok == want {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids
}

func getsid(pid int) (int, error) { return syscall.Getsid(pid) }

func psRun(name string, args ...string) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, name, args...).Output()
	return out
}
