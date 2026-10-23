package agy

import (
	"strings"
	"unicode/utf8"
)

// screenInfo is what Pyre reads from agy's TUI (validated on agy 1.4.2):
//
//	──────────────────── (full-width rule)
//	> input text
//	  continuation lines
//	────────────────────
//	? for shortcuts                    Gemini 4 Argon · medium
//
// The footer says "? for shortcuts" while idle with an empty box, "esc to
// cancel" while a turn runs (also under a slash-command menu), and only the
// model while the box holds a draft. Dialogs (workspace trust, tool
// permission) replace the input box and show "↑/↓ Navigate".
type screenInfo struct {
	Lines       []string
	HasInputBox bool
	Input       string // the input box text ("" when empty)
	Footer      string
	Dialog      string // "" or a description of the dialog covering the input box
	DialogKind  string // "trust" | "permission" | "other"
	TrustYes    bool   // trust dialog with "Yes, I trust this folder" selected
	Queued      bool   // agy shows natively queued input ("▸ ... (alt+m to send immediately)")
}

// Working reports a turn in progress (footer "esc to cancel" under the
// input box, not a slash-command menu).
func (s screenInfo) Working() bool {
	return s.HasInputBox && strings.Contains(s.Footer, "esc to cancel") && !strings.HasPrefix(strings.TrimSpace(s.Input), "/")
}

// Idle reports agy waiting at an empty prompt.
func (s screenInfo) Idle() bool {
	return s.HasInputBox && s.Dialog == "" && strings.Contains(s.Footer, "? for shortcuts")
}

// InputEmpty reports an empty input box.
func (s screenInfo) InputEmpty() bool { return s.HasInputBox && strings.TrimSpace(s.Input) == "" }

func isRule(l string) bool {
	t := strings.TrimSpace(l)
	if utf8.RuneCountInString(t) < 40 {
		return false
	}
	for _, r := range t {
		if r != '─' {
			return false
		}
	}
	return true
}

func parseScreen(plain string) screenInfo {
	lines := strings.Split(strings.TrimRight(plain, "\n"), "\n")
	si := screenInfo{Lines: lines}
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(lines[last]) == "" {
		last--
	}
	if last < 0 {
		return si
	}
	si.Footer = strings.TrimSpace(lines[last])
	var rules []int
	for i := 0; i <= last; i++ {
		if isRule(lines[i]) {
			rules = append(rules, i)
		}
	}
	// The input box is the bottom-most pair of rules whose first line starts
	// with the prompt ">".
	for k := len(rules) - 2; k >= 0; k-- {
		top, bot := rules[k], rules[k+1]
		if bot-top < 2 {
			continue
		}
		first := lines[top+1]
		if first != ">" && !strings.HasPrefix(first, "> ") && strings.TrimRight(first, " ") != ">" {
			continue
		}
		si.HasInputBox = true
		var parts []string
		for i := top + 1; i < bot; i++ {
			l := strings.TrimRight(lines[i], " ")
			if i == top+1 {
				l = strings.TrimPrefix(strings.TrimPrefix(l, ">"), " ")
			} else {
				l = strings.TrimPrefix(l, "  ")
			}
			parts = append(parts, l)
		}
		si.Input = strings.TrimRight(strings.Join(parts, "\n"), "\n ")
		break
	}
	for i := 0; i <= last; i++ {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "▸ ") && strings.Contains(l, "send immediately") {
			si.Queued = true
		}
	}
	navigate := false
	for i := 0; i <= last; i++ {
		if strings.Contains(lines[i], "↑/↓ Navigate") {
			navigate = true
		}
	}
	for i := 0; i <= last; i++ {
		l := strings.TrimSpace(lines[i])
		switch {
		case strings.Contains(l, "Do you trust the contents of this project?"):
			si.Dialog, si.DialogKind = "agy workspace trust dialog", "trust"
		case si.DialogKind == "trust" && strings.HasPrefix(l, "> ") && strings.Contains(l, "Yes, I trust this folder"):
			si.TrustYes = true
		case strings.HasPrefix(l, "Requesting permission for:") && si.DialogKind == "":
			what := ""
			if i+1 <= last {
				what = strings.TrimSpace(lines[i+1])
			}
			si.Dialog, si.DialogKind = "agy permission prompt: "+clip(what, 100), "permission"
		}
	}
	if si.Dialog == "" && navigate && !si.HasInputBox {
		q := ""
		for i := last; i >= 0; i-- {
			if l := strings.TrimSpace(lines[i]); strings.HasSuffix(l, "?") {
				q = l
				break
			}
		}
		si.Dialog, si.DialogKind = "agy dialog: "+clip(q, 100), "other"
	}
	if si.DialogKind == "trust" || si.DialogKind == "permission" {
		// A dialog replaces the input box; a prompt-looking option line
		// ("> 1. Yes, run command") is not one.
		si.HasInputBox = false
		si.Input = ""
	}
	return si
}

// showsMarker reports whether the input box shows message id's marker,
// plain or dated ("[pyre-msg:<id>]" / "[pyre-msg:<id> · sent …]"; the TUI
// may wrap it).
func (s screenInfo) showsMarker(id string) bool {
	if !s.HasInputBox || s.InputEmpty() || id == "" {
		return false
	}
	box, p := normalize(s.Input), normalize("[pyre-msg:"+id)
	return strings.Contains(box, p+"]") || strings.Contains(box, p+"·")
}

// showsText reports whether the input box shows message id's marker or
// the end of text (a long draft scrolls its start out of view). Only for a
// paste Pyre just made; recognising an older draft needs the marker.
func (s screenInfo) showsText(id, text string) bool {
	if s.showsMarker(id) {
		return true
	}
	if !s.HasInputBox || s.InputEmpty() {
		return false
	}
	t := normalize(text)
	tail := t
	if r := []rune(t); len(r) > 40 {
		tail = string(r[len(r)-40:])
	}
	return tail != "" && strings.Contains(normalize(s.Input), tail)
}

// normalize collapses whitespace (the TUI wraps long lines).
func normalize(s string) string { return strings.Join(strings.Fields(s), "") }
