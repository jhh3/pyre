package cli

// Compact rendering of the MCP read tools (W-compact). Agents call inbox,
// list_sessions and friends often (doorbell nudges, waiting for replies);
// every full JSON envelope (receipts, cursors, freshness, capabilities)
// lands in their context. Read tools therefore answer with a few lines of
// text by default and keep the ids a follow-up call needs (message id for
// get_message/ack/reply, session id, approval id, next cursor). The
// argument format: "json" returns the complete contract result, as before,
// with structuredContent.
//
// A compact tool declares no outputSchema: the MCP spec requires
// structuredContent conforming to a declared schema, and clients that
// prefer structuredContent over text (Codex does) would show the model the
// full JSON again.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/digest"
)

// compactText is a rendered read-tool result: call() sends it as the only
// text block, without structuredContent.
type compactText string

// fullResult is a read tool's contract result requested with format:
// "json"; call() sends it as JSON text plus structuredContent.
type fullResult struct{ v any }

func (f fullResult) MarshalJSON() ([]byte, error) { return json.Marshal(f.v) }

var formatProp = map[string]any{"type": "string", "enum": []any{"text", "json"},
	"description": "text (default): compact lines with the ids you need; json: the full structured result"}

// compactly makes t answer with render's text unless format is "json".
// Errors keep their JSON shape (rare, and their details matter).
func compactly[R any](t MCPTool, render func(rc *renderCtx, r R) string) MCPTool {
	inner := t.run
	t.OutputSchema = nil
	if p, ok := t.InputSchema["properties"].(map[string]any); ok {
		p["format"] = formatProp
	}
	t.run = func(ctx context.Context, o *Ops, args json.RawMessage) (any, error) {
		args, format, err := takeFormat(t.Name, args)
		if err != nil {
			return nil, err
		}
		out, err := inner(ctx, o, args)
		if err != nil {
			return out, err
		}
		if format == "json" {
			return fullResult{out}, nil
		}
		r, _ := out.(R)
		rc := newRenderCtx(ctx, o)
		rc.args = args
		return compactText(render(rc, r)), nil
	}
	return t
}

// takeFormat removes the format argument before the strict decode.
func takeFormat(tool string, args json.RawMessage) (json.RawMessage, string, error) {
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 || string(trimmed) == "null" || !bytes.Contains(trimmed, []byte(`"format"`)) {
		return args, "text", nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return args, "text", nil // the strict decode reports it
	}
	raw, ok := m["format"]
	if !ok {
		return args, "text", nil
	}
	var f string
	if err := json.Unmarshal(raw, &f); err != nil || (f != "" && f != "text" && f != "json") {
		return nil, "", usagef("invalid arguments for %s: format must be \"text\" or \"json\"", tool)
	}
	delete(m, "format")
	b, _ := json.Marshal(m)
	if f == "" {
		f = "text"
	}
	return b, f, nil
}

// ---- render context ----

// renderCtx resolves the few names a compact line shows (session titles and
// machines of message senders), with at most one batched lookup.
type renderCtx struct {
	ctx      context.Context
	o        *Ops
	now      time.Time
	sessions map[string]*c.SessionView
	args     json.RawMessage // the call's arguments (without format)
}

func newRenderCtx(ctx context.Context, o *Ops) *renderCtx {
	return &renderCtx{ctx: ctx, o: o, now: time.Now(), sessions: map[string]*c.SessionView{}}
}

// at measures ages against ref (a server snapshot time) when known.
func (rc *renderCtx) at(ref time.Time) *renderCtx {
	if !ref.IsZero() {
		rc.now = ref
	}
	return rc
}

func (rc *renderCtx) ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := rc.now.Sub(t)
	if d < 0 {
		d = 0
	}
	return short(d) + " ago"
}

// maxLookups bounds the per-call session lookups when the hub has no
// batched get (each is one request).
const maxLookups = 12

// load fetches the named sessions not yet known. Best effort: an unknown or
// unreachable session just renders by id.
func (rc *renderCtx) load(ids []string) {
	var need []string
	seen := map[string]bool{}
	for _, id := range ids {
		if _, ok := rc.sessions[id]; ok || seen[id] || !c.ValidID(id, c.PrefixSession) {
			continue
		}
		seen[id] = true
		need = append(need, id)
	}
	if len(need) == 0 || rc.o == nil || rc.o.API == nil {
		return
	}
	for _, id := range need {
		rc.sessions[id] = nil
	}
	if b, ok := rc.o.API.(c.SessionBatchAPI); ok {
		if len(need) > c.MaxGetManySessions {
			need = need[:c.MaxGetManySessions]
		}
		if r, err := b.GetManySessions(rc.ctx, c.GetManySessionsRequest{IDs: need}); err == nil {
			for i := range r.Items {
				sv := r.Items[i]
				rc.sessions[sv.Session.SessionID] = &sv
			}
			return
		}
	}
	for i, id := range need {
		if i >= maxLookups {
			break
		}
		if sv, err := rc.o.API.GetSession(rc.ctx, c.GetRequest{ID: id}); err == nil {
			rc.sessions[id] = &sv
		}
	}
}

func (rc *renderCtx) selfNode() string {
	if rc.o == nil {
		return ""
	}
	return rc.o.Self.NodeID
}

// nodeOf names a session's machine and whether it is another machine than
// the caller's (always "other" for a caller without a node, e.g. the owner).
func (rc *renderCtx) nodeOf(sv *c.SessionView) (name string, other bool) {
	if sv == nil || sv.Run == nil {
		return "", false
	}
	name = sv.NodeName
	if name == "" {
		name = sv.Run.NodeID
	}
	return name, sv.Run.NodeID != rc.selfNode()
}

// who renders a principal: a session as its title (plus its machine when it
// is another machine than the caller's), the owner as "owner".
func (rc *renderCtx) who(p c.Principal) (text, otherNode string) {
	switch p.Kind {
	case c.PrincipalOwner:
		return "owner", ""
	case c.PrincipalSession:
		if p.ID == rc.o.Self.SessionID && p.ID != "" {
			return "you", ""
		}
		sv := rc.sessions[p.ID]
		title := p.Name
		if sv != nil && sv.Session.Title != "" {
			title = sv.Session.Title
		}
		if title == "" {
			title = p.ID
		}
		if n, other := rc.nodeOf(sv); other && n != "" {
			return title + " (" + n + ")", n
		}
		return title, ""
	}
	if p.Name != "" {
		return p.Name, ""
	}
	return string(p.Kind) + " " + p.ID, ""
}

func (rc *renderCtx) addr(a c.Address) string {
	switch a.Kind {
	case c.AddrOwner:
		return "owner"
	case c.AddrSession:
		t, _ := rc.who(c.Principal{Kind: c.PrincipalSession, ID: a.ID})
		return t
	}
	return string(a.Kind) + ":" + a.ID
}

// ---- snippets that keep paths and links whole ----

// refRe matches what a reader must copy exactly: absolute or home paths,
// URLs (incl. pyre://), and published object ids.
var refRe = regexp.MustCompile(`^["'(\[<]*((?:~?/|[a-z][a-z0-9+.-]*://)\S*[^\s"'),.;:\]>]|(?:ver|rev|note|art|doc)_[A-Za-z0-9_-]+)`)

func refOf(word string) string {
	m := refRe.FindStringSubmatch(word)
	if m == nil || len(m[1]) < 2 {
		return ""
	}
	return m[1]
}

// snippet renders text on one line in about max runes. A path or link
// crossing the limit is kept whole, and up to two refs beyond the cut are
// appended, so a truncated line never hides or breaks a path.
func snippet(text string, max int) (string, bool) {
	words := strings.Fields(text)
	var b strings.Builder
	n := 0
	i := 0
	cut := false
	for ; i < len(words); i++ {
		w := words[i]
		wl := len([]rune(w))
		sep := 0
		if n > 0 {
			sep = 1
		}
		if n+sep+wl > max {
			cut = true
			if refOf(w) != "" && wl <= 400 {
				if sep == 1 {
					b.WriteByte(' ')
				}
				b.WriteString(w)
				i++
			} else if room := max - n - sep; room > 8 {
				if sep == 1 {
					b.WriteByte(' ')
				}
				b.WriteString(string([]rune(w)[:room]))
			}
			break
		}
		if sep == 1 {
			b.WriteByte(' ')
		}
		b.WriteString(w)
		n += sep + wl
	}
	if !cut {
		return b.String(), false
	}
	b.WriteString(" …")
	var extra []string
	for _, w := range words[i:] {
		if r := refOf(w); r != "" && len(extra) < 2 && !hasStr(extra, r) {
			extra = append(extra, r)
		}
	}
	if len(extra) > 0 {
		b.WriteString(" [refs: " + strings.Join(extra, " ") + "]")
	}
	return b.String(), true
}

func hasStr(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

var absPathRe = regexp.MustCompile(`(?:^|[\s"'(\[<=:])(?:~?/[A-Za-z0-9._-]+/)`)

func hasLocalPath(text string) bool { return absPathRe.MatchString(text) }

// ---- messages ----

func attachText(as []c.Attachment) string {
	var ids []string
	for _, a := range as {
		ids = append(ids, firstSet(a.ArtifactVersionID, a.DocumentRevisionID, a.NoteID))
	}
	if len(ids) == 0 {
		return ""
	}
	return " [attached: " + strings.Join(ids, " ") + "]"
}

func firstSet(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// msgState is the reader-relevant state of a message: acks for mail to
// the caller, delivery and acks for mail the caller sent.
func msgState(m c.MessageView, sent bool) string {
	st := m.Status
	var parts []string
	if sent {
		d := string(st.Runtime)
		if st.Storage != c.StorageStored {
			d = string(st.Storage)
		}
		if st.Runtime == c.DeliveryIndeterminate {
			d = "INDETERMINATE (unknown, not failed)"
		}
		if d != string(c.DeliveryNotApplicable) {
			parts = append(parts, d)
		}
	}
	switch {
	case st.Ack == c.AckReplied:
		parts = append(parts, "replied")
	case st.Ack == c.AckAcknowledged:
		parts = append(parts, "acked")
	case m.Envelope.RequestAck:
		parts = append(parts, "ack requested")
	}
	if len(parts) == 0 {
		return ""
	}
	return " · " + strings.Join(parts, ", ")
}

func (rc *renderCtx) loadMessageParties(ms []c.MessageView) {
	var ids []string
	for _, m := range ms {
		if m.Envelope.Sender.Kind == c.PrincipalSession {
			ids = append(ids, m.Envelope.Sender.ID)
		}
		if m.Envelope.Recipient.Kind == c.AddrSession {
			ids = append(ids, m.Envelope.Recipient.ID)
		}
	}
	rc.load(ids)
}

func (rc *renderCtx) sentByMe(m c.MessageView) bool {
	s := m.Envelope.Sender
	if rc.o.Self.SessionID != "" {
		return s.Kind == c.PrincipalSession && s.ID == rc.o.Self.SessionID
	}
	return s.Kind == c.PrincipalOwner && m.Envelope.Recipient.Kind != c.AddrOwner
}

// messageLine: "[pyre-msg:<id>] from <sender> · 3m ago · re <id> · <snippet>".
// The [pyre-msg:<id>] marker is the one delivered input carries (and the
// Claude doorbell hook reads it as inbox-read evidence).
func (rc *renderCtx) messageLine(m c.MessageView, max int) (string, bool) {
	e := m.Envelope
	sent := rc.sentByMe(m)
	var who string
	if sent {
		who = "to " + rc.addr(e.Recipient)
	} else {
		w, _ := rc.who(e.Sender)
		who = "from " + w
		if e.Recipient.Kind != c.AddrSession || e.Recipient.ID != rc.o.Self.SessionID {
			who += " to " + rc.addr(e.Recipient)
		}
	}
	line := c.MessageMarker(e.MessageID) + " " + who + " · " + rc.ago(e.CreatedAt)
	if e.InReplyTo != "" {
		line += " · re " + e.InReplyTo
	}
	line += msgState(m, sent)
	text, cut := snippet(e.Body.Text, max)
	return line + " · " + text + attachText(e.Attachments), cut
}

const snippetRunes = 160

func renderInbox(rc *renderCtx, pg c.Page[c.MessageView]) string {
	rc.at(pg.SnapshotAt)
	var p mcpInboxParams
	_ = json.Unmarshal(rc.args, &p)
	unacked := p.effective().Unacked
	rc.loadMessageParties(pg.Items)
	var b strings.Builder
	anyCut := false
	for _, m := range pg.Items {
		l, cut := rc.messageLine(m, snippetRunes)
		anyCut = anyCut || cut
		b.WriteString(l + "\n")
	}
	n := len(pg.Items)
	what := "message"
	if unacked {
		what = "unacked message"
	}
	if n == 0 {
		return "no " + what + "s\n"
	}
	foot := fmt.Sprintf("%d %s", n, what)
	if n != 1 {
		foot += "s"
	}
	if anyCut {
		foot += " · full text: get_message <id>"
	}
	if pg.NextCursor != "" {
		foot += " · more: raise limit"
	}
	b.WriteString(foot + "\n")
	return b.String()
}

func renderMessage(rc *renderCtx, m c.MessageView) string {
	rc.loadMessageParties([]c.MessageView{m})
	e := m.Envelope
	from, otherNode := rc.who(e.Sender)
	var b strings.Builder
	fmt.Fprintf(&b, "%s from %s to %s · %s", c.MessageMarker(e.MessageID), from, rc.addr(e.Recipient), rc.ago(e.CreatedAt))
	if e.Mode != c.ModeQueue && e.Mode != "" {
		b.WriteString(" · " + string(e.Mode))
	}
	if e.InReplyTo != "" {
		b.WriteString(" · re " + e.InReplyTo)
	}
	if e.ThreadID != "" && e.ThreadID != e.MessageID && e.ThreadID != e.InReplyTo {
		b.WriteString(" · thread " + e.ThreadID)
	}
	if e.Reissues != "" {
		b.WriteString(" · reissues " + e.Reissues)
	}
	// Delivery facts matter to everyone but the recipient reading its mail.
	mine := e.Recipient.Kind == c.AddrSession && e.Recipient.ID == rc.o.Self.SessionID && e.Recipient.ID != ""
	b.WriteString(msgState(m, !mine))
	if !mine && m.Status.RuntimeReason != "" {
		b.WriteString(" (" + m.Status.RuntimeReason + ")")
	}
	if e.Sender.Kind == c.PrincipalSession && e.Sender.ID != rc.o.Self.SessionID {
		b.WriteString(" · sender " + e.Sender.ID)
	}
	b.WriteString("\n")
	if a := attachText(e.Attachments); a != "" {
		b.WriteString(strings.TrimSpace(a) + " (read with pyre artifact get <ver_id>)\n")
	}
	if otherNode != "" && hasLocalPath(e.Body.Text) {
		b.WriteString("note: paths in this message are on " + otherNode + ", not this machine\n")
	}
	b.WriteString("---\n")
	b.WriteString(strings.TrimRight(e.Body.Text, "\n") + "\n")
	return b.String()
}

// ---- sessions ----

func (rc *renderCtx) sessionLine(sv c.SessionView) string {
	s := sv.Session
	title := s.Title
	if s.Icon != "" {
		title = s.Icon + " " + title
	}
	parts := []string{s.SessionID, clip(orDash(title), 60), compactRunState(sv)}
	if n, other := rc.nodeOf(&sv); n != "" {
		if !other {
			n += " (this machine)"
		}
		parts = append(parts, n)
	}
	parts = append(parts, string(s.Harness))
	if sv.Run != nil {
		parts = append(parts, rc.ago(sv.Run.Freshness.ObservedAt))
	} else {
		parts = append(parts, "created "+rc.ago(s.CreatedAt))
	}
	if s.ProjectID != nil && *s.ProjectID != "" {
		parts = append(parts, *s.ProjectID)
	}
	if s.Lineage.ParentSessionID != "" {
		parts = append(parts, "parent "+s.Lineage.ParentSessionID)
	}
	if f := sv.Flag; f != nil {
		parts = append(parts, "needs owner: "+clip(firstSet(f.Text, f.Reason), 60))
	} else if sv.Attention > 0 {
		parts = append(parts, fmt.Sprintf("attention %d", sv.Attention))
	}
	if s.Archived {
		parts = append(parts, "archived")
	}
	return strings.Join(parts, " · ")
}

func compactRunState(sv c.SessionView) string {
	if sv.Run == nil {
		return "no run"
	}
	s := string(sv.Run.State)
	if sv.Freshness.Stale || sv.Run.Freshness.Stale {
		s += "? (stale)"
	} else if a := sv.Run.Activity; a != nil && a.Text != "" && sv.Run.State == c.RunRunning {
		s += ": " + a.Text
	}
	return s
}

func renderSessions(rc *renderCtx, pg c.Page[c.SessionView]) string {
	rc.at(pg.SnapshotAt)
	ss := append([]c.SessionView(nil), pg.Items...)
	sort.SliceStable(ss, func(i, j int) bool { return ss[i].Session.CreatedAt.After(ss[j].Session.CreatedAt) })
	var b strings.Builder
	for _, sv := range ss {
		b.WriteString(rc.sessionLine(sv) + "\n")
	}
	fmt.Fprintf(&b, "%d session", len(ss))
	if len(ss) != 1 {
		b.WriteString("s")
	}
	b.WriteString("\n")
	return b.String()
}

func renderSession(rc *renderCtx, sv c.SessionView) string {
	var b strings.Builder
	b.WriteString(rc.sessionLine(sv) + "\n")
	s := sv.Session
	if s.Lineage.ForkedFrom != "" || s.Lineage.HandoffNoteID != "" {
		fmt.Fprintf(&b, "lineage: forked from %s, handoff %s\n", orDash(s.Lineage.ForkedFrom), orDash(s.Lineage.HandoffNoteID))
	}
	if r := sv.Run; r != nil {
		fmt.Fprintf(&b, "run %s on node %s · cwd %s", r.RunID, r.NodeID, orDash(r.Cwd))
		if r.Checkout != nil {
			fmt.Fprintf(&b, " · branch %s", orDash(r.Checkout.Branch))
		}
		b.WriteString("\n")
		if r.StateReason != "" {
			b.WriteString("state reason: " + r.StateReason + "\n")
		}
		if r.Freshness.Stale {
			fmt.Fprintf(&b, "stale since %s: %s (not a failure; the observer lost contact)\n", rc.ago(r.Freshness.ObservedAt), orDash(r.Freshness.Reason))
		}
		if len(r.PendingApprovalIDs) > 0 {
			b.WriteString("pending approvals: " + strings.Join(r.PendingApprovalIDs, ", ") + "\n")
		}
		if p := r.AdapterData["pyre.permissions"]; p != "" {
			b.WriteString("permissions: " + p + "\n")
		}
		for _, sa := range r.Subagents {
			fmt.Fprintf(&b, "subagent %s %s %s\n", sa.ID, sa.State, clip(sa.Summary, 60))
		}
	}
	return b.String()
}

// ---- other read tools ----

func renderTranscript(_ *renderCtx, resp c.ReadSessionResponse) string {
	var b strings.Builder
	printTranscript(&b, resp, false)
	if resp.NextCursor != "" {
		b.WriteString("next_cursor: " + resp.NextCursor + "\n")
	}
	if b.Len() == 0 {
		return "no transcript entries\n"
	}
	return b.String()
}

func renderApprovals(rc *renderCtx, pg c.Page[c.Approval]) string {
	rc.at(pg.SnapshotAt)
	var b strings.Builder
	for _, ap := range pg.Items {
		opts := make([]string, len(ap.Options))
		for i, o := range ap.Options {
			opts[i] = o.ID
		}
		line := strings.Join([]string{ap.ApprovalID, string(ap.Kind), string(ap.State), ap.SessionID, rc.ago(ap.CreatedAt), clip(ap.Title, 80)}, " · ")
		if len(opts) > 0 && ap.State != c.ApprovalExpired {
			line += " · options " + strings.Join(opts, "/")
		}
		if ap.AllowText {
			line += " · text allowed"
		}
		b.WriteString(line + "\n")
		if ap.State == c.ApprovalExpired {
			fmt.Fprintf(&b, "  remote answer window closed; the prompt waits in its terminal (pyre attach %s)\n", ap.SessionID)
		}
	}
	if len(pg.Items) == 0 {
		return "no approvals\n"
	}
	return b.String()
}

func renderProjects(rc *renderCtx, pg c.Page[c.Project]) string {
	rc.at(pg.SnapshotAt)
	var b strings.Builder
	for _, p := range pg.Items {
		parts := []string{p.ProjectID, p.Name}
		if p.Description != "" {
			parts = append(parts, clip(p.Description, 80))
		}
		if n := len(p.Repos); n > 0 {
			ids := make([]string, n)
			for i, r := range p.Repos {
				ids[i] = r.RepoID
			}
			parts = append(parts, "repos "+strings.Join(ids, ","))
		}
		parts = append(parts, "updated "+rc.ago(p.UpdatedAt))
		if p.Archived {
			parts = append(parts, "archived")
		}
		b.WriteString(strings.Join(parts, " · ") + "\n")
	}
	if len(pg.Items) == 0 {
		return "no projects\n"
	}
	return b.String()
}

func renderNodes(rc *renderCtx, pg c.Page[c.Node]) string {
	var b strings.Builder
	printNodes(&b, append([]c.Node(nil), pg.Items...), pg.SnapshotAt)
	for _, n := range pg.Items {
		if n.NodeID == rc.selfNode() && n.NodeID != "" {
			b.WriteString("you are on " + n.Name + "\n")
		}
	}
	return b.String()
}

func renderSearch(_ *renderCtx, resp c.SearchResponse) string {
	var b strings.Builder
	printSearch(&b, resp, false)
	return b.String()
}

func renderDigest(rc *renderCtx, d digest.Digest) string {
	var b strings.Builder
	digest.WriteText(&b, d, rc.now)
	return b.String()
}

// mcpInboxParams is the MCP inbox input: InboxParams with unacked
// defaulting to true for one's own inbox (handled mail is noise on every
// check); unacked: false lists everything. A thread or an outbox/all
// listing is complete by default.
type mcpInboxParams struct {
	InboxParams
	Unacked *bool `json:"unacked,omitempty"`
}

func (p mcpInboxParams) effective() InboxParams {
	ip := p.InboxParams
	switch {
	case p.Unacked != nil:
		ip.Unacked = *p.Unacked
	default:
		ip.Unacked = ip.Thread == "" && (ip.Box == "" || ip.Box == "inbox")
	}
	return ip
}
