package mock

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/notifications"
)

var _ c.AsksAPI = (*Backend)(nil)

// Asks (W-asks) on the mock: the contract semantics in memory. One open
// ask per (scope, topic): scope is the ask's project, else its asking
// session. Deadlines are checked lazily on every asks call against the
// backend clock, so a FakeClock.Advance past a deadline applies the
// default on the next read.

// askDefaultPrefs are the quiet hours used when the owner set none.
var askDefaultPrefs = c.NotificationPrefs{QuietHoursStart: "22:00", QuietHoursEnd: "08:00", TZ: "America/Los_Angeles"}

func (b *Backend) askMap() map[string]*c.Ask {
	if b.asks == nil {
		b.asks = map[string]*c.Ask{}
	}
	return b.asks
}

// normalizeTopic is the lowercase slug of a topic key.
func normalizeTopic(s string) string {
	var sb strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '/':
			sb.WriteRune(r)
			dash = false
		case !dash && sb.Len() > 0:
			sb.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(sb.String(), "-")
}

func askScope(projectID *string, sessionID string) string {
	if projectID != nil && *projectID != "" {
		return "prj:" + *projectID
	}
	return "ses:" + sessionID
}

// titleWords are the significant words of a title (duplicate detection).
func titleWords(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if len(w) >= 3 {
			out[w] = true
		}
	}
	return out
}

// similarTitles: Jaccard overlap of significant words of at least 0.6.
func similarTitles(a, b string) bool {
	wa, wb := titleWords(a), titleWords(b)
	if len(wa) == 0 || len(wb) == 0 {
		return false
	}
	inter := 0
	for w := range wa {
		if wb[w] {
			inter++
		}
	}
	union := len(wa) + len(wb) - inter
	return float64(inter)/float64(union) >= 0.6
}

func validateAsk(req c.CreateAskRequest) error {
	switch {
	case normalizeTopic(req.TopicKey) == "":
		return c.Errorf(c.ErrInvalid, "topic_key is required")
	case len(req.TopicKey) > c.AskMaxTopicKey:
		return c.Errorf(c.ErrInvalid, "topic_key exceeds %d characters", c.AskMaxTopicKey)
	case strings.TrimSpace(req.Title) == "":
		return c.Errorf(c.ErrInvalid, "title is required")
	case len(req.Title) > c.AskMaxTitle:
		return c.Errorf(c.ErrInvalid, "title exceeds %d characters", c.AskMaxTitle)
	case strings.TrimSpace(req.Context) == "":
		return c.Errorf(c.ErrInvalid, "context is required: say why, what you know and what happens next")
	case len(req.Context) > c.AskMaxContext:
		return c.Errorf(c.ErrInvalid, "context exceeds %d characters", c.AskMaxContext)
	case len(req.Options) < c.AskMinOptions || len(req.Options) > c.AskMaxOptions:
		return c.Errorf(c.ErrInvalid, "an ask has %d to %d options", c.AskMinOptions, c.AskMaxOptions)
	case req.DeadlineSeconds < 0:
		return c.Errorf(c.ErrInvalid, "deadline_seconds must not be negative")
	}
	seen := map[string]bool{}
	for _, o := range req.Options {
		if o.ID == "" || strings.TrimSpace(o.Label) == "" {
			return c.Errorf(c.ErrInvalid, "every option needs an id and a label")
		}
		if seen[o.ID] {
			return c.Errorf(c.ErrInvalid, "duplicate option id %q", o.ID)
		}
		seen[o.ID] = true
		if len(o.Label) > c.AskMaxOptionText || len(o.Detail) > c.AskMaxOptionText {
			return c.Errorf(c.ErrInvalid, "option text exceeds %d characters", c.AskMaxOptionText)
		}
	}
	if req.DefaultOption != "" && !seen[req.DefaultOption] {
		return c.Errorf(c.ErrInvalid, "default_option %q is not one of the options", req.DefaultOption)
	}
	return nil
}

// askDeadline applies the deadline rules: the request's seconds or the
// default hour; a deadline inside quiet hours moves to their end
// ("morning") unless the ask is urgent.
func (b *Backend) askDeadline(now time.Time, req c.CreateAskRequest) (time.Time, string) {
	d, rule := c.AskDefaultDeadline, "1h"
	if req.DeadlineSeconds > 0 {
		d, rule = time.Duration(req.DeadlineSeconds)*time.Second, "custom"
	}
	at := now.Add(d)
	if req.Urgent {
		return at, "urgent"
	}
	prefs := b.prefs
	if prefs.QuietHoursStart == "" {
		prefs.QuietHoursStart, prefs.QuietHoursEnd = askDefaultPrefs.QuietHoursStart, askDefaultPrefs.QuietHoursEnd
		if prefs.TZ == "" {
			prefs.TZ = askDefaultPrefs.TZ
		}
	}
	if !notifications.InQuietHours(prefs, at) {
		return at, rule
	}
	loc := time.Local
	if l, err := time.LoadLocation(prefs.TZ); err == nil {
		loc = l
	}
	end, err := time.Parse("15:04", prefs.QuietHoursEnd)
	if err != nil {
		return at, rule
	}
	lt := at.In(loc)
	morning := time.Date(lt.Year(), lt.Month(), lt.Day(), end.Hour(), end.Minute(), 0, 0, loc)
	if !morning.After(lt) {
		morning = morning.AddDate(0, 0, 1)
	}
	return morning.UTC(), "morning"
}

// askMessageText is the body of the owner message carrying an ask.
func askMessageText(a *c.Ask) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Ask: %s\n\n%s\n", a.Title, a.Context)
	for i, o := range a.Options {
		def := ""
		if o.ID == a.DefaultOption {
			def = " (default)"
		}
		fmt.Fprintf(&sb, "\n%d. %s%s", i+1, o.Label, def)
		if o.Detail != "" {
			sb.WriteString(" — " + o.Detail)
		}
	}
	return sb.String()
}

func (b *Backend) CreateAsk(ctx context.Context, req c.CreateAskRequest) (c.CreateAskResponse, error) {
	return mutate(b, ctx, c.OpAsksCreate, req.Mutation, req, func(p c.Principal) (c.CreateAskResponse, error) {
		b.expireAsksLocked()
		if err := validateAsk(req); err != nil {
			return c.CreateAskResponse{}, err
		}
		var sess *c.Session
		switch p.Kind {
		case c.PrincipalSession:
			s, ok := b.sessions[p.ID]
			if !ok {
				return c.CreateAskResponse{}, notFound(c.KindSession, p.ID)
			}
			sess = s
		case c.PrincipalOwner, c.PrincipalSystem:
		default:
			return c.CreateAskResponse{}, c.Errorf(c.ErrForbidden, "%s principals may not ask the owner", p.Kind)
		}
		now := b.now()
		if sess != nil && sess.Parent() != "" {
			// Only top-level sessions reach the owner: a child's ask goes
			// to its parent as an ordinary message.
			mv, err := b.sendLocked(p, c.SendMessageRequest{
				Mutation:  c.Mutation{OperationID: req.OperationID + ":redirect"},
				Recipient: c.Address{Kind: c.AddrSession, ID: sess.Parent()}, Mode: c.ModeQueue,
				Body: c.MessageBody{Text: askMessageText(&c.Ask{Title: req.Title, Context: req.Context, Options: req.Options, DefaultOption: req.DefaultOption})},
			}, "")
			if err != nil {
				return c.CreateAskResponse{}, err
			}
			return c.CreateAskResponse{Status: c.AskRedirected, MessageID: mv.Envelope.MessageID,
				Detail: "child sessions ask their parent; forwarded to " + sess.Parent()}, nil
		}
		sid := ""
		projectID := cloneStrPtr(req.ProjectID)
		if sess != nil {
			sid = sess.SessionID
			if projectID == nil {
				projectID = cloneStrPtr(sess.ProjectID)
			}
		}
		if projectID != nil {
			if _, ok := b.projects[*projectID]; !ok {
				return c.CreateAskResponse{}, notFound(c.KindProject, *projectID)
			}
		}
		topic := normalizeTopic(req.TopicKey)
		scope := askScope(projectID, sid)
		var open, decided *c.Ask
		var similar []c.Ask
		for _, id := range sortedKeys(b.askMap()) {
			a := b.asks[id]
			if askScope(a.ProjectID, a.SessionID) != scope {
				continue
			}
			switch {
			case a.TopicKey == topic && a.State == c.AskOpen:
				open = a
			case a.TopicKey == topic && (decided == nil || a.UpdatedAt.After(decided.UpdatedAt)):
				decided = a
			case a.TopicKey != topic && a.State == c.AskOpen && similarTitles(a.Title, req.Title):
				similar = append(similar, *a)
			}
		}
		if open != nil {
			// The same topic is open: one inbox item; the repeat is an
			// update on it, with no new notification.
			text := strings.TrimSpace(req.Context)
			if req.Title != open.Title {
				text = req.Title + ": " + text
			}
			open.Updates = append(open.Updates, c.AskUpdate{Seq: len(open.Updates) + 1, SessionID: sid, Text: truncate(text, c.AskMaxContext), At: now})
			open.Count++
			open.UpdatedAt = now
			b.withdrawAttentionLocked(waitingGroup(sid), c.AttnWithdrawn) // the session filed its question
			b.emit(c.EvAskChanged, ref(c.KindAsk, open.AskID), open.SessionID, open.ProjectID, open)
			return c.CreateAskResponse{Status: c.AskMerged, Ask: clonePtr(open), Detail: "merged into the open ask on this topic"}, nil
		}
		if decided != nil && !req.Reopen {
			return c.CreateAskResponse{Status: c.AskAlreadyDecided, Ask: clonePtr(decided),
				Detail: "this topic was already decided; resend with reopen to ask again"}, nil
		}
		if err := askQuality(req.Context, req.Options); err != nil { // webinbox.go
			return c.CreateAskResponse{}, err
		}
		if len(similar) > 0 && !req.ConfirmNew {
			return c.CreateAskResponse{Status: c.AskPossibleDuplicate, Candidates: similar,
				Detail: "a similar ask is open under another topic; use its topic or resend with confirm_new"}, nil
		}
		deadline, rule := b.askDeadline(now, req)
		a := &c.Ask{
			AskID: c.NewID(c.PrefixAsk), TopicKey: topic, ProjectID: projectID, SessionID: sid,
			Title: strings.TrimSpace(req.Title), Context: strings.TrimSpace(req.Context),
			Options: append([]c.AskOption(nil), req.Options...), DefaultOption: req.DefaultOption, Urgent: req.Urgent,
			DeadlineAt: &deadline, DeadlineRule: rule, State: c.AskOpen, Count: 1, CreatedAt: now, UpdatedAt: now,
		}
		if sess != nil {
			mv, err := b.sendLocked(p, c.SendMessageRequest{
				Mutation:  c.Mutation{OperationID: req.OperationID + ":ask"},
				Recipient: c.Address{Kind: c.AddrOwner, ID: "owner"}, Mode: c.ModeInfo, ProjectID: projectID,
				Body: c.MessageBody{Text: askMessageText(a)},
			}, "")
			if err != nil {
				return c.CreateAskResponse{}, err
			}
			a.MessageID, a.ThreadID = mv.Envelope.MessageID, mv.Envelope.ThreadID
			// The ask is its own inbox item; the carrying message is not a
			// second one.
			b.withdrawAttentionLocked("message:"+mv.Envelope.MessageID, c.AttnWithdrawn)
		}
		sev := "normal"
		if a.Urgent {
			sev = "urgent"
		}
		b.withdrawAttentionLocked(waitingGroup(sid), c.AttnWithdrawn) // the session filed its question
		att := b.raiseAttentionLocked(c.AttnAsk, sev, "ask:"+a.AskID, ref(c.KindAsk, a.AskID), sid, projectID, a.Title, truncate(firstLineOf(a.Context), 120))
		a.AttentionID = att.AttentionID
		b.askMap()[a.AskID] = a
		b.emit(c.EvAskChanged, ref(c.KindAsk, a.AskID), sid, projectID, a)
		return c.CreateAskResponse{Status: c.AskCreated, Ask: clonePtr(a)}, nil
	})
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func clonePtr(a *c.Ask) *c.Ask {
	cp := clone(*a)
	return &cp
}

func (b *Backend) ListAsks(ctx context.Context, req c.ListAsksRequest) (c.Page[c.Ask], error) {
	return query(b, ctx, func(p c.Principal) (c.Page[c.Ask], error) {
		b.expireAsksLocked()
		topic := normalizeTopic(req.TopicKey)
		var out []c.Ask
		for _, a := range b.askMap() {
			if len(req.States) > 0 && !hasAskState(req.States, a.State) {
				continue
			}
			if req.ProjectID != nil && !eqStrPtr(a.ProjectID, req.ProjectID) {
				continue
			}
			if req.SessionID != "" && a.SessionID != req.SessionID {
				continue
			}
			if topic != "" && a.TopicKey != topic {
				continue
			}
			if req.Since != nil && a.State.Decided() && !a.UpdatedAt.After(*req.Since) {
				continue
			}
			if req.Query != "" && !askMatches(a, req.Query) {
				continue
			}
			out = append(out, *a)
		}
		// Open asks first, soonest deadline first; then decided ones, most
		// recently decided first.
		sort.Slice(out, func(i, j int) bool {
			a, b := out[i], out[j]
			if (a.State == c.AskOpen) != (b.State == c.AskOpen) {
				return a.State == c.AskOpen
			}
			if a.State == c.AskOpen && a.DeadlineAt != nil && b.DeadlineAt != nil && !a.DeadlineAt.Equal(*b.DeadlineAt) {
				return a.DeadlineAt.Before(*b.DeadlineAt)
			}
			if !a.UpdatedAt.Equal(b.UpdatedAt) {
				return a.UpdatedAt.After(b.UpdatedAt)
			}
			return a.AskID < b.AskID
		})
		return paginate(out, req.PageRequest, b.now())
	})
}

func hasAskState(xs []c.AskState, s c.AskState) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func askMatches(a *c.Ask, q string) bool {
	hay := []string{a.Title, a.Context, a.TopicKey}
	for _, o := range a.Options {
		hay = append(hay, o.Label, o.Detail)
	}
	for _, u := range a.Updates {
		hay = append(hay, u.Text)
	}
	text := strings.ToLower(strings.Join(hay, "\x00"))
	for _, term := range strings.Fields(strings.ToLower(q)) {
		if !strings.Contains(text, term) {
			return false
		}
	}
	return true
}

func (b *Backend) GetAsk(ctx context.Context, req c.GetRequest) (c.Ask, error) {
	return query(b, ctx, func(p c.Principal) (c.Ask, error) {
		b.expireAsksLocked()
		a, ok := b.askMap()[req.ID]
		if !ok {
			return c.Ask{}, notFound(c.KindAsk, req.ID)
		}
		return *a, nil
	})
}

func (b *Backend) AnswerAsks(ctx context.Context, req c.AnswerAsksRequest) (c.AnswerAsksResponse, error) {
	return mutate(b, ctx, c.OpAsksAnswer, req.Mutation, req, func(p c.Principal) (c.AnswerAsksResponse, error) {
		if err := requireOwner(p); err != nil {
			return c.AnswerAsksResponse{}, err
		}
		if len(req.Answers) == 0 {
			return c.AnswerAsksResponse{}, c.Errorf(c.ErrInvalid, "answers is empty")
		}
		b.expireAsksLocked()
		return b.answerAsksLocked(p, req.OperationID, req.Answers), nil
	})
}

// answerAsksLocked answers each item (AnswerAsks and the held-answer
// flush). Caller holds b.mu.
func (b *Backend) answerAsksLocked(p c.Principal, opID string, items []c.AnswerAskItem) c.AnswerAsksResponse {
	now := b.now()
	out := c.AnswerAsksResponse{Results: []c.AnswerAskResult{}}
	var decided []*c.Ask
	for _, it := range items {
		res := c.AnswerAskResult{AskID: it.AskID}
		a, ok := b.askMap()[it.AskID]
		switch {
		case !ok:
			res.Error = notFound(c.KindAsk, it.AskID)
		case a.State != c.AskOpen:
			res.Error = c.Errorf(c.ErrConflict, "ask is already %s", a.State).WithRef(c.KindAsk, a.AskID)
			res.Ask = clonePtr(a)
		case it.OptionID == "" && strings.TrimSpace(it.Text) == "":
			res.Error = c.Errorf(c.ErrInvalid, "an answer needs an option or text")
		default:
			if _, ok := a.Option(it.OptionID); it.OptionID != "" && !ok {
				res.Error = c.Errorf(c.ErrInvalid, "option %q is not one of the ask's options", it.OptionID)
				break
			}
			a.State = c.AskAnswered
			a.Answer = &c.AskAnswer{OptionID: it.OptionID, Text: strings.TrimSpace(it.Text), By: p, At: now}
			a.UpdatedAt = now
			b.withdrawAttentionLocked("ask:"+a.AskID, c.AttnHandled)
			decided = append(decided, a)
			res.Ask = a // cloned below, after the decision message
		}
		out.Results = append(out.Results, res)
	}
	b.tellAskersLocked(p, opID, decided)
	for i := range out.Results {
		if out.Results[i].Ask != nil {
			out.Results[i].Ask = clonePtr(out.Results[i].Ask)
		}
	}
	return out
}

// tellAskersLocked sends each asking session one combined message with
// its decisions, replying to its first ask's message (so wait_reply
// works), and emits ask.changed for each ask.
func (b *Backend) tellAskersLocked(p c.Principal, opID string, decided []*c.Ask) {
	bySession := map[string][]*c.Ask{}
	var order []string
	for _, a := range decided {
		if a.SessionID == "" {
			continue
		}
		if _, ok := bySession[a.SessionID]; !ok {
			order = append(order, a.SessionID)
		}
		bySession[a.SessionID] = append(bySession[a.SessionID], a)
	}
	for _, sid := range order {
		if _, ok := b.sessions[sid]; !ok {
			continue
		}
		as := bySession[sid]
		var sb strings.Builder
		sb.WriteString("Decision")
		if len(as) > 1 {
			sb.WriteString("s")
		}
		sb.WriteString(" on your ask")
		if len(as) > 1 {
			sb.WriteString("s")
		}
		sb.WriteString(":")
		for _, a := range as {
			sb.WriteString("\n- " + a.Title + ": " + askOutcome(a))
		}
		reply := ""
		if a := as[0]; a.MessageID != "" {
			if _, ok := b.messages[a.MessageID]; ok {
				reply = a.MessageID
			}
		}
		mv, err := b.sendLocked(p, c.SendMessageRequest{
			Mutation:  c.Mutation{OperationID: opID + ":" + sid},
			Recipient: c.Address{Kind: c.AddrSession, ID: sid}, Mode: c.ModeQueue, InReplyTo: reply,
			Body: c.MessageBody{Text: sb.String()},
		}, "")
		if err == nil {
			for _, a := range as {
				a.DecisionMessageID = mv.Envelope.MessageID
			}
		}
	}
	for _, a := range decided {
		b.dropHeldLocked(a.AskID)
		b.emit(c.EvAskChanged, ref(c.KindAsk, a.AskID), a.SessionID, a.ProjectID, a)
	}
}

// askOutcome is the decision in words.
func askOutcome(a *c.Ask) string {
	if a.Answer == nil {
		return string(a.State)
	}
	s := ""
	if o, ok := a.Option(a.Answer.OptionID); ok {
		s = o.Label
	}
	if a.Answer.Text != "" {
		if s != "" {
			s += " — "
		}
		s += a.Answer.Text
	}
	if a.Answer.ByDefault {
		s += " (default: no answer by the deadline)"
	}
	return s
}

// expireAsksLocked applies the deadline: the default option when there is
// one ("defaulted"), else "expired". Caller holds b.mu.
func (b *Backend) expireAsksLocked() {
	b.sendHeldLocked() // webinbox.go: quiet window passed, or held answers of due asks
	now := b.now()
	var decided []*c.Ask
	for _, id := range sortedKeys(b.askMap()) {
		a := b.asks[id]
		if a.State != c.AskOpen || a.DeadlineAt == nil || now.Before(*a.DeadlineAt) {
			continue
		}
		at := *a.DeadlineAt
		if a.DefaultOption != "" {
			a.State = c.AskDefaulted
			a.Answer = &c.AskAnswer{OptionID: a.DefaultOption, By: c.Principal{Kind: c.PrincipalSystem, ID: "hub"}, ByDefault: true, At: at}
		} else {
			a.State = c.AskExpired
		}
		a.UpdatedAt = now
		b.withdrawAttentionLocked("ask:"+a.AskID, c.AttnHandled)
		decided = append(decided, a)
	}
	if len(decided) > 0 {
		b.tellAskersLocked(c.Principal{Kind: c.PrincipalSystem, ID: "hub"}, "op_ask-deadline-"+decided[0].AskID, decided)
	}
}

func (b *Backend) WithdrawAsk(ctx context.Context, req c.WithdrawAskRequest) (c.Ask, error) {
	return mutate(b, ctx, c.OpAsksWithdraw, req.Mutation, req, func(p c.Principal) (c.Ask, error) {
		b.expireAsksLocked()
		a, ok := b.askMap()[req.AskID]
		if !ok {
			return c.Ask{}, notFound(c.KindAsk, req.AskID)
		}
		switch p.Kind {
		case c.PrincipalOwner, c.PrincipalSystem:
		case c.PrincipalSession:
			if a.SessionID != p.ID {
				return c.Ask{}, c.Errorf(c.ErrForbidden, "only the asking session may withdraw its ask")
			}
		default:
			return c.Ask{}, c.Errorf(c.ErrForbidden, "%s principals may not withdraw asks", p.Kind)
		}
		if a.State != c.AskOpen {
			return c.Ask{}, c.Errorf(c.ErrConflict, "ask is already %s", a.State).WithRef(c.KindAsk, a.AskID)
		}
		if req.SupersededBy != "" {
			if _, ok := b.asks[req.SupersededBy]; !ok {
				return c.Ask{}, notFound(c.KindAsk, req.SupersededBy)
			}
		}
		a.State = c.AskWithdrawn
		if req.SupersededBy != "" {
			a.State, a.SupersededBy = c.AskSuperseded, req.SupersededBy
		}
		if r := strings.TrimSpace(req.Reason); r != "" {
			a.Updates = append(a.Updates, c.AskUpdate{Seq: len(a.Updates) + 1, SessionID: a.SessionID, Text: "Withdrawn: " + truncate(r, 300), At: b.now()})
		}
		a.UpdatedAt = b.now()
		b.withdrawAttentionLocked("ask:"+a.AskID, c.AttnWithdrawn)
		b.dropHeldLocked(a.AskID)
		b.emit(c.EvAskChanged, ref(c.KindAsk, a.AskID), a.SessionID, a.ProjectID, a)
		return *a, nil
	})
}
