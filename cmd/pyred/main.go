// Command pyred is the per-machine Pyre node daemon (WP06).
//
//	pyred run [--state DIR] [--hub URL] [--credential FILE] [--owner-credential FILE]
//	          [--name NAME] [--ssh-alias ALIAS] [--max-runs N] [--dev-mock]
//	pyred enroll --hub URL [--pin SHA256] --node-id ID --credential-file PATH
//	             --secret-stdin
//	pyred status [--state DIR] [--json]
//	pyred hook claude <event>
//	pyred channel [--run-dir DIR]
//	pyred version [--json]
//
// Stopping pyred never stops runs: they live in Pyre-owned tmux servers
// and are recovered on the next start.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/jhh3/pyre/internal/buildinfo"
	"github.com/jhh3/pyre/internal/contracts"
)

func usage() {
	fmt.Fprint(os.Stderr, `usage: pyred <command> [flags]

commands:
  run       run the node daemon (local socket, hub link, run supervision)
  enroll    redeem an enrollment ticket and write the node credential
  status    show the local daemon's status
  hook      harness hook entry point (pyred hook claude <event>)
  channel   optional Claude dev-channel MCP bridge (stdio)
  version   print the version
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(args)
	case "enroll":
		err = cmdEnroll(args)
	case "status":
		err = cmdStatus(args)
	case "hook":
		err = cmdHook(args)
	case "channel":
		err = cmdChannel(args)
	case "inbox-watch":
		err = cmdInboxWatch(args)
	case "version", "--version", "-v":
		if len(args) > 0 && args[0] == "--json" {
			b, _ := json.Marshal(map[string]any{"version": buildinfo.Version, "protocol": contracts.ProtocolVersion, "min_protocol": contracts.MinProtocolVersion})
			fmt.Println(string(b))
		} else {
			fmt.Printf("pyred %s\n", buildinfo.Version)
		}
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "pyred %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}
