package wsimport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Alias maps a legacy mailbox to a Pyre address.
type Alias struct {
	Kind contracts.AddressKind `json:"kind"`
	ID   string                `json:"id"`
	Note string                `json:"note,omitempty"` // operator hint (template only)
}

// AliasMap is operator-supplied: which Pyre address each legacy mailbox
// (stream slug, "hq") and channel becomes.
type AliasMap struct {
	Version   int              `json:"version"`
	Mailboxes map[string]Alias `json:"mailboxes"`
	// Channels maps legacy channel names to Pyre channel names (default: same).
	Channels map[string]string `json:"channels,omitempty"`
}

// LoadAliasMap reads an alias map file.
func LoadAliasMap(path string) (AliasMap, error) {
	var am AliasMap
	data, err := os.ReadFile(path)
	if err != nil {
		return am, err
	}
	if err := json.Unmarshal(data, &am); err != nil {
		return am, fmt.Errorf("alias map %s: %w", path, err)
	}
	return am, nil
}

// Template proposes an alias map for every mailbox in the tree: hq is the
// owner's inbox; streams need an operator decision (a Pyre session or
// project), with the stream's harness/session recorded as a hint.
func Template(t *Tree) AliasMap {
	am := AliasMap{Version: 1, Mailboxes: map[string]Alias{}, Channels: map[string]string{}}
	for _, mb := range t.Mailboxes {
		if mb == "hq" {
			am.Mailboxes[mb] = Alias{Kind: contracts.AddrOwner, ID: "owner", Note: "ws hq = the human's mailbox"}
			continue
		}
		s := t.Streams[mb]
		hint := "set kind=session id=ses_... (enrolled session) or kind=project id=prj_..."
		if s.Harness != "" || s.HarnessSessionID != "" {
			hint += fmt.Sprintf("; ws stream harness=%s harness_session=%s status=%s", s.Harness, s.HarnessSessionID, s.Status)
		}
		am.Mailboxes[mb] = Alias{Note: hint}
	}
	for name := range t.Channels {
		am.Channels[name] = name
	}
	return am
}

// Options control planning.
type Options struct {
	Aliases AliasMap
	// DefaultRecipient for mailboxes without a usable alias: "skip"
	// (default; counted as unresolved) or "owner" (archived to the owner's
	// inbox as info, recipient recorded in the header).
	DefaultRecipient string
	// UnreadMode for never-delivered messages to a session: "queue"
	// (default; Pyre takes over delivery at cutover) or "info".
	UnreadMode string
}

// Item is one planned import write.
type Item struct {
	Source     string                       `json:"source"` // "mail" | "channel"
	LegacyID   string                       `json:"legacy_id"`
	Mailbox    string                       `json:"mailbox,omitempty"`
	Channel    string                       `json:"channel,omitempty"`
	File       string                       `json:"file,omitempty"`
	State      string                       `json:"state"`                // unread | read | channel
	Obligation string                       `json:"obligation,omitempty"` // open | settled
	ImportID   string                       `json:"message_id"`
	Request    contracts.SendMessageRequest `json:"-"`
	CreatedAt  time.Time                    `json:"-"`
	order      int
}

// Report is the dry-run inventory and plan summary (counts; mailbox names
// only in PerMailbox, for the operator's local report).
type Report struct {
	Root    string    `json:"root"`
	Digest  string    `json:"digest"`
	TakenAt time.Time `json:"taken_at"`

	Streams                 int `json:"streams"`
	Mailboxes               int `json:"mailboxes"`
	MailboxesWithoutStream  int `json:"mailboxes_without_stream"`
	Messages                int `json:"messages"`
	Unread                  int `json:"unread"`
	Read                    int `json:"read"`
	PendingObligations      int `json:"pending_obligations"`
	ExpectReply             int `json:"expect_reply"`
	WithReplyTo             int `json:"with_reply_to"`
	MissingReplyParent      int `json:"missing_reply_parent"`
	MissingID               int `json:"missing_id"`
	NoFrontmatter           int `json:"no_frontmatter"`
	Channels                int `json:"channels"`
	ChannelPosts            int `json:"channel_posts"`
	Subscriptions           int `json:"subscriptions"`
	ChannelCursors          int `json:"channel_cursors"`
	UnreadChannelDeliveries int `json:"unread_channel_deliveries"` // (subscriber, post) pairs past the cursor

	Requests      int            `json:"requests"`
	QueueRequests int            `json:"queue_requests"`
	InfoRequests  int            `json:"info_requests"`
	RequestAck    int            `json:"request_ack"`
	Skipped       int            `json:"skipped"`
	AlreadyInPyre int            `json:"already_in_pyre"`      // rollback write-backs carrying pyre_message_id
	Unresolved    map[string]int `json:"unresolved,omitempty"` // mailbox -> skipped messages
	PerMailbox    []MailboxCount `json:"per_mailbox"`
	Problems      []string       `json:"problems,omitempty"`
}

type MailboxCount struct {
	Mailbox string `json:"mailbox"`
	Unread  int    `json:"unread"`
	Read    int    `json:"read"`
	Pending int    `json:"pending"`
	Address string `json:"address,omitempty"`
}

// Plan is the deterministic import plan for one tree snapshot.
type Plan struct {
	Report Report `json:"report"`
	Items  []Item `json:"items"`
}

func hash24(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:12])
}

// MailImportID is the deterministic Pyre message id for a legacy message.
func MailImportID(mailbox, key string) string {
	return contracts.PrefixMessage + "_legacy-ws-" + hash24("ws-mail", mailbox, key)
}

// ChannelImportID is the deterministic Pyre message id for a channel post.
func ChannelImportID(channel, legacyID string, index int) string {
	if legacyID == "" {
		legacyID = fmt.Sprintf("index:%d", index)
	}
	return contracts.PrefixMessage + "_legacy-ws-" + hash24("ws-chan", channel, legacyID)
}

func opIDFor(importID string) string {
	return contracts.PrefixOperation + "_" + strings.TrimPrefix(importID, contracts.PrefixMessage+"_")
}

func threadIDFor(rootImportID string) string {
	return contracts.PrefixThread + "_" + strings.TrimPrefix(rootImportID, contracts.PrefixMessage+"_")
}

// Build creates the plan. It is pure: the same tree and options always give
// byte-identical requests, which is what makes --apply idempotent.
func Build(t *Tree, opts Options) *Plan {
	if opts.DefaultRecipient == "" {
		opts.DefaultRecipient = "skip"
	}
	if opts.UnreadMode == "" {
		opts.UnreadMode = "queue"
	}
	rep := Report{Root: t.Root, Digest: t.Digest, TakenAt: t.TakenAt, Streams: len(t.Streams), Mailboxes: len(t.Mailboxes),
		Unresolved: map[string]int{}, Problems: t.Problems}
	for _, mb := range t.Mailboxes {
		if _, ok := t.Streams[mb]; !ok && mb != "hq" {
			rep.MailboxesWithoutStream++
		}
	}

	resolve := func(mb string) (contracts.Address, bool) {
		if a, ok := opts.Aliases.Mailboxes[mb]; ok && a.Kind != "" && a.ID != "" {
			return contracts.Address{Kind: a.Kind, ID: a.ID}, true
		}
		if mb == "hq" {
			return contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}, true
		}
		if opts.DefaultRecipient == "owner" {
			return contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}, false
		}
		return contracts.Address{}, false
	}

	// Index legacy ids -> messages for reply resolution.
	byLegacy := map[string][]int{}
	for i, m := range t.Messages {
		if m.ID != "" {
			byLegacy[m.ID] = append(byLegacy[m.ID], i)
		}
	}
	parentOf := func(i int) (int, bool) {
		m := t.Messages[i]
		if m.ReplyTo == "" {
			return 0, false
		}
		cands := byLegacy[m.ReplyTo]
		if len(cands) == 0 {
			return 0, false
		}
		for _, c := range cands { // the parent was received by the replier
			if t.Messages[c].Mailbox == m.From {
				return c, true
			}
		}
		return cands[0], true
	}
	hasChild := map[int]bool{}
	for i := range t.Messages {
		if p, ok := parentOf(i); ok {
			hasChild[p] = true
		}
	}
	rootOf := func(i int) int {
		seen := map[int]bool{}
		for !seen[i] {
			seen[i] = true
			p, ok := parentOf(i)
			if !ok {
				return i
			}
			i = p
		}
		return i
	}

	perMB := map[string]*MailboxCount{}
	for _, mb := range t.Mailboxes {
		mc := &MailboxCount{Mailbox: mb}
		if a, ok := resolve(mb); ok {
			mc.Address = string(a.Kind) + ":" + a.ID
		}
		perMB[mb] = mc
	}
	importIDs := make([]string, len(t.Messages))
	for i, m := range t.Messages {
		importIDs[i] = MailImportID(m.Mailbox, m.Key())
	}
	included := map[int]bool{}
	var items []Item
	for i, m := range t.Messages {
		rep.Messages++
		mc := perMB[m.Mailbox]
		if m.State == "unread" {
			rep.Unread++
			mc.Unread++
		} else {
			rep.Read++
			mc.Read++
		}
		if m.Pending {
			rep.PendingObligations++
			mc.Pending++
		}
		if m.ExpectReply {
			rep.ExpectReply++
		}
		if m.ReplyTo != "" {
			rep.WithReplyTo++
		}
		if m.ID == "" {
			rep.MissingID++
		}
		if m.NoFront {
			rep.NoFrontmatter++
		}
		if pid := m.Extra["pyre_message_id"]; contracts.ValidID(pid, contracts.PrefixMessage) {
			// Written back by a rollback (MIGRATION.md step 7): the hub already
			// holds this message; importing it again would duplicate it.
			rep.AlreadyInPyre++
			continue
		}
		addr, mapped := resolve(m.Mailbox)
		if addr.Kind == "" {
			rep.Skipped++
			rep.Unresolved[m.Mailbox]++
			continue
		}
		included[i] = true
		obligation := ""
		switch {
		case m.ExpectReply && (m.Pending || m.State == "unread"):
			// ws creates the pending/ copy on delivery; an unread message's
			// obligation is still ahead of it, and Pyre now delivers it.
			obligation = "open"
		case m.ExpectReply:
			obligation = "settled"
		case m.Pending:
			obligation = "open" // obligation copy without the flag: keep it visible
		}
		mode := contracts.ModeInfo
		if m.State == "unread" && addr.Kind == contracts.AddrSession && opts.UnreadMode == "queue" {
			mode = contracts.ModeQueue
		}
		items = append(items, Item{Source: "mail", LegacyID: m.Key(), Mailbox: m.Mailbox, File: m.File, State: m.State,
			Obligation: obligation, ImportID: importIDs[i], CreatedAt: m.CreatedAt, order: i,
			Request: contracts.SendMessageRequest{
				Mutation:   contracts.Mutation{OperationID: opIDFor(importIDs[i])},
				MessageID:  importIDs[i],
				Recipient:  addr,
				Mode:       mode,
				RequestAck: obligation == "open",
			}})
		_ = mapped
	}
	// Second pass: reply relations and headers (need the included set).
	for k := range items {
		it := &items[k]
		i := it.order
		m := t.Messages[i]
		addr, mapped := resolve(m.Mailbox)
		replyNote := ""
		if m.ReplyTo != "" {
			if p, ok := parentOf(i); ok {
				if included[p] {
					it.Request.InReplyTo = importIDs[p]
					replyNote = fmt.Sprintf("%s (imported as %s)", m.ReplyTo, importIDs[p])
				} else {
					replyNote = m.ReplyTo + " (parent not imported: unresolved recipient)"
				}
			} else {
				rep.MissingReplyParent++
				replyNote = m.ReplyTo + " (parent not found in the legacy archive)"
			}
		}
		if _, ok := parentOf(i); ok || hasChild[i] {
			it.Request.ThreadID = threadIDFor(importIDs[rootOf(i)])
		}
		it.Request.Body = contracts.MessageBody{Text: mailHeader(m, addr, mapped, replyNote, it.Obligation) + m.Body}
	}

	// Channels.
	names := make([]string, 0, len(t.Channels))
	for n := range t.Channels {
		names = append(names, n)
	}
	sort.Strings(names)
	rep.Channels = len(names)
	subs := map[string][]Stream{}
	for _, s := range t.Streams {
		rep.ChannelCursors += len(s.Cursors)
		for _, c := range s.Channels {
			rep.Subscriptions++
			subs[c] = append(subs[c], s)
		}
	}
	for _, n := range names {
		sort.Slice(subs[n], func(a, b int) bool { return subs[n][a].Slug < subs[n][b].Slug })
		target := n
		if v, ok := opts.Aliases.Channels[n]; ok && v != "" {
			target = v
		}
		for _, p := range t.Channels[n] {
			rep.ChannelPosts++
			var seenBy, unseen []string
			for _, s := range subs[n] {
				if s.Slug == p.Sender {
					continue
				}
				if s.Cursors[n] > p.Offset {
					seenBy = append(seenBy, s.Slug)
				} else {
					unseen = append(unseen, s.Slug)
					rep.UnreadChannelDeliveries++
				}
			}
			id := ChannelImportID(n, p.ID, p.Index)
			items = append(items, Item{Source: "channel", LegacyID: p.ID, Channel: n, State: "channel", ImportID: id,
				CreatedAt: p.CreatedAt, order: len(t.Messages) + len(items),
				Request: contracts.SendMessageRequest{
					Mutation:  contracts.Mutation{OperationID: opIDFor(id)},
					MessageID: id,
					Recipient: contracts.Address{Kind: contracts.AddrChannel, ID: target},
					Mode:      contracts.ModeInfo,
					Body:      contracts.MessageBody{Text: channelHeader(p, seenBy, unseen) + p.Body},
				}})
		}
	}

	// Order: parents before replies, then by time, then deterministic key.
	sort.SliceStable(items, func(a, b int) bool {
		ta, tb := items[a].CreatedAt, items[b].CreatedAt
		if !ta.Equal(tb) {
			if ta.IsZero() || tb.IsZero() {
				return !ta.IsZero()
			}
			return ta.Before(tb)
		}
		return items[a].ImportID < items[b].ImportID
	})
	items = parentsFirst(items)
	for _, it := range items {
		rep.Requests++
		if it.Request.Mode == contracts.ModeQueue {
			rep.QueueRequests++
		} else {
			rep.InfoRequests++
		}
		if it.Request.RequestAck {
			rep.RequestAck++
		}
	}
	for _, mb := range t.Mailboxes {
		rep.PerMailbox = append(rep.PerMailbox, *perMB[mb])
	}
	if len(rep.Unresolved) == 0 {
		rep.Unresolved = nil
	}
	return &Plan{Report: rep, Items: items}
}

// parentsFirst moves any reply after its parent (stable otherwise).
func parentsFirst(items []Item) []Item {
	pos := map[string]int{}
	for i, it := range items {
		pos[it.ImportID] = i
	}
	out := make([]Item, 0, len(items))
	placed := map[string]bool{}
	var place func(i int, depth int)
	place = func(i int, depth int) {
		it := items[i]
		if placed[it.ImportID] {
			return
		}
		if p, ok := pos[it.Request.InReplyTo]; ok && depth < 1000 {
			place(p, depth+1)
		}
		placed[it.ImportID] = true
		out = append(out, it)
	}
	for i := range items {
		place(i, 0)
	}
	return out
}

func mailHeader(m Message, addr contracts.Address, mapped bool, replyNote, obligation string) string {
	var b strings.Builder
	b.WriteString("[legacy ws mail, imported by pyre]\n")
	from := m.From
	if from == "" {
		from = "(unknown)"
	}
	fmt.Fprintf(&b, "from: %s\n", from)
	to := m.Mailbox
	if !mapped {
		to += fmt.Sprintf(" (no alias; archived to %s:%s)", addr.Kind, addr.ID)
	}
	fmt.Fprintf(&b, "to: %s\n", to)
	if m.Created != "" {
		fmt.Fprintf(&b, "sent: %s\n", m.Created)
	} else {
		b.WriteString("sent: (unknown)\n")
	}
	fmt.Fprintf(&b, "legacy-id: %s\n", m.Key())
	if m.Kind != "" || m.Sev != "" {
		kind := m.Kind
		if kind == "" {
			kind = "fyi"
		}
		line := "kind: " + kind
		if m.Sev != "" {
			line += "; severity: " + m.Sev
		}
		b.WriteString(line + "\n")
	}
	if replyNote != "" {
		fmt.Fprintf(&b, "reply-to: %s\n", replyNote)
	}
	switch m.State {
	case "unread":
		b.WriteString("legacy-delivery: unread (never returned by ws; Pyre owns delivery from cutover)\n")
	default:
		b.WriteString("legacy-delivery: read (observation only: ws marked it read when a hook or inbox call returned its text; not evidence that a model acted on it)\n")
	}
	switch obligation {
	case "open":
		b.WriteString("reply-obligation: open (reply expected; none recorded in the legacy archive)\n")
	case "settled":
		b.WriteString("reply-obligation: settled in ws (reply sent or obligation cleared)\n")
	}
	b.WriteString("---\n")
	return b.String()
}

func channelHeader(p ChannelPost, seenBy, unseen []string) string {
	var b strings.Builder
	b.WriteString("[legacy ws channel post, imported by pyre]\n")
	fmt.Fprintf(&b, "channel: #%s\nfrom: %s\nsent: %s\nlegacy-id: %s\n", p.Channel, p.Sender, p.Created, p.ID)
	if len(seenBy) > 0 {
		fmt.Fprintf(&b, "legacy-delivery: returned to %s (cursor observation, not acknowledgement)\n", strings.Join(seenBy, ", "))
	}
	if len(unseen) > 0 {
		fmt.Fprintf(&b, "legacy-pending-for: %s\n", strings.Join(unseen, ", "))
	}
	b.WriteString("---\n")
	return b.String()
}
