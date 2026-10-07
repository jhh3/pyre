package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"

	"github.com/jhh3/pyre/runtime/channels"
)

// cmdChannel runs the optional Claude dev-channel MCP bridge on stdio for
// one run (input.channel; off by default). The Claude adapter starts it via
// --mcp-config only when the owner enabled channels for the session.
func cmdChannel(args []string) error {
	fs := flag.NewFlagSet("channel", flag.ContinueOnError)
	runDir := fs.String("run-dir", os.Getenv("PYRE_RUN_DIR"), "the run's private state dir (default $PYRE_RUN_DIR)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *runDir == "" {
		return errors.New("--run-dir (or PYRE_RUN_DIR) is required; pyred channel is started by the Claude adapter for one run")
	}
	s := &channels.Server{Spool: filepath.Join(*runDir, "channel", "spool"), Events: filepath.Join(*runDir, "channel", "events.jsonl"),
		In: os.Stdin, Out: os.Stdout}
	return s.Run(context.Background())
}
