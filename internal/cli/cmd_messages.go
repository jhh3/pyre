package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
)

func (a *App) addMessageCommands(root *cobra.Command) {
	var sp SendParams
	send := &cobra.Command{
		Use:   `send <session|owner|channel:NAME|project:P> "text"`,
		Short: "Send a durable message to a session, the owner, a channel or a project",
		Long: `Send a durable, addressed message. The result reports separate facts:
stored on the hub, accepted by the recipient runtime (with evidence), and
acknowledged/replied. Runtime acceptance means the input interface took it,
not that the agent read or completed it.

With --reply-to MSG and a single argument, the reply goes to the original
sender in the original thread. Use "-" as the text to read stdin.
--attach takes published IDs (ver_, rev_, note_); local paths are not
transferable.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p := sp
			if len(args) == 1 {
				if p.ReplyTo == "" {
					return usagef("give a recipient and text, or --reply-to MSG and text")
				}
				p.Text = args[0]
			} else {
				p.To, p.Text = args[0], args[1]
			}
			if p.Text, err = a.readArg(p.Text); err != nil {
				return err
			}
			p.OperationID = a.opID
			m, err := ops.Send(cmd.Context(), p)
			if err != nil {
				return err
			}
			return a.emit(m, func(w io.Writer) {
				fmt.Fprintf(w, "%s -> %s\n", m.Envelope.MessageID, addrText(m.Envelope.Recipient))
				printFacts(w, m)
			})
		}),
	}
	sf := send.Flags()
	sf.StringVar(&sp.Thread, "thread", "", "thread ID")
	sf.StringVar(&sp.ReplyTo, "reply-to", "", "message this replies to")
	sf.BoolVar(&sp.Steer, "steer", false, "explicit immediate steering (only where the runtime supports it)")
	sf.BoolVar(&sp.Info, "info", false, "store and show only; never inject into a runtime")
	sf.StringSliceVar(&sp.Attach, "attach", nil, "published artifact version / document revision / note ID")
	sf.BoolVar(&sp.RequestAck, "request-ack", false, "ask the recipient to acknowledge")
	sf.StringVar(&sp.Project, "project", "", "project the message belongs to")
	sf.IntVar(&sp.TTLSeconds, "ttl", 0, "retention for undelivered mail in seconds (hub default if 0)")
	sf.StringVar(&sp.MessageID, "message-id", "", "stable message ID (safe retries across processes)")
	root.AddCommand(send)

	var ip InboxParams
	inbox := &cobra.Command{
		Use:   "inbox [--unacked] [--wait SECS] [--after CURSOR]",
		Short: "List messages addressed to you (this session inside an agent, else the owner)",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			if ip.WaitSeconds > 60 {
				ip.WaitSeconds = 60
			}
			page, err := ops.Inbox(cmd.Context(), ip)
			if err != nil {
				return err
			}
			return a.emit(page, func(w io.Writer) { printMessages(w, page.Items) })
		}),
	}
	inf := inbox.Flags()
	inf.StringVar(&ip.Session, "session", "", `mailbox: a session, or "owner" (default: this session, else owner)`)
	inf.StringVar(&ip.Box, "box", "inbox", "inbox, outbox or all")
	inf.StringVar(&ip.Thread, "thread", "", "a whole thread instead of a mailbox")
	inf.BoolVar(&ip.Unacked, "unacked", false, "only messages not yet acknowledged or replied to")
	inf.StringSliceVar(&ip.States, "delivery", nil, "filter by runtime delivery state (e.g. indeterminate)")
	inf.IntVar(&ip.WaitSeconds, "wait", 0, "long-poll up to SECS (max 60) for a new message")
	inf.Int64Var(&ip.AfterCursor, "after", 0, "only messages after this inbox cursor")
	inf.IntVar(&ip.Limit, "limit", 0, "at most N messages")
	root.AddCommand(inbox)

	var wp WaitReplyParams
	wait := &cobra.Command{
		Use:   "wait-reply <msg_id> [--timeout SECS]",
		Short: "Wait for a reply to a message you sent (exit 5 on timeout)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			wp.MessageID = args[0]
			res, err := ops.WaitReply(cmd.Context(), wp)
			if err != nil {
				if res.Message.Envelope.MessageID != "" && a.jsonOut {
					_ = a.emit(res, func(io.Writer) {})
				} else if res.Message.Envelope.MessageID != "" {
					printFacts(a.Stderr, res.Message)
				}
				return err
			}
			return a.emit(res, func(w io.Writer) {
				r := res.Reply
				fmt.Fprintf(w, "reply %s from %s:\n%s\n", r.Envelope.MessageID, principalText(r.Envelope.Sender), r.Envelope.Body.Text)
				printAttachments(w, r.Envelope.Attachments)
			})
		}),
	}
	wait.Flags().IntVar(&wp.TimeoutSeconds, "timeout", 60, "seconds to wait")
	root.AddCommand(wait)

	msg := &cobra.Command{Use: "msg", Short: "Inspect messages", Args: cobra.NoArgs}
	msg.AddCommand(&cobra.Command{
		Use:   "show <msg_id>",
		Short: "Show a message and every delivery fact (stored / runtime / acknowledged)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			m, err := ops.GetMessage(cmd.Context(), MessageRef{MessageID: args[0]})
			if err != nil {
				return err
			}
			return a.emit(m, func(w io.Writer) { printMessage(w, m) })
		}),
	})
	root.AddCommand(msg)

	var ackNote string
	ack := &cobra.Command{
		Use:   "ack <msg_id>",
		Short: "Acknowledge a message without replying",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			m, err := ops.Ack(cmd.Context(), MessageRef{MessageID: args[0], Note: ackNote, OperationID: a.opID})
			if err != nil {
				return err
			}
			return a.emit(m, func(w io.Writer) { fmt.Fprintf(w, "%s acknowledged\n", m.Envelope.MessageID) })
		}),
	}
	ack.Flags().StringVar(&ackNote, "note", "", "optional note")
	root.AddCommand(ack)

	var rp ReissueParams
	reissue := &cobra.Command{
		Use:   "reissue <msg_id> --accept-duplicate-risk [--text TEXT]",
		Short: "Explicitly re-send uncertain or rejected work as a new message referencing the original",
		Long: `Create a new message that references the original. Use it only after
inspecting an indeterminate or rejected delivery: the recipient may already
have acted on the original, so you must accept the duplication risk.`,
		Args: cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p := rp
			p.MessageID, p.OperationID = args[0], a.opID
			m, err := ops.Reissue(cmd.Context(), p)
			if err != nil {
				return err
			}
			return a.emit(m, func(w io.Writer) {
				fmt.Fprintf(w, "%s reissues %s -> %s\n", m.Envelope.MessageID, args[0], addrText(m.Envelope.Recipient))
				printFacts(w, m)
			})
		}),
	}
	reissue.Flags().BoolVar(&rp.AcceptDuplicateRisk, "accept-duplicate-risk", false, "confirm the recipient may act twice")
	reissue.Flags().StringVar(&rp.Text, "text", "", "replacement body (default: the original)")
	root.AddCommand(reissue)

	var rdNote string
	resolve := &cobra.Command{
		Use:   "resolve-delivery <msg_id> accepted|rejected --note TEXT",
		Short: "Record your decision about an indeterminate delivery after inspecting evidence",
		Args:  cobra.ExactArgs(2),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			m, err := ops.ResolveDelivery(cmd.Context(), ResolveDeliveryParams{MessageID: args[0], Resolution: args[1], Note: rdNote, OperationID: a.opID})
			if err != nil {
				return err
			}
			return a.emit(m, func(w io.Writer) {
				fmt.Fprintf(w, "%s delivery recorded as %s\n", m.Envelope.MessageID, m.Status.Runtime)
				printFacts(w, m)
			})
		}),
	}
	resolve.Flags().StringVar(&rdNote, "note", "", "what evidence you inspected")
	root.AddCommand(resolve)

	root.AddCommand(&cobra.Command{
		Use:   "cancel <msg_id>",
		Short: "Cancel a queued message before any delivery attempt",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			m, err := ops.Cancel(cmd.Context(), MessageRef{MessageID: args[0], OperationID: a.opID})
			if err != nil {
				return err
			}
			return a.emit(m, func(w io.Writer) {
				fmt.Fprintf(w, "%s: delivery %s\n", m.Envelope.MessageID, m.Status.Runtime)
			})
		}),
	})
}

// ---- formatting ----

func addrText(ad c.Address) string {
	switch ad.Kind {
	case c.AddrOwner:
		return "owner"
	case c.AddrSession:
		return "session " + ad.ID
	}
	return string(ad.Kind) + " " + ad.ID
}

func principalText(p c.Principal) string {
	if p.Kind == c.PrincipalOwner {
		return "owner"
	}
	s := string(p.Kind) + " " + p.ID
	if p.Name != "" {
		s += " (" + p.Name + ")"
	}
	return s
}

// factLines renders the independent delivery facts; they are never
// collapsed into one progress state.
func factLines(m c.MessageView) []string {
	st := m.Status
	var out []string
	switch st.Storage {
	case c.StorageStored:
		out = append(out, "stored:        yes, on the hub "+agoPtr(st.StoredAt))
	case c.StoragePendingLocal:
		out = append(out, "stored:        not yet: in a local outbox only (the hub has not accepted it)")
	case c.StorageRejected:
		out = append(out, "stored:        REJECTED by the hub")
	default:
		out = append(out, "stored:        "+string(st.Storage))
	}
	reason := ""
	if st.RuntimeReason != "" {
		reason = " (" + st.RuntimeReason + ")"
	}
	run := ""
	if st.BoundRunID != "" {
		run = " on run " + st.BoundRunID
	}
	switch st.Runtime {
	case c.DeliveryNotApplicable:
		out = append(out, "runtime:       n/a (not injected into a runtime)")
	case c.DeliveryQueued:
		out = append(out, "runtime:       queued, not yet delivered"+reason)
	case c.DeliveryNodeReceived:
		out = append(out, "runtime:       on the recipient's node, not yet delivered"+reason)
	case c.DeliveryAttempting:
		out = append(out, "runtime:       delivery attempt in progress"+run+reason)
	case c.DeliveryAccepted:
		ev, caveat := "", " (not proof it was read or done)"
		if n := len(st.Receipts); n > 0 {
			ev = ", evidence " + string(st.Receipts[n-1].Evidence)
			if st.Receipts[n-1].Evidence == c.EvidenceMock {
				ev += " (mock, not runtime evidence)"
			}
			if c.PullEvidence(st.Receipts[n-1].Evidence) {
				caveat = " (the agent fetched it with the inbox tool; not proof it was done)"
			}
		}
		out = append(out, "runtime:       accepted by the recipient's input"+run+ev+caveat)
	case c.DeliveryRejected:
		out = append(out, "runtime:       rejected"+run+reason+"; nothing was executed, safe to reissue")
	case c.DeliveryIndeterminate:
		out = append(out, "runtime:       INDETERMINATE"+run+reason+": it may or may not have been received.",
			"               Do not resend. Inspect (pyre read), then `pyre resolve-delivery "+m.Envelope.MessageID+" accepted|rejected`",
			"               or `pyre reissue "+m.Envelope.MessageID+" --accept-duplicate-risk`.")
	case c.DeliveryExpired:
		out = append(out, "runtime:       expired before delivery"+reason)
	case c.DeliveryCancelled:
		out = append(out, "runtime:       cancelled before any attempt")
	default:
		out = append(out, "runtime:       "+string(st.Runtime)+reason)
	}
	if st.ConsumedAt != nil {
		out = append(out, "consumed:      taken into a turn "+agoPtr(st.ConsumedAt))
	}
	switch st.Ack {
	case c.AckReplied:
		out = append(out, "acknowledged:  replied ("+st.AckRef+")")
	case c.AckAcknowledged:
		out = append(out, "acknowledged:  yes"+refSuffix(st.AckRef))
	default:
		s := "acknowledged:  no"
		if m.Envelope.RequestAck {
			s += " (acknowledgement requested)"
		}
		out = append(out, s)
	}
	if st.OutcomeRef != nil {
		out = append(out, "outcome:       "+string(st.OutcomeRef.Kind)+" "+st.OutcomeRef.ID)
	}
	return out
}

func refSuffix(r string) string {
	if r == "" {
		return ""
	}
	return " (" + r + ")"
}

func printFacts(w io.Writer, m c.MessageView) {
	for _, l := range factLines(m) {
		fmt.Fprintln(w, "  "+l)
	}
}

func printAttachments(w io.Writer, as []c.Attachment) {
	for _, at := range as {
		switch {
		case at.ArtifactVersionID != "":
			fmt.Fprintf(w, "  attachment: artifact version %s (pyre artifact get %s)\n", at.ArtifactVersionID, at.ArtifactVersionID)
		case at.DocumentRevisionID != "":
			fmt.Fprintf(w, "  attachment: document revision %s\n", at.DocumentRevisionID)
		case at.NoteID != "":
			fmt.Fprintf(w, "  attachment: note %s\n", at.NoteID)
		}
	}
}

func printMessage(w io.Writer, m c.MessageView) {
	e := m.Envelope
	fmt.Fprintf(w, "%s  %s -> %s  (%s)\n", e.MessageID, principalText(e.Sender), addrText(e.Recipient), e.Mode)
	if e.Sender.OnBehalfOf != nil {
		fmt.Fprintf(w, "  on behalf of: %s\n", principalText(*e.Sender.OnBehalfOf))
	}
	if e.ThreadID != "" || e.InReplyTo != "" || e.Reissues != "" {
		fmt.Fprintf(w, "  thread %s  reply-to %s  reissues %s\n", orDash(e.ThreadID), orDash(e.InReplyTo), orDash(e.Reissues))
	}
	fmt.Fprintf(w, "  project %s  created %s  inbox cursor %d\n", strPtr(e.ProjectID), stamp(e.CreatedAt), m.Cursor)
	printAttachments(w, e.Attachments)
	fmt.Fprintln(w, "  ----")
	for _, l := range strings.Split(strings.TrimRight(e.Body.Text, "\n"), "\n") {
		fmt.Fprintln(w, "  "+l)
	}
	fmt.Fprintln(w, "  ----")
	printFacts(w, m)
	for _, r := range m.Status.Receipts {
		fmt.Fprintf(w, "  receipt %s: %s on run %s (node %s) evidence %s %s %s\n", r.AttemptID, r.State, r.RunID, r.NodeID, r.Evidence, orDash(r.EvidenceRef), ago(r.ObservedAt))
	}
}

func deliveryShort(m c.MessageView) string {
	st := m.Status
	s := string(st.Runtime)
	if st.Storage != c.StorageStored {
		s = string(st.Storage) + "/" + s
	}
	if st.Runtime == c.DeliveryIndeterminate {
		s = "INDETERMINATE"
	}
	return s
}

func printMessages(w io.Writer, ms []c.MessageView) {
	var rows [][]string
	for _, m := range ms {
		ack := string(m.Status.Ack)
		if m.Status.Ack == c.AckNone && m.Envelope.RequestAck {
			ack = "requested"
		}
		rows = append(rows, []string{fmt.Sprint(m.Cursor), m.Envelope.MessageID, principalText(m.Envelope.Sender), addrText(m.Envelope.Recipient),
			orDash(m.Envelope.ThreadID), ago(m.Envelope.CreatedAt), deliveryShort(m), ack, clip(m.Envelope.Body.Text, 50)})
	}
	table(w, "CURSOR\tID\tFROM\tTO\tTHREAD\tAGE\tDELIVERY\tACK\tTEXT", rows)
}
