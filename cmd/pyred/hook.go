package main

import (
	"errors"
	"os"

	"github.com/jhh3/pyre/internal/runtime/claude"
)

// cmdHook is the harness hook entry point:
//
//	pyred hook claude <Event> [<state-dir>]
//
// It never fails the harness: claude.HookMain always exits 0 (a non-zero
// hook exit has blocking semantics in Claude Code).
func cmdHook(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: pyred hook claude <event> [state-dir]")
	}
	switch args[0] {
	case "claude":
		os.Exit(claude.HookMain(args[1:], os.Stdin, os.Stdout))
	}
	return errors.New("unknown harness " + args[0])
}
