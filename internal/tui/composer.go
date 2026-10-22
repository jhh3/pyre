package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// The composer works like Claude Code's prompt box: Enter sends;
// shift+enter / alt+enter / ctrl+j insert a newline; ctrl+r searches sent
// messages; @ completes session and project mentions; Esc (or jk) leaves
// without sending and keeps the draft for that session. Pastes arrive as
// one bracketed paste and are inserted verbatim, except dropped image files
// and ctrl+v clipboard images, which become pending images
// (compose_images.go); ctrl+x removes the last one.

type composeOverlay struct {
	sid, title, node string
	instance         string
	ta               textarea.Model
	steerOK          bool
	steerReason      string
	queueNote        string
	mode             contracts.MessageMode
	replyTo          string // message ID this answers (threaded reply)
	replyText        string // first line of the message being answered
	sending          bool
	errLine          string
	uncertain        bool
	images           []pendingImage // published and attached on send
	note             string         // why a pasted image was not attached

	lastJ   time.Time
	mention int  // selected @ completion
	rsearch bool // ctrl+r reverse search active
	rquery  string
	rskip   int
}

func (m *Model) openCompose(s contracts.SessionView) tea.Cmd {
	ta := textarea.New()
	ta.Placeholder = "Message " + s.Session.Title + " — enter sends, shift+enter newline, @ mentions"
	ta.ShowLineNumbers = false
	ta.CharLimit = 8000
	ta.SetHeight(5)
	ta.Prompt = "│ "
	c := &composeOverlay{sid: s.Session.SessionID, title: s.Session.Title, node: s.NodeName,
		instance: contracts.NewID(contracts.PrefixOperation), ta: ta, mode: contracts.ModeQueue}
	if s.Run != nil && contracts.HasCap(s.Run.Capabilities, contracts.CapInputSteer) && !s.Run.State.Terminal() {
		c.steerOK = true
	} else if s.Run == nil || s.Run.State.Terminal() {
		c.steerReason = "no live run"
	} else {
		c.steerReason = "not supported by " + harnessName(s)
	}
	if r := m.actionReason(s, "message"); r != "" {
		c.queueNote = r
	}
	if d, ok := m.drafts()[c.sid]; ok {
		c.ta.SetValue(d)
	}
	c.images = m.draftImages[c.sid]
	m.overlay = c
	return c.ta.Focus()
}

func (m *Model) drafts() map[string]string {
	if m.draftMap == nil {
		m.draftMap = map[string]string{}
	}
	return m.draftMap
}

// keepDraft stores the text and pending images for the next compose to
// this session.
func (c *composeOverlay) keepDraft(m *Model) {
	m.drafts()[c.sid] = c.ta.Value()
	if m.draftImages == nil {
		m.draftImages = map[string][]pendingImage{}
	}
	m.draftImages[c.sid] = c.images
}

func (m *Model) rememberMessage(body string) {
	if n := len(m.msgHist); n > 0 && m.msgHist[n-1] == body {
		return
	}
	m.msgHist = append(m.msgHist, body)
	if len(m.msgHist) > 200 {
		m.msgHist = m.msgHist[len(m.msgHist)-200:]
	}
}

func (c *composeOverlay) hints() []hint {
	if c.rsearch {
		return []hint{{"ctrl+r", "older"}, {"enter", "use"}, {"esc", "cancel"}}
	}
	h := []hint{{"enter", "send"}, {"shift+enter", "newline"}, {"ctrl+r", "history"}, {"@", "mention"}, {"ctrl+v", "paste image"}}
	if len(c.images) > 0 {
		h = append(h, hint{"ctrl+x", "remove image"})
	}
	h = append(h, hint{"esc", "close (draft kept)"})
	if c.steerOK {
		h = append(h, hint{"tab", "queue/steer"})
	}
	return h
}

func (c *composeOverlay) updateMsg(m *Model, msg tea.Msg) tea.Cmd {
	if p, ok := msg.(tea.PasteMsg); ok {
		if c.sending {
			return nil
		}
		return c.paste(p.Content)
	}
	var cmd tea.Cmd
	c.ta, cmd = c.ta.Update(msg)
	return cmd
}

// paste inserts pasted text, except dropped image files, which become
// pending images.
func (c *composeOverlay) paste(text string) tea.Cmd {
	res := takeDropped(text)
	c.addImages(res)
	if res.text == "" {
		return nil
	}
	var cmd tea.Cmd
	c.ta, cmd = c.ta.Update(tea.PasteMsg{Content: res.text})
	return cmd
}

// addImages adds dropped or pasted images (up to maxComposeImages) and
// says why any could not be attached.
func (c *composeOverlay) addImages(res dropResult) {
	notes := res.notes
	for _, img := range res.images {
		if len(c.images) >= maxComposeImages {
			notes = append(notes, fmt.Sprintf("%s not attached: a message can carry at most %d images", img.name, maxComposeImages))
			continue
		}
		c.images = append(c.images, img)
	}
	if len(res.images) > 0 || len(notes) > 0 {
		c.note = strings.Join(notes, "; ")
	}
}

// typedImage turns a path the terminal typed as keys (a drop without
// bracketed paste) into a pending image once the trailing space arrives.
func (c *composeOverlay) typedImage() {
	v := c.ta.Value()
	lines := strings.Split(v, "\n")
	if c.ta.Line() != len(lines)-1 || c.ta.Column() != len([]rune(lines[len(lines)-1])) {
		return // typing in the middle of the text
	}
	i, res, ok := typedDrop(v)
	if !ok {
		return
	}
	if len(res.images) == 0 {
		c.note = strings.Join(res.notes, "; ") // stays text
		return
	}
	c.ta.SetValue(v[:i] + res.text)
	c.ta.CursorEnd()
	c.addImages(res)
}

// pasteClipboard (ctrl+v) attaches the clipboard's image, or pastes its
// text the way the textarea always did.
func (c *composeOverlay) pasteClipboard(m *Model) tea.Cmd {
	read := m.opts.ReadClipboard
	return m.do(func(ctx context.Context) (any, error) { return read(ctx) },
		func(m *Model, v any, err error) tea.Cmd {
			if m.overlay != c || c.sending {
				return nil
			}
			cc, _ := v.(ClipboardContent)
			switch {
			case errors.Is(err, ErrNoClipboardTool):
				c.note = err.Error()
				return c.paste(cc.Text)
			case err != nil:
				c.note = "clipboard: " + errText(err)
				return nil
			case cc.Data == nil && cc.Path == "":
				return c.paste(cc.Text)
			}
			img, err := clipboardPendingImage(cc)
			if err != nil {
				c.addImages(dropResult{notes: []string{err.Error()}})
				return nil
			}
			c.addImages(dropResult{images: []pendingImage{img}})
			return nil
		})
}

// removeImage drops the last pending image.
func (c *composeOverlay) removeImage() {
	if n := len(c.images); n > 0 {
		c.images = c.images[: n-1 : n-1]
		c.note = ""
	}
}

// mentionQuery returns the @word being typed at the end of the text.
func (c *composeOverlay) mentionQuery() (string, bool) {
	v := c.ta.Value()
	i := strings.LastIndexAny(v, " \n\t")
	word := v[i+1:]
	if strings.HasPrefix(word, "@") {
		return word[1:], true
	}
	return "", false
}

func (c *composeOverlay) mentions(m *Model) []completion {
	q, ok := c.mentionQuery()
	if !ok {
		return nil
	}
	type sc struct {
		c     completion
		score int
	}
	var out []sc
	for _, s := range m.sessions() {
		if s.Session.Archived || s.Session.SessionID == c.sid {
			continue
		}
		if score, pos, ok := matchTerms(q, s.Session.Title, s.NodeName); ok {
			out = append(out, sc{completion{insert: s.Session.Title, label: s.Session.Title, desc: "session · " + s.NodeName, pos: pos}, score})
		}
	}
	for _, p := range m.projects() {
		if p.Archived {
			continue
		}
		if score, pos, ok := matchTerms(q, p.Name); ok {
			out = append(out, sc{completion{insert: p.Name, label: p.Name, desc: "project", pos: pos}, score})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	res := make([]completion, 0, min(6, len(out)))
	for i := 0; i < len(out) && i < 6; i++ {
		res = append(res, out[i].c)
	}
	return res
}

func (c *composeOverlay) historyMatch(m *Model) (string, bool) {
	skip := c.rskip
	for i := len(m.msgHist) - 1; i >= 0; i-- {
		if strings.Contains(strings.ToLower(m.msgHist[i]), strings.ToLower(c.rquery)) {
			if skip == 0 {
				return m.msgHist[i], true
			}
			skip--
		}
	}
	return "", false
}

func (c *composeOverlay) update(m *Model, k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	if c.rsearch {
		switch key {
		case "esc", "ctrl+g":
			c.rsearch = false
		case "ctrl+r":
			c.rskip++
			if _, ok := c.historyMatch(m); !ok {
				c.rskip--
			}
		case "enter", "tab":
			if h, ok := c.historyMatch(m); ok {
				c.ta.SetValue(h)
				c.ta.CursorEnd()
			}
			c.rsearch = false
		case "backspace":
			if r := []rune(c.rquery); len(r) > 0 {
				c.rquery = string(r[:len(r)-1])
			}
			c.rskip = 0
		default:
			if k.Text != "" && k.Mod&^tea.ModShift == 0 {
				c.rquery += k.Text
				c.rskip = 0
			}
		}
		return nil
	}
	if ms := c.mentions(m); len(ms) > 0 {
		switch key {
		case "up", "ctrl+k":
			c.mention = max(0, c.mention-1)
			return nil
		case "down":
			c.mention = min(len(ms)-1, c.mention+1)
			return nil
		case "tab", "enter":
			q, _ := c.mentionQuery()
			v := c.ta.Value()
			c.ta.SetValue(v[:len(v)-len(q)] + ms[min(c.mention, len(ms)-1)].insert + " ")
			c.ta.CursorEnd()
			c.mention = 0
			return nil
		}
	}
	// jk leaves the composer (owner override; keys.toml jk_escape).
	if m.km.JKEscape && key == "k" && !c.lastJ.IsZero() && time.Since(c.lastJ) <= m.km.JKTimeout {
		c.lastJ = time.Time{}
		var cmd tea.Cmd
		c.ta, cmd = c.ta.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
		c.keepDraft(m)
		m.overlay = nil
		return cmd
	}
	if key == "j" {
		c.lastJ = time.Now()
	} else {
		c.lastJ = time.Time{}
	}
	switch key {
	case "esc":
		c.keepDraft(m)
		m.overlay = nil
		return nil
	case "tab":
		if c.steerOK {
			if c.mode == contracts.ModeQueue {
				c.mode = contracts.ModeSteer
			} else {
				c.mode = contracts.ModeQueue
			}
		}
		return nil
	case "enter", "ctrl+s":
		return c.send(m)
	case "shift+enter", "alt+enter", "ctrl+j":
		if !c.sending {
			c.ta.InsertString("\n")
		}
		return nil
	case "ctrl+r":
		c.rsearch, c.rquery, c.rskip = true, "", 0
		return nil
	}
	if c.sending {
		return nil
	}
	switch {
	case key == "ctrl+v":
		return c.pasteClipboard(m)
	case key == "ctrl+x", key == "backspace" && c.ta.Value() == "" && len(c.images) > 0:
		c.removeImage()
		return nil
	}
	c.mention = 0
	var cmd tea.Cmd
	c.ta, cmd = c.ta.Update(k)
	if k.Text == " " {
		c.typedImage()
	}
	return cmd
}

func (c *composeOverlay) view(m *Model, w, h int) []string {
	bw := min(96, w-2)
	inner := bw - 4
	c.ta.SetWidth(inner)
	mode := m.th.dim.Render("queue") + m.th.dim.Render(" · delivered when the runtime can accept input")
	if c.mode == contracts.ModeSteer {
		mode = m.th.tone(toneWarn, "steer") + m.th.dim.Render(" · explicit steering into the current turn")
	}
	lines := []string{m.th.label.Render("To   ") + m.th.bold.Render(c.title) + m.th.dim.Render("  on "+c.node)}
	if c.replyTo != "" {
		lines = append(lines, m.th.label.Render("Re   ")+m.th.dim.Render(trunc("“"+c.replyText+"”  ("+shortID(c.replyTo)+")", inner-5)))
	}
	lines = append(lines, m.th.label.Render("Mode ")+mode)
	if !c.steerOK {
		lines = append(lines, m.th.dim.Render("     steer unavailable: "+c.steerReason))
	}
	if c.queueNote != "" {
		lines = append(lines, m.th.tone(toneWarn, c.queueNote))
	}
	lines = append(lines, "")
	for i, img := range c.images {
		lines = append(lines, m.th.label.Render("Image")+" "+trunc(fmt.Sprintf("%d. %s · %s", i+1, img.name, humanBytes(int64(len(img.data)))), inner-6))
	}
	if c.note != "" {
		for _, l := range wrap(c.note, inner) {
			lines = append(lines, m.th.tone(toneWarn, l))
		}
	}
	lines = append(lines, strings.Split(c.ta.View(), "\n")...)
	if ms := c.mentions(m); len(ms) > 0 {
		lines = append(lines, m.th.faint.Render(strings.Repeat("─", inner)))
		for i, cp := range ms {
			sel := i == c.mention
			row := m.cursor(sel) + "@" + m.highlight(cp.label, cp.pos, styleIf(sel, m.th.selText)) + "  " + m.th.dim.Render(cp.desc)
			lines = append(lines, m.rowSel(sel, trunc(row, inner), inner))
		}
	}
	if c.rsearch {
		match, ok := c.historyMatch(m)
		if !ok {
			match = m.th.dim.Render("(no match)")
		}
		lines = append(lines, "", m.th.key.Render("history ›")+" "+c.rquery+m.th.accent.Render("▏")+"  "+trunc(firstLine(match), max(0, inner-20-width(c.rquery))))
	}
	lines = append(lines, "")
	if c.sending {
		lines = append(lines, m.th.dim.Render("sending…"))
	}
	if c.errLine != "" {
		tn := toneBad
		if c.uncertain {
			tn = toneWarn
		}
		for _, l := range wrap(c.errLine, inner) {
			lines = append(lines, m.th.tone(tn, l))
		}
	}
	lines = append(lines, m.th.dim.Render("Delivery is tracked as separate facts: stored · runtime accepted · acknowledged."))
	return m.box("Message", lines, bw, w, h)
}
