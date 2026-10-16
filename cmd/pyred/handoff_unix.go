//go:build !windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// doExec replaces the process. Only returns on failure.
func doExec(r *execRequest) error {
	p := absExecPath(r.path)
	argv := append([]string{p}, os.Args[1:]...)
	fmt.Fprintf(os.Stderr, "pyred: exec %s (%s)\n", p, r.why)
	return syscall.Exec(p, argv, r.env)
}

// watchSupervisor is Windows-only (a supervised daemon there cannot be
// replaced in place); on Unix the service manager owns the process.
func watchSupervisor(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}

func childAttrs(*exec.Cmd, bool) {}

func hideConsole() {}
