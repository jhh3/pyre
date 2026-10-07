package tui

import (
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/jhh3/pyre/internal/contracts"
)

// Helpers that make real fleet data read well: terminal snapshots of real
// harness TUIs, raw identifiers and timestamps in hub-provided text, home
// directories and runtime message envelopes.

// compactScreen turns a terminal snapshot into display lines: trailing
// spaces removed, blank lines at the top and bottom dropped, and runs of
// blank lines collapsed to one. A real harness pane (Claude Code, Codex)
// is as tall as the window, with its content at the top and its prompt at
// the bottom; without compaction a bottom-anchored excerpt shows only the
// prompt box and empty rows, and the conversation or the approval question
// above it is cut off.
func compactScreen(screen string) []string {
	if screen == "" {
		return nil
	}
	raw := strings.Split(strings.ReplaceAll(screen, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(raw))
	blank := false
	for _, l := range raw {
		l = trimRightCells(l)
		if ansi.Strip(l) == "" {
			if len(out) > 0 {
				blank = true
			}
			continue
		}
		if blank {
			out = append(out, "")
			blank = false
		}
		out = append(out, l)
	}
	return out
}

// trimRightCells removes trailing spaces from a line that may carry ANSI
// styling (a styled run of spaces at the end is dropped too).
func trimRightCells(l string) string {
	if !strings.Contains(l, "\x1b") {
		return strings.TrimRight(l, " \t")
	}
	w := ansi.StringWidth(strings.TrimRight(ansi.Strip(l), " \t"))
	if w == ansi.StringWidth(l) {
		return l
	}
	return ansi.Truncate(l, w, "")
}

// clip cuts s to w cells without an ellipsis, the way a terminal viewport
// clips a wider pane (rules and right-aligned prompts simply end).
func clip(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if width(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "")
}

var homeRe = regexp.MustCompile(`^(/Users/[^/]+|/home/[^/]+)(/|$)`)

// tildePath abbreviates a home directory prefix (/Users/<u>, /home/<u>) to
// "~", as shells and Claude Code do; the machine is shown beside it.
func tildePath(p string) string {
	if loc := homeRe.FindStringSubmatchIndex(p); loc != nil {
		return "~" + p[loc[3]:]
	}
	return p
}

var rfc3339Re = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)

// humanTimes rewrites RFC 3339 timestamps inside free text (state reasons,
// receipt details) as local clock time plus age: "since 21:20 (11m ago)".
func humanTimes(s string, now time.Time) string {
	if !strings.Contains(s, "T") {
		return s
	}
	return rfc3339Re.ReplaceAllStringFunc(s, func(ts string) string {
		t, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return ts
		}
		lt := t.In(now.Location())
		clock := lt.Format("15:04")
		if lt.Year() != now.Year() || lt.YearDay() != now.YearDay() {
			clock = lt.Format("Jan 2 15:04")
		}
		a := ago(now, t)
		if a == "now" || a == "—" {
			return clock
		}
		return clock + " (" + a + ")"
	})
}

var idRe = regexp.MustCompile(`\b(?:ses|node)_[0-9A-Za-z][0-9A-Za-z-]*`)

// humanIDs replaces session and node IDs inside hub-provided text (inbox
// titles, error messages) with the session title or machine name when
// known. IDs stay where they are the point (the Run row, message detail).
func (m *Model) humanIDs(s string) string {
	if !strings.Contains(s, "_") {
		return s
	}
	return idRe.ReplaceAllStringFunc(s, func(id string) string {
		switch {
		case strings.HasPrefix(id, contracts.PrefixSession+"_"):
			if sv, ok := m.session(id); ok && sv.Session.Title != "" {
				return sv.Session.Title
			}
		case strings.HasPrefix(id, contracts.PrefixNode+"_"):
			if n := m.node(id); n != nil && n.Name != "" {
				return n.Name
			}
		}
		return id
	})
}

// who names a principal for people: "you" for the owner, the session
// title for an agent session, the machine name for a node.
func (m *Model) who(p contracts.Principal) string {
	switch p.Kind {
	case contracts.PrincipalOwner:
		return "you"
	case contracts.PrincipalSession:
		if sv, ok := m.session(p.ID); ok && sv.Session.Title != "" {
			return sv.Session.Title
		}
	case contracts.PrincipalNode:
		if n := m.node(p.ID); n != nil && n.Name != "" {
			return n.Name
		}
	case contracts.PrincipalSystem:
		return "Pyre"
	}
	if p.Name != "" {
		return p.Name
	}
	return string(p.Kind) + ":" + p.ID
}

// whoFull is who plus the stable principal ID, for detail panes where the
// identity itself matters (message sender/recipient, approval resolver).
func (m *Model) whoFull(p contracts.Principal) string {
	s := m.who(p)
	id := string(p.Kind) + ":" + p.ID
	if p.Kind == contracts.PrincipalOwner {
		id = "owner"
	}
	s += " (" + id + ")"
	if p.OnBehalfOf != nil {
		s += " on behalf of " + m.who(*p.OnBehalfOf)
	}
	return s
}

var pyreMsgRe = regexp.MustCompile(`^\[pyre-msg:([^\]]+)\] from (\w+):(\S+)`)

// pyreMessage is a delivered message as it appears in a harness transcript
// ("[pyre-msg:<id>] from <kind>:<id> (<name>) (reply to …):\n<body>").
type pyreMessage struct {
	id, from, context, body string
}

// parsePyreMessage splits the runtime envelope Pyre injects from the body,
// naming the sender for people. ok is false for text without the marker.
func (m *Model) parsePyreMessage(text string) (pyreMessage, bool) {
	sm := pyreMsgRe.FindStringSubmatch(text)
	if sm == nil {
		return pyreMessage{}, false
	}
	pm := pyreMessage{id: sm[1]}
	pm.from = m.who(contracts.Principal{Kind: contracts.PrincipalKind(sm[2]), ID: sm[3]})
	header, body := text, ""
	if i := strings.Index(text, ":\n"); i >= 0 {
		header, body = text[:i], text[i+2:]
	} else if i := strings.Index(text, "): "); i >= 0 {
		header, body = text[:i+1], text[i+3:]
	}
	var ctx []string
	for _, k := range [][2]string{{"steering", "steering"}, {"reply to ", "reply"}, {"thread ", "thread"},
		{"reissue of ", "reissue"}, {"ack requested", "ack requested"}} {
		if strings.Contains(header, k[0]) {
			ctx = append(ctx, k[1])
		}
	}
	pm.context = strings.Join(ctx, ", ")
	pm.body = strings.TrimSpace(body)
	return pm, true
}
