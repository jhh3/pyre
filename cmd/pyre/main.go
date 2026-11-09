// Command pyre is the CLI, terminal UI entry point, and agent tool
// interface (CLI + MCP server). See internal/cli.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata" // Pacific times on hosts without a tz database (Windows)

	"github.com/jhh3/pyre/internal/cli"
	"github.com/jhh3/pyre/internal/ghguard"
)

func main() {
	// Started as `gh` (pyred's shim link on a run's PATH): guard the
	// GitHub API budget, then exec the real gh.
	if ghguard.IsShimName(os.Args[0]) {
		os.Exit(ghguard.Main(os.Args[1:]))
	}
	// Cancel on SIGINT/SIGTERM so long-polls and follows end cleanly. An
	// attached terminal (pyre attach) is not bound to this context.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.NewApp().Main(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
