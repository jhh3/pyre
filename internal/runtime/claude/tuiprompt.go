package claude

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// Terminal permission prompts. Most Claude permission dialogs also fire the
// PermissionRequest hook, which the approval broker answers. Some do not:
// the hard-coded safety checks that ask even in bypass mode ("Dangerous rm
// operation on statically-unresolvable target …", Claude Code 2.1.304), and
// prompts raised by in-session subagents ("Bash command · from the
// implementer agent"). Those used to leave the run waiting_approval with no
// approval to answer (iss_…). Pyre now reads any
// permission prompt the screen shows and no open broker request covers as
// an approval of its own, answered by selecting the option in the dialog.

// tuiPrompt is a permission prompt read from the screen: a "Do you want to
// …?" question followed by numbered options, under a rule with its title.
type tuiPrompt struct {
	Title    string // first row of the dialog ("Bash command · from the implementer agent")
	Body     string // rows between the title and the question
	Question string
	Options  []tuiOption
	Selected int // number of the selected option (0 if none is marked)
}

type tuiOption struct {
	N     int
	Label string
}

// key identifies the prompt's content: the same dialog redrawn keeps it.
func (p *tuiPrompt) key() string {
	var b strings.Builder
	b.WriteString(oneLine(p.Title, 1000) + "\x00" + oneLine(p.Body, 4000) + "\x00" + p.Question)
	for _, o := range p.Options {
		fmt.Fprintf(&b, "\x00%d.%s", o.N, o.Label)
	}
	return b.String()
}

var (
	questionRe  = regexp.MustCompile(`^\s*│?\s*(Do you want to .+\?)\s*│?\s*$`)
	tuiOptionRe = regexp.MustCompile(`^\s*│?\s*(` + promptGlyph + `|` + altPromptGlyph + `)?\s*(\d+)\.\s+(.+?)\s*│?\s*$`)
)

// parseTUIPrompt finds the last permission question on the screen with its
// numbered options below it (nil if none).
func parseTUIPrompt(lines []string) *tuiPrompt {
	for q := len(lines) - 1; q >= 0 && q >= len(lines)-dialogMaxRows*2; q-- {
		m := questionRe.FindStringSubmatch(lines[q])
		if m == nil {
			continue
		}
		p := &tuiPrompt{Question: m[1]}
		for _, l := range lines[q+1:] {
			if strings.TrimSpace(l) == "" && len(p.Options) == 0 {
				continue
			}
			om := tuiOptionRe.FindStringSubmatch(l)
			if om == nil {
				break
			}
			n, _ := strconv.Atoi(om[2])
			p.Options = append(p.Options, tuiOption{N: n, Label: om[3]})
			if om[1] != "" {
				p.Selected = n
			}
		}
		if len(p.Options) < 2 {
			return nil
		}
		// Title and body: the rows between the rule above the dialog and the
		// question. Without a rule the rows above are the transcript, so
		// only the question names the prompt.
		for i := q - 1; i >= 0 && i >= q-dialogMaxRows; i-- {
			if !isSeparator(lines[i]) {
				continue
			}
			var body []string
			for _, l := range lines[i+1 : q] {
				t := strings.TrimSpace(strings.Trim(strings.TrimSpace(l), "│"))
				if t == "" {
					continue
				}
				if p.Title == "" {
					p.Title = t
				} else {
					body = append(body, t)
				}
			}
			p.Body = strings.Join(body, "\n")
			break
		}
		return p
	}
	return nil
}

// optionKind classifies a prompt option by its label and gives the
// approval option id Pyre offers for it: the first "Yes" is "allow" (as for
// broker approvals), a "Yes, and don't ask again …" is "allow_always", "No"
// is "deny"; anything else is "opt_<n>".
func optionKind(label string, seenAllow bool) (id, kind string) {
	l := strings.ToLower(label)
	switch {
	case strings.HasPrefix(l, "yes") && (strings.Contains(l, "don't ask") || strings.Contains(l, "don’t ask") || strings.Contains(l, "always") || strings.Contains(l, "allow all")):
		return "allow_always", "allow_always"
	case strings.HasPrefix(l, "yes") && !seenAllow:
		return "allow", "allow"
	case strings.HasPrefix(l, "no"):
		return "deny", "deny"
	}
	return "", "choice"
}

// tuiTrack is one terminal prompt Pyre saw, from first sight until the
// screen stopped showing it.
type tuiTrack struct {
	RequestID string
	Key       string
	Prompt    tuiPrompt
	OptionIDs map[string]int // approval option id -> option number
	Options   []contracts.ApprovalOption
	FirstSeen time.Time
	GoneAt    time.Time
	// Emitted: reported as an approval at least once (only those are
	// reported again when resolved).
	Emitted bool
	// Covered: a broker request is pending alongside it; the broker's
	// approval answers it (once that expires, this one is reported).
	Covered    bool
	AnsweredAt time.Time
	Answer     string // option id Pyre selected
}

func newTUITrack(p *tuiPrompt, now time.Time) *tuiTrack {
	key := p.key()
	sum := sha256.Sum256([]byte(key + "\x00" + strconv.FormatInt(now.UnixNano(), 10)))
	t := &tuiTrack{RequestID: "tui_" + hex.EncodeToString(sum[:10]), Key: key, Prompt: *p, OptionIDs: map[string]int{}, FirstSeen: now}
	seenAllow := false
	for _, o := range p.Options {
		id, kind := optionKind(o.Label, seenAllow)
		if id == "" || t.OptionIDs[id] != 0 {
			id = "opt_" + strconv.Itoa(o.N)
		}
		seenAllow = seenAllow || id == "allow"
		t.OptionIDs[id] = o.N
		t.Options = append(t.Options, contracts.ApprovalOption{ID: id, Label: o.Label, Kind: kind})
	}
	return t
}

// tuiPromptGrace: a prompt must stay on screen this long before it becomes
// an approval, so the broker's PermissionRequest (which races the dialog)
// is folded in first when there is one.
const tuiPromptGrace = permDialogGrace

// observeTUIPrompt updates the terminal prompt tracks from the screen (e.mu
// held). brokerOpen: a broker request is pending (its approval answers the
// dialog through the hook).
func (rs *runState) observeTUIPrompt(si *screenInfo, brokerOpen bool, now time.Time) {
	var p *tuiPrompt
	if si.DialogKind == "permission" {
		p = si.Prompt
	}
	cur := rs.tuiPrompts[rs.tuiOpen]
	if cur != nil && (p == nil || p.key() != cur.Key) {
		cur.GoneAt = now
		rs.tuiOpen, cur = "", nil
	}
	if p == nil {
		return
	}
	if cur == nil {
		cur = newTUITrack(p, now)
		rs.tuiPrompts[cur.RequestID] = cur
		rs.tuiOrder = append(rs.tuiOrder, cur.RequestID)
		rs.tuiOpen = cur.RequestID
		if len(rs.tuiOrder) > 50 {
			delete(rs.tuiPrompts, rs.tuiOrder[0])
			rs.tuiOrder = rs.tuiOrder[1:]
		}
	}
	cur.Prompt.Selected = p.Selected
	cur.Covered = brokerOpen
}

// tuiApprovals reports the terminal prompts as approvals: the one on screen
// is pending, an earlier one Pyre reported is resolved.
func (rs *runState) tuiApprovals(h contracts.RunHandle, now time.Time) []contracts.Approval {
	var out []contracts.Approval
	for _, rid := range rs.tuiOrder {
		t := rs.tuiPrompts[rid]
		if t.Covered {
			continue
		}
		open := t.GoneAt.IsZero()
		if open && !t.Emitted && now.Sub(t.FirstSeen) < tuiPromptGrace {
			continue
		}
		if !open && !t.Emitted {
			continue
		}
		t.Emitted = true
		body := t.Prompt.Body
		if body != "" {
			body += "\n"
		}
		body += t.Prompt.Question
		title := t.Prompt.Title
		if title == "" {
			title = "permission prompt"
		}
		ap := contracts.Approval{
			ApprovalID: approvalID(h.RunID, rid), SessionID: h.SessionID, RunID: h.RunID, RequestID: rid,
			Kind: contracts.ApprovalPermission, Title: "Terminal prompt: " + oneLine(title, 160), Body: redact(body),
			Options: append([]contracts.ApprovalOption(nil), t.Options...), CreatedAt: t.FirstSeen, State: contracts.ApprovalPending,
		}
		if !open {
			res := &contracts.ApprovalResolution{ResolvedAt: t.GoneAt, Source: "native", Reason: "answered_in_terminal"}
			if !t.AnsweredAt.IsZero() {
				res.OptionID, res.Source, res.Effect, res.Reason = t.Answer, "pyre", "effective", ""
			}
			ap.State, ap.Resolution = contracts.ApprovalResolved, res
		}
		out = append(out, ap)
	}
	return out
}

func isTUIRequest(rid string) bool { return strings.HasPrefix(rid, "tui_") }

// answerTUI answers a terminal prompt by selecting the chosen option in the
// dialog (Up/Down, then Enter), after checking the screen still shows the
// same prompt, so the keys never land in a different dialog.
func (a *Adapter) answerTUI(ctx context.Context, h contracts.RunHandle, ap contracts.Approval, optionID string) (contracts.CommandReceipt, error) {
	rid := ap.RequestID
	if ap.ApprovalID != "" && ap.ApprovalID != approvalID(h.RunID, rid) {
		return a.receipt(contracts.OutcomeFailed, false, ""), contracts.Errorf(contracts.ErrConflict, "approval id does not match request %s", rid)
	}
	e := a.entry(h)
	e.deliverMu.Lock()
	defer e.deliverMu.Unlock()
	e.mu.Lock()
	t := e.rs.tuiPrompts[rid]
	var key string
	var target int
	open := t != nil && t.GoneAt.IsZero() && t.AnsweredAt.IsZero()
	if t != nil {
		key, target = t.Key, t.OptionIDs[optionID]
	}
	e.mu.Unlock()
	if t == nil {
		return a.receipt(contracts.OutcomeFailed, false, ""), contracts.Errorf(contracts.ErrNotFound, "no terminal prompt %q for run %s", rid, h.RunID)
	}
	if !open {
		return a.receipt(contracts.OutcomeFailed, false, "prompt already closed"), contracts.Errorf(contracts.ErrConflict, "terminal prompt %s is no longer shown", rid)
	}
	if target == 0 {
		return a.receipt(contracts.OutcomeFailed, false, ""), contracts.Errorf(contracts.ErrInvalid, "option %q is not offered by this prompt", optionID)
	}
	// Move the selection to the target, re-reading the screen each time.
	for attempt := 0; ; attempt++ {
		si, err := a.screen(ctx, h.TmuxTarget)
		if err != nil {
			return a.receipt(contracts.OutcomeFailed, false, err.Error()), nil
		}
		p := si.Prompt
		if si.DialogKind != "permission" || p == nil || p.key() != key {
			return a.receipt(contracts.OutcomeFailed, false, "prompt no longer shown"), contracts.Errorf(contracts.ErrConflict, "terminal prompt %s is no longer shown", rid)
		}
		if p.Selected == target {
			break
		}
		if attempt >= 3 || p.Selected == 0 {
			return a.receipt(contracts.OutcomeFailed, false, "could not select the option"), contracts.Errorf(contracts.ErrConflict, "could not select option %d in the terminal prompt (selected: %d)", target, p.Selected)
		}
		move, n := tmux.KeyDown, target-p.Selected
		if n < 0 {
			move, n = tmux.KeyUp, -n
		}
		keys := make([]tmux.Key, n)
		for i := range keys {
			keys[i] = move
		}
		if err := a.srv.SendKeys(ctx, h.TmuxTarget, keys...); err != nil {
			return a.receipt(contracts.OutcomeFailed, false, err.Error()), nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	if err := a.srv.SendKeys(ctx, h.TmuxTarget, tmux.KeyEnter); err != nil {
		return a.receipt(contracts.OutcomeFailed, false, err.Error()), nil
	}
	e.mu.Lock()
	t.AnsweredAt, t.Answer = a.o.Now(), optionID
	e.mu.Unlock()
	return a.receipt(contracts.OutcomeDelivered, true, "effect=submitted; option selected in the terminal prompt"), nil
}
