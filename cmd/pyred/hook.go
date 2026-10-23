package main

import (
	"errors"
	"os"

	"github.com/jhh3/pyre/internal/runtime/agy"
	"github.com/jhh3/pyre/internal/runtime/claude"
)

// cmdHook is the harness hook entry point:
//
//	pyred hook claude <Event> [<state-dir>]
//	pyred hook agy <Event> <state-dir>
//
// It never fails the harness: claude.HookMain and agy.HookMain always exit
// 0 (a non-zero hook exit has blocking semantics in Claude Code).
func cmdHook(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: pyred hook claude|agy <event> [state-dir]")
	}
	switch args[0] {
	case "claude":
		os.Exit(claude.HookMain(args[1:], os.Stdin, os.Stdout))
	case "agy":
		os.Exit(agy.HookMain(args[1:], os.Stdin, os.Stdout))
	}
	return errors.New("unknown harness " + args[0])
}
