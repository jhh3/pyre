package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

func (c *composeOverlay) send(m *Model) tea.Cmd {
	body := strings.TrimSpace(c.ta.Value())
	if body == "" && len(c.images) == 0 {
		c.errLine = "nothing to send"
		return nil
	}
	if c.sending {
		return nil
	}
	key := string(c.mode) + "\x00" + body
	for _, img := range c.images {
		key += "\x00" + img.sum
	}
	sum := sha256.Sum256([]byte(key))
	// Same compose + same text + same mode + same images => same operation
	// id: a retry after an unknown outcome is deduplicated by the hub, never
	// a second message.
	opID := m.opID("send:" + c.instance + ":" + hex.EncodeToString(sum[:8]))
	req := contracts.SendMessageRequest{
		Mutation:  contracts.Mutation{OperationID: opID},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: c.sid},
		Mode:      c.mode,
		Body:      contracts.MessageBody{Text: body},
		InReplyTo: c.replyTo,
	}
	c.sending = true
	c.errLine = ""
	api, imgs := m.api, c.images
	// Each image is published first (its own stable operation id: a retry
	// resumes or replays the same upload), then the message attaches the
	// versions; the recipient's node fetches them (daemon/attachments.go).
	imgOp := map[string]string{}
	for _, img := range imgs {
		imgOp[img.sum] = m.opID("image:" + c.instance + ":" + img.sum)
	}
	send := func(ctx context.Context) (any, error) {
		if len(imgs) > 0 {
			atts, err := publishImages(ctx, api, imgs, func(img pendingImage) string { return imgOp[img.sum] })
			if err != nil {
				return nil, err
			}
			req.Attachments = atts
		}
		return api.SendMessage(ctx, req)
	}
	timeout := m.opts.LoadTimeout
	if len(imgs) > 0 {
		timeout = max(timeout, imageSendTimeout)
	}
	return m.doWithin(timeout, send,
		func(m *Model, v any, err error) tea.Cmd {
			c.sending = false
			var ue uploadErr
			if errors.As(err, &ue) {
				c.errLine = "Not sent: " + ue.Error() + ". enter retries."
				return nil
			}
			if err != nil {
				if contracts.IsCode(err, contracts.ErrOutcomeIndeterminate) || retryable(err) {
					c.uncertain = true
					c.errLine = "Outcome unknown (" + errText(err) + "). The hub may have stored it. enter retries with the SAME operation id, which the hub deduplicates — it cannot create a second message."
				} else {
					c.errLine = "Not sent: " + errText(err)
				}
				return nil
			}
			mv := v.(contracts.MessageView)
			delete(m.drafts(), c.sid)
			delete(m.draftImages, c.sid)
			m.rememberMessage(body)
			if m.overlay == c {
				m.overlay = nil
			}
			m.curMsgSess = c.sid
			l := m.listFor("messages:" + c.sid)
			l.sel = mv.Envelope.MessageID
			key := "messages:" + c.sid
			if st := m.loads[key]; st != nil {
				st.stale = true
			}
			return tea.Batch(m.push(viewMessages), m.setToast("sent to "+trunc(c.title, 40)+" · "+string(mv.Status.Storage)+"; delivery facts below update live", toneMuted))
		})
}

func deliverySummary(mv contracts.MessageView) string {
	s := mv.Status
	return fmt.Sprintf("message %s: storage %s · runtime %s · ack %s", shortID(mv.Envelope.MessageID), s.Storage, s.Runtime, s.Ack)
}

// ---- message history view ----

func (m *Model) loadMessages(force bool) tea.Cmd {
	sid := m.curMsgSess
	if sid == "" {
		return nil
	}
	api := m.api
	fn := func(ctx context.Context) (any, error) {
		p, err := api.ListMessages(ctx, contracts.ListMessagesRequest{Box: "all",
			Address: &contracts.Address{Kind: contracts.AddrSession, ID: sid}, PageRequest: contracts.PageRequest{Limit: 100}})
		return p.Items, err
	}
	if force {
		return m.reload("messages:"+sid, fn)
	}
	return m.ensure("messages:"+sid, 0, fn)
}

func (m *Model) sessionMessages() []contracts.MessageView {
	ms, _ := get[[]contracts.MessageView](m, "messages:"+m.curMsgSess)
	out := append([]contracts.MessageView(nil), ms...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Envelope.CreatedAt, out[j].Envelope.CreatedAt
		if !a.Equal(b) {
			return a.After(b)
		}
		return out[i].Envelope.MessageID > out[j].Envelope.MessageID
	})
	return out
}

func (m *Model) selectedMessage() (contracts.MessageView, bool) {
	l := m.listFor("messages:" + m.curMsgSess)
	for _, mv := range m.sessionMessages() {
		if mv.Envelope.MessageID == l.sel {
			return mv, true
		}
	}
	return contracts.MessageView{}, false
}

func (m *Model) messageHints() []hint {
	h := []hint{{"↑↓", "select"}, {"m", "new message"}}
	if mv, ok := m.selectedMessage(); ok {
		switch mv.Status.Runtime {
		case contracts.DeliveryIndeterminate:
			h = append(h, hint{"R", "reconcile"})
		case contracts.DeliveryQueued, contracts.DeliveryNodeReceived:
			h = append(h, hint{"c", "cancel"})
		case contracts.DeliveryRejected, contracts.DeliveryExpired:
			h = append(h, hint{"R", "reissue"})
		}
	}
	return h
}

func (m *Model) viewMessages(w, h int) []string {
	ms := m.sessionMessages()
	l := m.listFor("messages:" + m.curMsgSess)
	ids := make([]string, len(ms))
	byID := map[string]contracts.MessageView{}
	for i, mv := range ms {
		ids[i] = mv.Envelope.MessageID
		byID[ids[i]] = mv
	}
	l.setIDs(ids)
	list := func(w, h int) []string {
		out := []string{m.th.heading.Render("Messages · "+trunc(m.sessionTitle(m.curMsgSess), w-30)) + m.th.dim.Render("  → to it  ← from it")}
		_, st := get[[]contracts.MessageView](m, "messages:"+m.curMsgSess)
		if len(ms) == 0 {
			switch {
			case st == nil || st.inflight:
				out = append(out, m.th.dim.Render("  loading…"))
			case st.err != nil:
				out = append(out, m.th.tone(toneBad, "  ✗ "+errText(st.err)))
			default:
				out = append(out, m.th.dim.Render("  no messages yet — m to send one"))
			}
			return out
		}
		now := m.now()
		out = append(out, m.colHeader("    "+padLeft("AGE", 4)+" "+pad("RUNTIME", 13)+" "+pad("ACK", 12)+" MESSAGE", w))
		start, end := l.window(h - 2)
		for i := start; i < end; i++ {
			mv := byID[ids[i]]
			sel := i == l.idx
			dir := "→"
			if mv.Envelope.Sender.Kind == contracts.PrincipalSession && mv.Envelope.Sender.ID == m.curMsgSess {
				dir = "←"
			}
			cur := m.cursor(sel)
			rt := strings.ReplaceAll(string(mv.Status.Runtime), "_", " ")
			if mv.Status.Runtime == contracts.DeliveryNotApplicable {
				rt = "— (to you)" // owner-inbox messages have no runtime delivery
			}
			row := cur + dir + " " + padLeft(age(now, mv.Envelope.CreatedAt), 4) + " " +
				m.rowTone(sel, deliveryTone(mv.Status.Runtime), pad(rt, 13)) + " " + pad(string(mv.Status.Ack), 12) + " " + firstLine(mv.Envelope.Body.Text)
			out = append(out, m.rowSel(sel, trunc(row, w), w))
		}
		return out
	}
	detail := func(w, h int) []string {
		mv, ok := byID[l.sel]
		if !ok {
			return []string{m.th.dim.Render("No message selected")}
		}
		return m.messageDetail(mv, w)
	}
	return m.listDetail(w, h, list, detail)
}

func (m *Model) messageDetail(mv contracts.MessageView, w int) []string {
	e, s := mv.Envelope, mv.Status
	now := m.now()
	lw := 12
	out := []string{m.th.title.Render(shortID(e.MessageID)) + "  " + m.th.dim.Render(string(e.Mode))}
	out = append(out, m.kvl("From", trunc(m.whoFull(e.Sender), w-lw), lw))
	to := string(e.Recipient.Kind) + ":" + e.Recipient.ID
	switch e.Recipient.Kind {
	case contracts.AddrSession:
		to = m.sessionTitle(e.Recipient.ID) + " (" + e.Recipient.ID + ")"
	case contracts.AddrOwner:
		to = "you (owner inbox)"
	}
	out = append(out, m.kvl("To", trunc(to, w-lw), lw))
	if e.ThreadID != "" {
		out = append(out, m.kvl("Thread", e.ThreadID, lw))
	}
	if e.InReplyTo != "" {
		out = append(out, m.kvl("In reply to", e.InReplyTo, lw))
	}
	if e.Reissues != "" {
		out = append(out, m.kvl("Reissues", e.Reissues, lw))
	}
	for _, a := range e.Attachments {
		out = append(out, m.kvl("Attached", trunc(attachmentText(a), w-lw), lw))
	}
	out = append(out, m.kvl("Created", ago(now, e.CreatedAt)+" (client time)", lw), "")
	// The words first, then how far they got.
	out = append(out, wrap(e.Body.Text, w)...)
	out = append(out, "", m.sectionTitle("Delivery facts", w))
	if d := m.deliverySummary(mv); d.Summary != "" {
		out = append(out, m.kvl("Status", trunc(d.Summary, w-lw+20), lw))
	}
	stored := string(s.Storage)
	if s.StoredAt != nil {
		stored += " " + ago(now, *s.StoredAt)
	}
	out = append(out, m.kvl("Stored", stored, lw))
	rt := m.th.tone(deliveryTone(s.Runtime), strings.ReplaceAll(string(s.Runtime), "_", " "))
	if s.RuntimeReason != "" {
		rt += " — " + s.RuntimeReason
	}
	out = append(out, m.kvl("Runtime", trunc(rt, w-lw+20), lw))
	if s.BoundRunID != "" {
		out = append(out, m.kvl("Bound run", s.BoundRunID, lw))
	}
	if s.ConsumedAt != nil {
		out = append(out, m.kvl("Consumed", ago(now, *s.ConsumedAt), lw))
	}
	ack := string(s.Ack)
	if s.AckRef != "" {
		ack += " (" + s.AckRef + ")"
	}
	out = append(out, m.kvl("Ack", ack, lw))
	outcome := "absent (delivery is not completion)"
	if s.OutcomeRef != nil {
		outcome = string(s.OutcomeRef.Kind) + " " + s.OutcomeRef.ID
	}
	out = append(out, m.kvl("Outcome", trunc(outcome, w-lw), lw))
	if s.Runtime == contracts.DeliveryIndeterminate {
		out = append(out, "", m.th.tone(toneWarn, "⁇ The runtime may have accepted this message."))
		out = append(out, wrap("Pyre will not retry it automatically. Press R to record evidence or to reissue with an explicit duplicate-risk acknowledgement.", w)...)
	}
	if len(s.Receipts) > 0 {
		out = append(out, "", m.sectionTitle("Receipts", w))
		for _, r := range s.Receipts {
			line := fmt.Sprintf("%s %s · %s · run %s", ago(now, r.ObservedAt), r.State, r.Evidence, shortID(r.RunID))
			if r.Detail != "" {
				line += " — " + humanTimes(r.Detail, now)
			}
			out = append(out, trunc(line, w))
		}
	}
	return out
}

func (m *Model) keyMessages(k tea.KeyPressMsg) tea.Cmd {
	l := m.listFor("messages:" + m.curMsgSess)
	if navKey(l, k.String(), m.halfPage()) {
		return nil
	}
	switch k.String() {
	case "esc":
		return m.goBack()
	case "r":
		return m.loadMessages(true)
	case "m":
		if s, ok := m.session(m.curMsgSess); ok {
			return m.openCompose(s)
		}
	case "c":
		mv, ok := m.selectedMessage()
		if !ok {
			return nil
		}
		if mv.Status.Runtime != contracts.DeliveryQueued && mv.Status.Runtime != contracts.DeliveryNodeReceived {
			return m.setToast("only messages not yet attempted can be cancelled", toneWarn)
		}
		id, api := mv.Envelope.MessageID, m.api
		req := contracts.CancelMessageRequest{Mutation: contracts.Mutation{OperationID: m.opID("cancel:" + id)}, MessageID: id}
		return m.do(func(ctx context.Context) (any, error) { return api.CancelMessage(ctx, req) }, m.afterMessageAction("cancel"))
	case "R":
		mv, ok := m.selectedMessage()
		if !ok {
			return nil
		}
		switch mv.Status.Runtime {
		case contracts.DeliveryIndeterminate, contracts.DeliveryRejected, contracts.DeliveryExpired:
			m.overlay = &reconcileOverlay{mv: mv}
		default:
			return m.setToast("reconcile applies to indeterminate, rejected or expired deliveries", toneMuted)
		}
	}
	return nil
}

func (m *Model) afterMessageAction(verb string) func(m *Model, v any, err error) tea.Cmd {
	return func(m *Model, v any, err error) tea.Cmd {
		if err != nil {
			return m.errToast(verb+" failed", err)
		}
		mv := v.(contracts.MessageView)
		if verb == "reissue" {
			m.listFor("messages:" + m.curMsgSess).sel = mv.Envelope.MessageID
		}
		return tea.Batch(m.setToast(verb+": "+deliverySummary(mv), toneMuted), m.loadMessages(true))
	}
}

// ---- reconcile overlay ----

type reconcileOverlay struct {
	mv         contracts.MessageView
	armReissue bool
}

func (r *reconcileOverlay) hints() []hint {
	if r.mv.Status.Runtime == contracts.DeliveryIndeterminate {
		return []hint{{"a", "evidence: accepted"}, {"x", "evidence: not received"}, {"d", "reissue (duplicate risk)"}, {"esc", "cancel"}}
	}
	return []hint{{"d", "reissue"}, {"esc", "cancel"}}
}

func (r *reconcileOverlay) update(m *Model, k tea.KeyPressMsg) tea.Cmd {
	id, api := r.mv.Envelope.MessageID, m.api
	indet := r.mv.Status.Runtime == contracts.DeliveryIndeterminate
	resolve := func(res contracts.RuntimeDeliveryState, note string) tea.Cmd {
		m.overlay = nil
		req := contracts.ResolveDeliveryRequest{Mutation: contracts.Mutation{OperationID: m.opID("resolve:" + id + ":" + string(res))},
			MessageID: id, Resolution: res, Note: note}
		return m.do(func(ctx context.Context) (any, error) { return api.ResolveDelivery(ctx, req) }, m.afterMessageAction("resolve"))
	}
	switch k.String() {
	case "esc", "q", "n":
		m.overlay = nil
		return nil
	case "a":
		if indet {
			return resolve(contracts.DeliveryAccepted, "owner confirmed runtime received the message (pyre tui)")
		}
	case "x":
		if indet {
			return resolve(contracts.DeliveryRejected, "owner confirmed the runtime did not receive the message (pyre tui)")
		}
	case "d":
		if !r.armReissue {
			r.armReissue = true
			return nil
		}
	case "y", "Y":
		if r.armReissue {
			m.overlay = nil
			req := contracts.ReissueMessageRequest{Mutation: contracts.Mutation{OperationID: m.opID("reissue:" + id)}, MessageID: id, AcceptDuplicateRisk: true}
			return m.do(func(ctx context.Context) (any, error) { return api.ReissueMessage(ctx, req) }, m.afterMessageAction("reissue"))
		}
	}
	return nil
}

func (r *reconcileOverlay) view(m *Model, w, h int) []string {
	bw := 78
	inner := min(bw, w-2) - 4
	s := r.mv.Status
	var lines []string
	add := func(t string) { lines = append(lines, wrap(t, inner)...) }
	if s.Runtime == contracts.DeliveryIndeterminate {
		lines = append(lines, m.th.tone(toneWarn, "⁇ Delivery outcome is indeterminate"))
		add("Message " + r.mv.Envelope.MessageID + " may have been accepted by run " + s.BoundRunID + ". Pyre never retries this automatically.")
		if len(s.Receipts) > 0 {
			lr := s.Receipts[len(s.Receipts)-1]
			add("Latest evidence: " + string(lr.State) + " via " + string(lr.Evidence) + " " + lr.Detail)
		} else {
			add("No runtime evidence has been recorded.")
		}
		lines = append(lines, "")
		lines = append(lines, "  "+m.th.key.Render("a")+"  record evidence: the runtime DID receive it")
		lines = append(lines, "  "+m.th.key.Render("x")+"  record evidence: the runtime did NOT receive it")
	} else {
		add("Delivery " + string(s.Runtime) + ": " + s.RuntimeReason)
		lines = append(lines, "")
	}
	lines = append(lines, "  "+m.th.key.Render("d")+"  reissue as a NEW message referencing the original")
	if r.armReissue {
		lines = append(lines, "")
		if s.Runtime == contracts.DeliveryIndeterminate {
			lines = append(lines, m.th.tone(toneBad, "DUPLICATE RISK"))
			add("If the original was received, the agent may perform this work twice. Your acceptance of this risk is recorded.")
		}
		lines = append(lines, m.th.key.Render("y")+" reissue now   "+m.th.key.Render("esc")+" cancel")
	}
	return m.box("Reconcile delivery", lines, bw, w, h)
}

// ---- generic picker overlay ----

type pickItem struct {
	id, label, detail, reason string
	group                     string // optional section header
	run                       func(m *Model) tea.Cmd
}

type pickerOverlay struct {
	title  string
	items  []pickItem
	l      listState
	top    int
	query  string
	filter bool
	onPick func(m *Model, it pickItem) tea.Cmd
}

// visible filters items by the query, ranking label prefix matches first,
// then label matches, then matches only in the detail text.
func (p *pickerOverlay) visible() []pickItem {
	q := strings.ToLower(strings.TrimSpace(p.query))
	type scored struct {
		it    pickItem
		score int
	}
	var ss []scored
	for _, it := range p.items {
		if !matchQuery(q, it.label, it.detail) {
			continue
		}
		label := strings.ToLower(it.label)
		if i := strings.Index(label, ": "); i >= 0 {
			label = label[i+2:]
		}
		score := 2
		switch {
		case q == "":
			score = 0
		case strings.HasPrefix(label, q):
			score = 0
		case matchQuery(q, it.label):
			score = 1
		}
		ss = append(ss, scored{it, score})
	}
	sort.SliceStable(ss, func(i, j int) bool { return ss[i].score < ss[j].score })
	out := make([]pickItem, len(ss))
	for i, s := range ss {
		out[i] = s.it
	}
	return out
}

func (p *pickerOverlay) hints() []hint {
	return []hint{{"enter", "choose"}, {"↑↓", "move"}, {"esc", "cancel"}}
}

func (p *pickerOverlay) sync() []pickItem {
	vis := p.visible()
	ids := make([]string, len(vis))
	for i, it := range vis {
		ids[i] = it.id
	}
	p.l.setIDs(ids)
	return vis
}

func (p *pickerOverlay) update(m *Model, k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	vis := p.sync()
	switch key {
	case "esc":
		m.overlay = nil
		return nil
	case "up", "ctrl+p":
		p.l.move(-1)
		return nil
	case "down", "ctrl+n":
		p.l.move(1)
		return nil
	case "enter":
		for _, it := range vis {
			if it.id == p.l.sel {
				if it.reason != "" {
					return m.setToast(it.label+": "+it.reason, toneWarn)
				}
				m.overlay = nil
				if it.run != nil {
					return it.run(m)
				}
				if p.onPick != nil {
					return p.onPick(m, it)
				}
			}
		}
		return nil
	}
	if p.filter {
		switch {
		case key == "backspace":
			if r := []rune(p.query); len(r) > 0 {
				p.query = string(r[:len(r)-1])
			}
		case k.Text != "" && k.Mod&^tea.ModShift == 0:
			p.query += k.Text
		default:
			return nil
		}
		p.l.sel = ""
		p.l.idx = 0
		p.sync()
		return nil
	}
	navKey(&p.l, key, m.halfPage())
	return nil
}

// ---- associate standalone session with a project ----

func (m *Model) openAssociate(s contracts.SessionView) tea.Cmd {
	ps := m.projects()
	if len(ps) == 0 {
		return m.setToast("standalone session — no projects exist yet", toneMuted)
	}
	sid, api := s.Session.SessionID, m.api
	var items []pickItem
	for _, p := range ps {
		if p.Archived {
			continue
		}
		pid := p.ProjectID
		items = append(items, pickItem{id: pid, label: p.Name, detail: plural(len(p.Repos), "repo", "repos")})
	}
	m.overlay = &pickerOverlay{title: "Associate “" + s.Session.Title + "” with a project", items: items, filter: true,
		onPick: func(m *Model, it pickItem) tea.Cmd {
			pid := it.id
			req := contracts.AssociateRequest{Mutation: contracts.Mutation{OperationID: m.opID("associate:" + sid + ":" + pid)}, SessionID: sid, ProjectID: &pid}
			return m.do(func(ctx context.Context) (any, error) { return api.AssociateSession(ctx, req) },
				func(m *Model, v any, err error) tea.Cmd {
					if err != nil {
						return m.errToast("associate failed", err)
					}
					return tea.Batch(m.setToast("associated with "+it.label, toneGood), m.reload("sessions", nil))
				})
		}}
	return nil
}

// attachmentText names a message's reference the way `pyre msg show` and the
// Android message card do, so the three surfaces can be matched.
func attachmentText(a contracts.Attachment) string {
	switch {
	case a.ArtifactVersionID != "":
		return "artifact version " + a.ArtifactVersionID
	case a.DocumentRevisionID != "":
		return "document revision " + a.DocumentRevisionID
	case a.NoteID != "":
		return "note " + a.NoteID
	}
	return "empty attachment"
}
