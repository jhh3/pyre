package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// Attention items behind session flags (W-attn). There is no Inbox view:
// pending requests are answered from their session (a), the finder still
// finds items by title and opens what they are about, and each session's
// flag (attention_flag.go) is what the lists show.

func severityRank(s string) int {
	switch s {
	case "urgent":
		return 0
	case "normal":
		return 1
	}
	return 2
}

// openItems returns open/read items, most important first. Withdrawn,
// handled and snoozed items are not included. The list filter is ignored.
func (m *Model) openItems() []contracts.AttentionItem {
	var out []contracts.AttentionItem
	for _, a := range m.attention() {
		if a.State != contracts.AttnOpen && a.State != contracts.AttnRead {
			continue
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.State == contracts.AttnOpen) != (b.State == contracts.AttnOpen) {
			return a.State == contracts.AttnOpen
		}
		if severityRank(a.Severity) != severityRank(b.Severity) {
			return severityRank(a.Severity) < severityRank(b.Severity)
		}
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		return a.AttentionID < b.AttentionID
	})
	return out
}

func isAnswerable(a contracts.AttentionItem) bool {
	if a.Object.Kind != "" && a.Object.Kind != contracts.KindApproval {
		// e.g. "answer in its terminal" (expired remote approval): the item
		// points at the session; it is resolved by attaching, not here.
		return false
	}
	return a.Kind == contracts.AttnApproval || a.Kind == contracts.AttnQuestion || a.Object.Kind == contracts.KindApproval
}

// loadApproval fetches the live approval behind an answerable item.
func (m *Model) loadApproval(a contracts.AttentionItem, force bool) tea.Cmd {
	if !isAnswerable(a) {
		return nil
	}
	api := m.api
	key := "approval:" + a.AttentionID
	var fn loaderFn
	if a.Object.Kind == contracts.KindApproval {
		id := a.Object.ID
		fn = func(ctx context.Context) (any, error) { return api.GetApproval(ctx, contracts.GetRequest{ID: id}) }
	} else {
		sid := a.SessionID
		fn = func(ctx context.Context) (any, error) {
			p, err := api.ListApprovals(ctx, contracts.ListApprovalsRequest{SessionID: sid})
			if err != nil {
				return contracts.Approval{}, err
			}
			if len(p.Items) == 0 {
				return contracts.Approval{}, contracts.Errorf(contracts.ErrNotFound, "no pending approval for this session")
			}
			return p.Items[0], nil
		}
	}
	if force {
		return m.reload(key, fn)
	}
	return m.ensure(key, 0, fn)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// answer submits an approval answer by (approval_id, run_id). The same
// answer to the same approval always carries the same operation id, so a
// double press or a retry after a lost response is deduplicated by the hub.
func (m *Model) answer(a contracts.AttentionItem, ap contracts.Approval, optionID, text string) tea.Cmd {
	opKey := "answer:" + ap.ApprovalID + ":" + ap.RunID + ":" + optionID + ":" + text
	busyKey := "answer:" + ap.ApprovalID
	if m.busy[busyKey] {
		return m.setToast("answer already being submitted…", toneMuted)
	}
	m.busy[busyKey] = true
	req := contracts.AnswerApprovalRequest{
		Mutation:   contracts.Mutation{OperationID: m.opID(opKey)},
		ApprovalID: ap.ApprovalID, RunID: ap.RunID, OptionID: optionID, Text: text,
	}
	attID, api := a.AttentionID, m.api
	m.answerResultSet(attID, m.th.dim.Render("submitting answer…"))
	return m.do(func(ctx context.Context) (any, error) { return api.AnswerApproval(ctx, req) },
		func(m *Model, v any, err error) tea.Cmd {
			m.busy[busyKey] = false
			if err != nil {
				switch contracts.CodeOf(err) {
				case contracts.ErrConflict, contracts.ErrExpired:
					m.answerResultSet(attID, m.th.tone(toneWarn, "Not applied: "+errText(err)))
				case contracts.ErrOutcomeIndeterminate, contracts.ErrUnavailable:
					m.answerResultSet(attID, m.th.tone(toneWarn, "Outcome unknown: "+errText(err)+". Pressing the same option again reuses the same operation id (safe)."))
				default:
					m.answerResultSet(attID, m.th.tone(toneBad, "Answer failed: "+errText(err)))
				}
				return tea.Batch(m.reload("approval:"+attID, nil), m.reload("attention", nil))
			}
			got := v.(contracts.Approval)
			msg := "Answer submitted"
			if r := got.Resolution; r != nil {
				switch r.Effect {
				case "superseded":
					msg = "This request was already resolved elsewhere first; your answer was not applied"
				case "effective":
					msg = "Answer took effect (runtime evidence)"
				default:
					msg = "Answer submitted to the runtime (effect: " + orDash(r.Effect) + ")"
				}
			}
			m.answerResultSet(attID, m.th.tone(toneGood, msg))
			if st := m.loads["approval:"+attID]; st != nil {
				st.val, st.have = got, true
			}
			return tea.Batch(m.reload("attention", nil), m.reload("sessions", nil))
		})
}

func (m *Model) answerResultSet(id, s string) {
	if m.answerResult == nil {
		m.answerResult = map[string]string{}
	}
	m.answerResult[id] = s
}

// openAttentionTarget navigates to the object an item is about.
func (m *Model) openAttentionTarget(a contracts.AttentionItem) tea.Cmd {
	switch a.Object.Kind {
	case contracts.KindArtifactVersion, contracts.KindArtifact:
		if a.ProjectID != nil {
			return m.openArtifactByRef(*a.ProjectID, a.Object)
		}
	case contracts.KindDocument, contracts.KindDocumentRevision, contracts.KindProposal:
		if a.ProjectID != nil {
			cmd := m.openProject(*a.ProjectID)
			m.projTab = tabDocs
			return cmd
		}
	case contracts.KindMessage:
		if a.SessionID != "" {
			m.curMsgSess = a.SessionID
			m.listFor("messages:" + a.SessionID).sel = a.Object.ID
			return m.push(viewMessages)
		}
	case contracts.KindNode:
		m.mgmtList.sel = a.Object.ID
		return m.push(viewMachines)
	case contracts.KindAccount:
		m.mgmtList.sel = a.Object.ID
		return m.push(viewAccounts)
	}
	if a.SessionID != "" {
		return m.focusSession(a.SessionID)
	}
	if a.ProjectID != nil {
		return m.openProject(*a.ProjectID)
	}
	return m.setToast("no related view for "+string(a.Object.Kind), toneMuted)
}

// focusSession switches to Sessions with id selected, clearing a filter
// that would hide it.
func (m *Model) focusSession(id string) tea.Cmd {
	m.view, m.back = viewSessions, nil
	if !m.sessList.selectID(id) {
		m.applyFilter("")
		m.sessList.sel = id
		m.syncSessions()
	}
	return m.schedulePreview()
}

// ---- text prompt overlay ----

type textPrompt struct {
	title string
	in    textinput.Model
	done  func(m *Model, text string) tea.Cmd
}

func (m *Model) openTextPrompt(title, placeholder string, done func(m *Model, text string) tea.Cmd) tea.Cmd {
	in := textinput.New()
	in.Placeholder = placeholder
	in.CharLimit = 4000
	in.Prompt = "> "
	tp := &textPrompt{title: title, in: in, done: done}
	m.overlay = tp
	return tp.in.Focus()
}

func (t *textPrompt) hints() []hint { return []hint{{"enter", "submit"}, {"esc", "cancel"}} }

func (t *textPrompt) updateMsg(m *Model, msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	t.in, cmd = t.in.Update(msg)
	return cmd
}

func (t *textPrompt) update(m *Model, k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "esc":
		m.overlay = nil
		return nil
	case "enter":
		v := strings.TrimSpace(t.in.Value())
		if v == "" {
			return nil
		}
		m.overlay = nil
		return t.done(m, v)
	}
	var cmd tea.Cmd
	t.in, cmd = t.in.Update(k)
	return cmd
}

func (t *textPrompt) view(m *Model, w, h int) []string {
	bw := min(76, w-2)
	t.in.SetWidth(bw - 8)
	return m.box(t.title, []string{t.in.View()}, bw, w, h)
}

// attentionDetail describes an item (finder preview). It never fetches a
// message body: reading a message by ID counts as viewing it at the hub.
func (m *Model) attentionDetail(a contracts.AttentionItem, w int) []string {
	now := m.now()
	lw := 10
	tw := min(w, textMax)
	st := attentionStatus(a.Kind)
	meta := m.th.status(st) + m.th.dim.Render(" · "+a.Severity+" · "+string(a.State))
	if a.Count > 1 {
		meta += m.th.dim.Render(fmt.Sprintf(" · %d occurrences", a.Count))
	}
	out := []string{m.th.title.Render(trunc(m.humanIDs(a.Title), w)), meta}
	if a.Summary != "" {
		out = append(out, "")
		out = append(out, wrap(m.humanIDs(a.Summary), tw)...)
	}
	out = append(out, "")
	if a.SessionID != "" {
		out = append(out, m.kvl("Session", trunc(m.sessionTitle(a.SessionID), w-lw), lw))
	}
	if a.ProjectID != nil {
		out = append(out, m.kvl("Project", m.projectName(a.ProjectID), lw))
	}
	upd := ago(now, a.UpdatedAt)
	if !a.CreatedAt.Equal(a.UpdatedAt) {
		upd += m.th.dim.Render(" · first " + ago(now, a.CreatedAt))
	}
	out = append(out, m.kvl("Updated", upd, lw))
	if isAnswerable(a) {
		out = append(out, "", m.sectionTitle("Request", w))
		out = append(out, m.approvalLines(a, tw)...)
	}
	if msg := m.answerResult[a.AttentionID]; msg != "" {
		out = append(out, "")
		out = append(out, wrap(msg, tw)...)
	}
	return out
}

func (m *Model) approvalLines(a contracts.AttentionItem, w int) []string {
	ap, st := get[contracts.Approval](m, "approval:"+a.AttentionID)
	switch {
	case st == nil || (!st.have && st.err == nil):
		return []string{m.th.dim.Render("loading request…")}
	case !st.have:
		return []string{m.th.tone(toneBad, trunc("✗ "+errText(st.err), w))}
	}
	now := m.now()
	var out []string
	out = append(out, m.th.bold.Render(trunc(ap.Title, w)))
	if ap.Body != "" {
		out = append(out, wrap(ap.Body, w)...)
	}
	meta := fmt.Sprintf("%s · requested %s · run %s", ap.Kind, ago(now, ap.CreatedAt), shortID(ap.RunID))
	if ap.ExpiresAt != nil {
		meta += " · expires " + until(now, *ap.ExpiresAt)
	}
	out = append(out, m.th.dim.Render(trunc(meta, w)))
	if ap.State != contracts.ApprovalPending {
		line := "This request is " + string(ap.State) + "; answer controls are withdrawn."
		if r := ap.Resolution; r != nil {
			who := m.whoFull(r.Actor)
			if r.Source == "native" {
				who = "the native terminal"
			}
			line = fmt.Sprintf("Resolved %s by %s (option %q, effect %s). Controls withdrawn.", ago(now, r.ResolvedAt), who, r.OptionID, orDash(r.Effect))
		}
		out = append(out, "")
		for _, l := range wrap(line, w) {
			out = append(out, m.th.dim.Render(l))
		}
		return out
	}
	out = append(out, "")
	for i, o := range ap.Options {
		if i >= 9 {
			break
		}
		lbl := o.Label
		if o.Kind != "" && o.Kind != "choice" && !strings.EqualFold(o.Kind, o.Label) {
			lbl += m.th.dim.Render("  " + o.Kind)
		}
		out = append(out, "  "+m.th.key.Render(itoa(i+1))+"  "+lbl)
	}
	if ap.AllowText {
		out = append(out, "  "+m.th.key.Render("t")+"  answer with text")
	}
	if len(ap.Options) == 0 && !ap.AllowText {
		out = append(out, m.th.dim.Render("No answer options offered; attach to the session to respond."))
	}
	return out
}
