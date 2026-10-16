package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/enrollment"
)

// cmdEnroll redeems an enrollment ticket (the one-time secret) at the hub
// over pinned TLS and writes the node credential file (0600). The CLI shape
// matches enrollment.SSHBootstrapPlan:
//
//	pyred enroll --hub URL [--pin SHA256] --node-id ID --credential-file PATH --secret-stdin
//	pyred enroll --ticket-stdin [--hub URL] [--state DIR | --credential-file PATH]
//
// --ticket-stdin reads the whole ticket JSON (`pyre --json enroll-node`
// output: node_id, secret, hub_url, hub_cert_sha256) from stdin, for
// unattended installers (scripts/install/install.ps1); --hub and --pin
// override the ticket's values (the hub URL as reachable from this node).
func cmdEnroll(args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	hub := fs.String("hub", "", "hub URL (https://host:port)")
	pin := fs.String("pin", "", "hub TLS certificate SHA-256 fingerprint")
	nodeID := fs.String("node-id", "", "node ID from the enrollment ticket")
	credFile := fs.String("credential-file", "", "where to write the node credential (default <state>/node-credential.json)")
	state := fs.String("state", "", "state directory (for the default credential path)")
	secretStdin := fs.Bool("secret-stdin", false, "read the one-time enrollment secret from stdin")
	ticketStdin := fs.Bool("ticket-stdin", false, "read the whole enrollment ticket JSON (pyre --json enroll-node) from stdin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	secret := ""
	if *ticketStdin {
		if *secretStdin {
			return errors.New("--ticket-stdin and --secret-stdin are exclusive")
		}
		var t contracts.EnrollmentTicket
		if err := json.NewDecoder(io.LimitReader(os.Stdin, 64<<10)).Decode(&t); err != nil {
			return errors.New("--ticket-stdin: stdin is not an enrollment ticket JSON (pyre --json enroll-node)")
		}
		if *hub == "" {
			*hub = t.HubURL
		}
		if *pin == "" {
			*pin = t.HubCertSHA256
		}
		if *nodeID == "" {
			*nodeID = t.NodeID
		} else if t.NodeID != "" && t.NodeID != *nodeID {
			return fmt.Errorf("--node-id %s disagrees with the ticket's node %s", *nodeID, t.NodeID)
		}
		secret = strings.TrimSpace(t.Secret)
	}
	if *hub == "" || *nodeID == "" {
		return errors.New("--hub and --node-id are required")
	}
	// The secret is accepted only on stdin: a command-line value would be
	// visible to other local processes (ps).
	if *secretStdin {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("read secret from stdin: %w", err)
		}
		secret = strings.TrimSpace(line)
	}
	if secret == "" {
		return errors.New("an enrollment secret is required (--secret-stdin)")
	}
	path := *credFile
	if path == "" {
		dir := *state
		if dir == "" {
			dir = defaultState()
		}
		path = filepath.Join(dir, "node-credential.json")
	}
	host, _ := os.Hostname()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cred, err := enrollment.ExchangeEnrollment(ctx, *hub, *pin, contracts.NodeEnrollExchange{NodeID: *nodeID, Secret: secret,
		Hostname: host, Platform: runtime.GOOS, Arch: runtime.GOARCH})
	if err != nil {
		return err
	}
	if err := enrollment.SaveNodeCredential(path, enrollment.NodeCredentialFile{HubURL: *hub, HubCertSHA256: *pin, NodeID: cred.NodeID,
		Token: cred.Token, EnrolledAt: time.Now().UTC()}); err != nil {
		return err
	}
	fmt.Printf("enrolled node %s; credential written to %s\n", cred.NodeID, path)
	return nil
}
