package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
)

// addConnectionSubcommands adds grant/revoke/invoke/verify under
// `pyre connections`.
func (a *App) addConnectionSubcommands(connections *cobra.Command) {
	for _, active := range []bool{true, false} {
		active := active
		var p ConnectionGrantCLIParams
		name, short := "grant", "Allow a session, a project's sessions, or a node's sessions to use a connection"
		if !active {
			name, short = "revoke", "Withdraw exactly that grant (the next use is denied)"
		}
		cmd := &cobra.Command{
			Use:   name + " <connection> (--session S | --project P | --node N)",
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: a.run(func(cmd *cobra.Command, args []string) error {
				ops, err := a.connect()
				if err != nil {
					return err
				}
				q := p
				q.Connection, q.OperationID = args[0], a.opID
				if q.Session == "" && q.Project == "" && q.Node == "" {
					return usagef("give --session, --project or --node")
				}
				res, err := ops.GrantConnection(cmd.Context(), q, active)
				if err != nil {
					return err
				}
				return a.emit(res, func(w io.Writer) {
					verb := "granted"
					if !active {
						verb = "revoked"
					}
					g := res.Grant
					fmt.Fprintf(w, "%s %s: node %s", verb, g.ConnectionID, orDash(g.NodeID))
					if g.ProjectID != "" {
						fmt.Fprintf(w, " project %s", g.ProjectID)
					}
					if g.SessionID != "" {
						fmt.Fprintf(w, " session %s", g.SessionID)
					}
					fmt.Fprintf(w, " (pushed to %s)\n", strings.Join(res.PushedTo, ", "))
				})
			}),
		}
		cmd.Flags().StringVar(&p.Session, "session", "", "session ID or title (scope node defaults to its node)")
		cmd.Flags().StringVar(&p.Project, "project", "", "project ID or name: its sessions on --node")
		cmd.Flags().StringVar(&p.Node, "node", "", "node whose sessions may use the connection")
		connections.AddCommand(cmd)
	}

	var ip ConnectionInvokeCLIParams
	var kv []string
	invoke := &cobra.Command{
		Use:   "invoke <connection> <operation> [--arg k=v]... [--session S] [--node N]",
		Short: "Run a read operation through the node that holds the connection (no credential is returned)",
		Args:  cobra.ExactArgs(2),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			q := ip
			q.Connection, q.Operation, q.OperationID = args[0], args[1], a.opID
			pairs, err := parsePairs(kv, "--arg")
			if err != nil {
				return err
			}
			for _, pr := range pairs {
				if q.Args == nil {
					q.Args = map[string]string{}
				}
				q.Args[pr[0]] = pr[1]
			}
			res, err := ops.InvokeConnection(cmd.Context(), q)
			if err != nil {
				return err
			}
			return a.emit(res, func(w io.Writer) {
				var buf bytes.Buffer
				if json.Indent(&buf, res.Output, "", "  ") != nil {
					buf.Write(res.Output)
				}
				fmt.Fprintln(w, strings.TrimSpace(buf.String()))
				if res.Truncated {
					fmt.Fprintln(w, "(truncated)")
				}
			})
		}),
	}
	invoke.Flags().StringArrayVar(&kv, "arg", nil, "operation argument key=value (repeatable; sent on stdin to Outfit, never argv)")
	invoke.Flags().StringVar(&ip.Session, "session", "", "session the use is attributed to (default: this session)")
	invoke.Flags().StringVar(&ip.Node, "node", "", "executing node (default: the hub routes to the verified holder)")
	connections.AddCommand(invoke)

	var cp ConnectionCheckCLIParams
	verify := &cobra.Command{
		Use:   "verify <connection> [--node N]",
		Short: "Verify the connection now on its holder node(s) (connection.check)",
		Long: `Verify the connection now: pyred on each node runs the real check there
(outfit connector test NAME for an Outfit connection, a store read for a
Pyre-provisioned credential) and reports its result. pyred runs in the
user's login session, so the result is what Pyre sessions on that node get;
an ssh login can reach a different credential store (Windows Credential
Manager, a locked macOS keychain) and see another result.

Exits 0 only when every checked node reports "verified"; otherwise it prints
(or, with --json, emits) the per-node results and exits 1.`,
		Args: cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			q := cp
			q.Connection = args[0]
			avs, err := ops.CheckConnection(cmd.Context(), q)
			if err != nil {
				return err
			}
			if err := a.emit(avs, func(w io.Writer) {
				var rows [][]string
				for _, av := range avs {
					rows = append(rows, []string{av.NodeID, av.Mode, av.State, ago(av.VerifiedAt), orDash(av.Detail)})
				}
				table(w, "NODE\tMODE\tSTATE\tVERIFIED\tDETAIL", rows)
				if len(avs) > 0 {
					fmt.Fprintln(w, "checked by pyred on each node: the result Pyre sessions there get (an ssh login may reach a different credential store)")
				}
			}); err != nil {
				return err
			}
			return verifyFailures(args[0], avs)
		}),
	}
	verify.Flags().StringVar(&cp.Node, "node", "", "node to verify on (default: every holder)")
	connections.AddCommand(verify)
}

// addNotePromote adds `pyre note promote`.
func (a *App) addNotePromote(note *cobra.Command) {
	var p PromoteNoteParams
	promote := &cobra.Command{
		Use:   "promote <note> (--title T [--slug S] | --doc DOC --base REV) [--project P]",
		Short: "Explicitly turn a note into an authoritative document revision",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			q := p
			q.Note, q.OperationID = args[0], a.opID
			if q.Document == "" && q.Title == "" {
				return usagef("give --title (new document) or --doc and --base (successor revision)")
			}
			res, err := ops.PromoteNote(cmd.Context(), q)
			if err != nil {
				return err
			}
			return a.emit(res, func(w io.Writer) {
				fmt.Fprintf(w, "promoted %s to %s %q revision %s\n", q.Note, res.Document.DocumentID, res.Document.Title, res.Revision.RevisionID)
			})
		}),
	}
	promote.Flags().StringVar(&p.Title, "title", "", "title of a new document")
	promote.Flags().StringVar(&p.Slug, "slug", "", "slug of a new document")
	promote.Flags().StringVar(&p.Document, "doc", "", "existing document ID")
	promote.Flags().StringVar(&p.BaseRevision, "base", "", "the document head revision the promotion builds on")
	promote.Flags().StringVar(&p.Project, "project", "", "project ID or name")
	note.AddCommand(promote)
}

// verifyFailures is nil when every node verified the connection, else an
// error (exit 1) naming each node and state. No node at all is a failure:
// nothing holds the connection.
func verifyFailures(conn string, avs []c.ConnectionAvailability) error {
	if len(avs) == 0 {
		return withStatus(ExitError, fmt.Errorf("%s: no node holds this connection (give --node to check one)", conn))
	}
	var bad []string
	for _, av := range avs {
		if av.State != "verified" {
			bad = append(bad, fmt.Sprintf("%s on %s: %s", conn, av.NodeID, av.State))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return withStatus(ExitError, fmt.Errorf("not verified: %s", strings.Join(bad, "; ")))
}
