// Command pyre is the CLI, terminal UI entry point, and agent tool
// interface (CLI + MCP server). See internal/cli.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/jhh3/pyre/internal/cli"
)

func main() {
	// Cancel on SIGINT/SIGTERM so long-polls and follows end cleanly. An
	// attached terminal (pyre attach) is not bound to this context.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.NewApp().Main(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
