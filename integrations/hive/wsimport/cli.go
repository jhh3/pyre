package wsimport

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// APIFactory returns the hub MessageAPI used by --apply (the pyre CLI wires
// its authenticated client here). It is only called with --apply.
type APIFactory func(ctx context.Context) (contracts.MessageAPI, error)

// Main runs the importer CLI. Dry run by default.
//
//	pyre-wsimport --root COPY [--snapshot-from LIVE] [--alias-map F]
//	    [--write-alias-template F] [--default-recipient skip|owner]
//	    [--unread-mode queue|info] [--map-out F] [--json] [--apply]
func Main(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string, api APIFactory) int {
	fs := flag.NewFlagSet("pyre-wsimport", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "ws root COPY to import (required)")
	snapFrom := fs.String("snapshot-from", "", "copy this live ws root into --root first (read-only on the source)")
	aliasPath := fs.String("alias-map", "", "alias map JSON (legacy mailbox -> Pyre address)")
	tmplPath := fs.String("write-alias-template", "", "write a proposed alias map for every mailbox and exit")
	defRcpt := fs.String("default-recipient", "skip", "unmapped mailboxes: skip | owner")
	unreadMode := fs.String("unread-mode", "queue", "never-delivered messages to sessions: queue | info")
	mapOut := fs.String("map-out", "", "write legacy-id -> message-id map (no bodies) for cutover/rollback reconciliation")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	apply := fs.Bool("apply", false, "send the plan to the hub (idempotent); default is a dry run")
	allowLive := fs.Bool("allow-live", false, "permit --root to be the live ws root (not recommended)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *root == "" {
		fmt.Fprintln(stderr, "pyre-wsimport: --root is required (a copy of the ws root; use --snapshot-from to make one)")
		return 2
	}
	if *defRcpt != "skip" && *defRcpt != "owner" {
		fmt.Fprintln(stderr, "pyre-wsimport: --default-recipient must be skip or owner")
		return 2
	}
	if *unreadMode != "queue" && *unreadMode != "info" {
		fmt.Fprintln(stderr, "pyre-wsimport: --unread-mode must be queue or info")
		return 2
	}
	live := LiveRoots(getenv)
	if *snapFrom != "" {
		if isLive(*root, live) {
			fmt.Fprintln(stderr, "pyre-wsimport: --root must not be the live ws root")
			return 2
		}
		n, err := CopyTree(*snapFrom, *root)
		if err != nil {
			fmt.Fprintf(stderr, "pyre-wsimport: snapshot: %v\n", err)
			return 1
		}
		fmt.Fprintf(stderr, "snapshot: copied %d files from %s to %s\n", n, *snapFrom, *root)
	}
	tree, err := Load(*root, live, *allowLive, time.Now().UTC())
	if err != nil {
		fmt.Fprintf(stderr, "pyre-wsimport: %v\n", err)
		return 1
	}
	if *tmplPath != "" {
		if err := writeJSON(*tmplPath, Template(tree)); err != nil {
			fmt.Fprintf(stderr, "pyre-wsimport: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "wrote alias template for %d mailboxes to %s\n", len(tree.Mailboxes), *tmplPath)
		return 0
	}
	var am AliasMap
	if *aliasPath != "" {
		if am, err = LoadAliasMap(*aliasPath); err != nil {
			fmt.Fprintf(stderr, "pyre-wsimport: %v\n", err)
			return 1
		}
	}
	plan := Build(tree, Options{Aliases: am, DefaultRecipient: *defRcpt, UnreadMode: *unreadMode})
	if *mapOut != "" {
		if err := writeJSON(*mapOut, struct {
			Digest  string    `json:"digest"`
			TakenAt time.Time `json:"taken_at"`
			Items   []Item    `json:"items"`
		}{plan.Report.Digest, plan.Report.TakenAt, plan.Items}); err != nil {
			fmt.Fprintf(stderr, "pyre-wsimport: %v\n", err)
			return 1
		}
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(plan.Report)
	} else {
		printReport(stdout, plan.Report, !*apply)
	}
	if !*apply {
		return 0
	}
	if api == nil {
		fmt.Fprintln(stderr, "pyre-wsimport: --apply needs a hub connection; run it through `pyre migrate ws-mail --apply`")
		return 2
	}
	client, err := api(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "pyre-wsimport: hub: %v\n", err)
		return 1
	}
	res, err := Apply(ctx, client, plan, ApplyOptions{})
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Fprintf(stdout, "apply result:\n%s\n", b)
	if err != nil {
		fmt.Fprintf(stderr, "pyre-wsimport: %v\n", err)
		return 1
	}
	if res.Conflicts > 0 || res.Errors > 0 {
		return 1
	}
	return 0
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return errors.New(path + " already exists; refusing to overwrite")
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func printReport(w io.Writer, r Report, dry bool) {
	mode := "APPLY"
	if dry {
		mode = "DRY RUN (nothing sent; add --apply)"
	}
	fmt.Fprintf(w, "ws mail import plan: %s\n", mode)
	fmt.Fprintf(w, "source        %s\n", r.Root)
	fmt.Fprintf(w, "watermark     %s (taken %s)\n", r.Digest, r.TakenAt.Format(time.RFC3339))
	fmt.Fprintf(w, "mailboxes     %d (streams %d, without stream %d)\n", r.Mailboxes, r.Streams, r.MailboxesWithoutStream)
	fmt.Fprintf(w, "messages      %d: unread %d, read %d; open reply obligations %d (expect_reply %d)\n",
		r.Messages, r.Unread, r.Read, r.PendingObligations, r.ExpectReply)
	fmt.Fprintf(w, "replies       %d with reply_to, %d parents missing; %d without id; %d without frontmatter\n",
		r.WithReplyTo, r.MissingReplyParent, r.MissingID, r.NoFrontmatter)
	fmt.Fprintf(w, "channels      %d, posts %d, subscriptions %d, cursors %d, undelivered (subscriber,post) %d\n",
		r.Channels, r.ChannelPosts, r.Subscriptions, r.ChannelCursors, r.UnreadChannelDeliveries)
	fmt.Fprintf(w, "requests      %d (queue %d, info %d, request_ack %d); skipped %d; already in pyre %d\n",
		r.Requests, r.QueueRequests, r.InfoRequests, r.RequestAck, r.Skipped, r.AlreadyInPyre)
	if len(r.Unresolved) > 0 {
		keys := make([]string, 0, len(r.Unresolved))
		for k := range r.Unresolved {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintln(w, "unresolved    (no alias; add them to --alias-map or use --default-recipient owner)")
		for _, k := range keys {
			fmt.Fprintf(w, "  %-28s %d message(s)\n", k, r.Unresolved[k])
		}
	}
	fmt.Fprintln(w, "per mailbox   unread/read/pending -> address")
	for _, m := range r.PerMailbox {
		addr := m.Address
		if addr == "" {
			addr = "(unmapped)"
		}
		fmt.Fprintf(w, "  %-28s %d/%d/%d -> %s\n", m.Mailbox, m.Unread, m.Read, m.Pending, addr)
	}
	for _, p := range r.Problems {
		fmt.Fprintf(w, "problem       %s\n", p)
	}
}
