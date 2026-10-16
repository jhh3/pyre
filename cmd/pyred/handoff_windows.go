//go:build windows

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

// doExec hands the exec request to the supervisor (see supervise.go): the
// request file is written and the process exits with exitExecRequest. A
// daemon started without a supervisor becomes one, starting the target as
// its child. Only returns on failure.
func doExec(r *execRequest) error {
	t := requestFromExec(r)
	if trampolineFile != "" {
		if err := writeTrampolineRequest(trampolineFile, t); err != nil {
			return fmt.Errorf("hand the exec of %s to the supervisor: %w", t.Path, err)
		}
		fmt.Fprintf(os.Stderr, "pyred: exec %s (%s) via the supervisor\n", t.Path, r.why)
		os.Exit(exitExecRequest)
	}
	launcher, _, _ := launcherInfo()
	fmt.Fprintf(os.Stderr, "pyred: exec %s (%s); this process now supervises it\n", t.Path, r.why)
	return superviseLoop(absExecPath(launcher), os.Args[1:], &t, io.Writer(os.Stderr), false)
}

// watchSupervisor cancels the returned context when the supervising
// process (envTrampolinePID) exits: `pyred service stop` ends the task's
// supervisor, and the daemon then shuts down cleanly instead of lingering.
func watchSupervisor(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	pid, err := strconv.Atoi(trampolinePID)
	if err != nil || pid <= 0 {
		return ctx, cancel
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return ctx, cancel
	}
	go func() {
		defer windows.CloseHandle(h)
		if ev, _ := windows.WaitForSingleObject(h, windows.INFINITE); ev == windows.WAIT_OBJECT_0 {
			fmt.Fprintln(os.Stderr, "pyred: supervisor exited; stopping")
			cancel()
		}
	}()
	return ctx, cancel
}

// childAttrs: a supervised daemon gets no console window of its own.
func childAttrs(cmd *exec.Cmd, noWindow bool) {
	if !noWindow {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

// hideConsole detaches from the console window Task Scheduler opens for a
// console program in the interactive session (it closes once no process
// is attached).
func hideConsole() { _, _, _ = procFreeConsole.Call() }

var procFreeConsole = windows.NewLazySystemDLL("kernel32.dll").NewProc("FreeConsole")
