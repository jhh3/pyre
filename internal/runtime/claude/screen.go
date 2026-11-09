package claude

import (
	"regexp"
	"strings"
	"unicode"
)

// screenInfo is what Pyre reads from Claude's Ink TUI via capture-pane.
// Calibrated on Claude Code 2.1.289: the input box is a "❯ " line directly
// below a full-width "────" rule with another rule below the box; an empty
// box shows a dim (SGR 2) placeholder such as `Try "fix lint errors"`.
type screenInfo struct {
	HasInputBox bool
	InputLines  []string // box contents (prompt glyph/indent stripped)
	Placeholder bool
	Dialog      string // title of a dialog covering the input ("" if none)
	DialogKind  string // "trust" | "devchannels" | "onboarding" | "permission" | "login" | "unknown"
	// Prompt: the permission prompt's question and options (DialogKind
	// "permission", when they could be read).
	Prompt *tuiPrompt
	// Onboarding: which first-run onboarding screen is shown ("theme" or
	// "security"); both are answered with Enter.
	Onboarding string
	TrustYes   bool // trust dialog: "Yes, I trust this folder" is selected
	// DevAccept: development-channels warning, its "I am using this for
	// local development" option is selected.
	DevAccept bool
	// LimitAutoResume: the usage-limit menu offers "Don’t continue
	// automatically" (someone chose to wait and continue at the reset).
	LimitAutoResume bool
	RuleWidth       int // width of the rule above the input box (Claude draws it across the pane)
	Lines           []string
}

// InputEmpty reports an input box with no draft text.
func (s screenInfo) InputEmpty() bool {
	if !s.HasInputBox {
		return false
	}
	if s.Placeholder {
		return true
	}
	for _, l := range s.InputLines {
		if strings.TrimSpace(l) != "" {
			return false
		}
	}
	return true
}

// Working reports Claude's busy indicator ("esc to interrupt", shown with
// the spinner while a turn or tool runs) near the bottom of the screen.
func (s screenInfo) Working() bool {
	start := len(s.Lines) - 12
	if start < 0 {
		start = 0
	}
	for _, l := range s.Lines[start:] {
		if strings.Contains(strings.ToLower(l), "esc to interrupt") {
			return true
		}
	}
	return false
}

// Clear reports a screen with the input box and no dialog over it.
func (s screenInfo) Clear() bool { return s.HasInputBox && s.Dialog == "" }

// InputText joins the box lines.
func (s screenInfo) InputText() string { return strings.Join(s.InputLines, "\n") }

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

func isRule(l string) bool {
	t := strings.TrimSpace(l)
	if len([]rune(t)) < 10 {
		return false
	}
	for _, r := range t {
		if r != '─' && r != '━' && r != '═' {
			return false
		}
	}
	return true
}

const promptGlyph = "❯"

// altPromptGlyph is the ASCII marker Claude Code draws on Windows (2.1.296
// on a GCP Windows node) where macOS/Linux show promptGlyph.
const altPromptGlyph = ">"

// glyphPrefix reports whether s starts with either prompt marker and returns
// the marker's byte length.
func glyphPrefix(s string) (int, bool) {
	if strings.HasPrefix(s, promptGlyph) {
		return len(promptGlyph), true
	}
	if strings.HasPrefix(s, altPromptGlyph) {
		return len(altPromptGlyph), true
	}
	return 0, false
}

// glyphIndex is strings.Index for either prompt marker (the first found).
func glyphIndex(s string) (int, int) {
	if i := strings.Index(s, promptGlyph); i >= 0 {
		return i, len(promptGlyph)
	}
	if i := strings.Index(s, altPromptGlyph); i >= 0 {
		return i, len(altPromptGlyph)
	}
	return -1, 0
}

// parseScreen parses a plain capture (-p -J) and, optionally, the same
// capture with escapes (-e) for placeholder detection.
func parseScreen(plain, escaped string) screenInfo {
	lines := strings.Split(strings.TrimRight(plain, "\n"), "\n")
	si := screenInfo{Lines: lines}
	// Find the input box: last "❯" line directly under a rule, with a
	// closing rule below.
	box := -1
	for i := len(lines) - 1; i > 0; i-- {
		if _, ok := glyphPrefix(strings.TrimLeft(lines[i], " ")); ok && isRule(lines[i-1]) {
			box = i
			break
		}
	}
	if box >= 0 {
		end := -1
		for j := box + 1; j < len(lines) && j <= box+60; j++ {
			if isRule(lines[j]) {
				end = j
				break
			}
		}
		if end > 0 {
			si.HasInputBox = true
			si.RuleWidth = len([]rune(strings.TrimSpace(lines[box-1])))
			for j := box; j < end; j++ {
				l := lines[j]
				if j == box {
					l = strings.TrimLeft(l, " ")
					if n, ok := glyphPrefix(l); ok {
						l = l[n:]
					}
					l = strings.TrimPrefix(l, " ")
				} else {
					l = strings.TrimPrefix(l, "  ")
				}
				si.InputLines = append(si.InputLines, strings.TrimRight(l, " "))
			}
			si.Placeholder = placeholder(si.InputLines, escaped)
		}
	}
	all := strings.Join(lines, "\n")
	if !si.HasInputBox {
		si.Prompt = parseTUIPrompt(lines)
	}
	switch {
	case strings.Contains(all, "Is this a project you created or one you trust") ||
		(strings.Contains(all, "Accessing workspace") && strings.Contains(all, "trust this folder")):
		si.DialogKind, si.Dialog = "trust", "workspace trust"
		for _, l := range lines {
			t := strings.TrimSpace(l)
			// Claude on Windows marks the selected option with ">" instead
			// of "❯" (2.1.296 on a GCP Windows node); without this the
			// automation pressed Down forever between the two options.
			if _, ok := glyphPrefix(t); ok && strings.Contains(t, "Yes") {
				si.TrustYes = true
			}
		}
	case !si.HasInputBox && strings.Contains(all, themePickerTitle):
		// Claude's first onboarding screen, shown whenever the config
		// lacks hasCompletedOnboarding (2.1.302 on win-home sat an hour
		// on it). It has no numbered options or key hints, so it never
		// looked like a dialog.
		si.DialogKind, si.Dialog, si.Onboarding = "onboarding", "Claude first-run theme picker", "theme"
	case !si.HasInputBox && strings.Contains(all, securityNotesTitle) && strings.Contains(all, pressEnterToContinue):
		si.DialogKind, si.Dialog, si.Onboarding = "onboarding", "Claude first-run security notes", "security"
	case !si.HasInputBox && strings.Contains(all, devChannelsTitle) && strings.Contains(all, devChannelsAccept):
		// Claude 2.1.299 shows it at every start with
		// --dangerously-load-development-channels: confirm or Exit.
		si.DialogKind, si.Dialog = "devchannels", "development channels warning"
		for _, l := range lines {
			t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "│"))
			if optionRe.MatchString(t) && strings.Contains(t, devChannelsAccept) {
				si.DevAccept = true
			}
		}
	case !si.HasInputBox && (si.Prompt != nil || strings.Contains(all, "Do you want to proceed?") ||
		strings.Contains(all, "Do you want to make this edit") || strings.Contains(all, "Do you want to create")):
		si.DialogKind, si.Dialog = "permission", "permission prompt"
	case !si.HasInputBox && (strings.Contains(all, "Select login method") || strings.Contains(all, "Please run /login")):
		si.DialogKind, si.Dialog = "login", "login required"
	case !si.HasInputBox && limitMenu(lines):
		si.DialogKind, si.Dialog = "limit", limitMenuDialog
		si.LimitAutoResume = strings.Contains(all, limitMenuCancelAuto)
	case !si.HasInputBox && looksLikeDialog(lines):
		si.DialogKind, si.Dialog = "unknown", dialogTitle(lines)
	}
	if si.DialogKind != "permission" {
		si.Prompt = nil
	}
	return si
}

// The development-channels startup warning (Claude Code 2.1.299 binary).
const (
	devChannelsTitle  = "Loading development channels"
	devChannelsAccept = "I am using this for local development"
)

// Claude's first-run onboarding screens (Claude Code 2.1.304 binary and a
// fresh CLAUDE_CONFIG_DIR, 2026-10-20): the theme picker ("Let's get
// started." above it) and, for a logged-in profile, the security notes.
const (
	themePickerTitle     = "Choose the text style that looks best with your terminal"
	securityNotesTitle   = "Security notes:"
	pressEnterToContinue = "Press Enter to continue"
)

// optionRe matches a selected numbered dialog option ("❯ 1. Yes"). Past
// prompts in the scrollback also start with "❯ ", so unnumbered lines never
// count as dialog options.
var optionRe = regexp.MustCompile(`^\s*(?:` + promptGlyph + `|` + altPromptGlyph + `)\s*\d+\.\s`)

func looksLikeDialog(lines []string) bool {
	all := strings.Join(lines, "\n")
	if strings.Contains(all, "Enter to confirm") || strings.Contains(all, "Esc to cancel") ||
		strings.Contains(all, "Press Enter to continue") || strings.Contains(all, "Enter to select") {
		return true
	}
	for _, l := range lines {
		if optionRe.MatchString(l) {
			return true
		}
	}
	return false
}

// dialogTitle names a dialog Pyre does not know by its own title. Claude
// draws its dialogs at the bottom of the pane, most under a rule (─, or ▔
// for a bottom panel) with the title on the first row below it; without a
// rule, the title is the row above the options. The rows above a dialog are
// the transcript and status lines (agent rows "● implementer(…)", spinner
// lines "✻ Waiting for 2 background agents to finish"), never its title:
// naming the pane's top row made a limit menu read "needs human: ●
// implementer(…)" (iss_…).
func dialogTitle(lines []string) string {
	anchor := -1 // the dialog's last option or key-hint row
	for i := len(lines) - 1; i >= 0; i-- {
		if optionRe.MatchString(lines[i]) || dialogHint(lines[i]) {
			anchor = i
			break
		}
	}
	if anchor < 0 {
		return "unrecognised screen"
	}
	top := max(0, anchor-dialogMaxRows)
	for i := anchor; i >= top; i-- {
		if !isSeparator(lines[i]) {
			continue
		}
		for _, l := range lines[i+1 : anchor+1] {
			if t := strings.TrimSpace(l); t != "" {
				return truncStr(t, 80)
			}
		}
		break
	}
	// No rule: the nearest row above the options and hints.
	i := anchor
	for i >= top && (optionRe.MatchString(lines[i]) || numberedRe.MatchString(lines[i]) || dialogHint(lines[i]) || strings.TrimSpace(lines[i]) == "") {
		i--
	}
	if i >= top {
		if t := strings.TrimSpace(lines[i]); !isBanner(t) {
			return truncStr(t, 80)
		}
	}
	return "unrecognised screen"
}

// dialogMaxRows bounds how far above its last row a dialog's title (or the
// limit menu's title) is looked for.
const dialogMaxRows = 30

// numberedRe matches an unselected numbered option row ("   2. No").
var numberedRe = regexp.MustCompile(`^\s+\d+\.\s`)

// dialogHint reports a dialog's key-hint row ("Enter to confirm · Esc to
// cancel").
func dialogHint(l string) bool {
	return strings.Contains(l, "Enter to confirm") || strings.Contains(l, "Esc to cancel") ||
		strings.Contains(l, pressEnterToContinue) || strings.Contains(l, "Enter to select")
}

// isSeparator reports a rule Claude draws above a dialog: the input box's
// (─) or the top edge of a bottom panel (▔, the usage-limit menu of Claude
// Code 2.1.304).
func isSeparator(l string) bool {
	if isRule(l) {
		return true
	}
	t := strings.TrimSpace(l)
	return len([]rune(t)) >= 10 && strings.Trim(t, "▔") == ""
}

// isBanner reports a row of Claude's logo.
func isBanner(t string) bool { return strings.HasPrefix(t, "▐") || strings.HasPrefix(t, "▝") }

// Claude's usage-limit menu (Claude Code 2.1.304 binary: the rate-limit
// options dialog). Claude shows it when a turn fails at a usage, session or
// spend limit: titled "What do you want to do?", with "Stop and wait for
// limit to reset" (or "Stop") and, as Claude offers them, "Wait here, then
// continue automatically …", "Don’t continue automatically", "Ask your
// admin for more usage", "Switch to usage credits" / "Add funds to continue
// …" and "Upgrade your plan". Its Escape is "Stop": the turn has already
// ended, and the menu only closes.
const (
	limitMenuTitle      = "What do you want to do?"
	limitMenuDialog     = "Claude's usage-limit menu"
	limitMenuCancelAuto = "Don’t continue automatically"
)

var limitMenuOptions = []string{"Stop and wait for limit to reset", "continue automatically", limitMenuCancelAuto,
	"Ask your admin for more usage", "Switch to usage credits", "Add funds to continue", "Upgrade your plan"}

// limitMenu reports Claude's usage-limit menu at the bottom of the screen:
// its title with one of its options below it.
func limitMenu(lines []string) bool {
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-dialogMaxRows; i-- {
		if strings.TrimSpace(lines[i]) != limitMenuTitle {
			continue
		}
		for _, l := range lines[i+1:] {
			for _, o := range limitMenuOptions {
				if strings.Contains(l, o) {
					return true
				}
			}
		}
		return false
	}
	return false
}

// placeholder: an empty input box renders dim (SGR 2) text directly after
// the prompt glyph. Falls back to the `Try "…"` heuristic without escapes.
func placeholder(box []string, escaped string) bool {
	if len(box) != 1 {
		return false
	}
	if escaped != "" {
		el := strings.Split(strings.TrimRight(escaped, "\n"), "\n")
		for i := len(el) - 1; i >= 0; i-- {
			idx, glen := glyphIndex(el[i])
			if idx < 0 {
				continue
			}
			if i == 0 || !isRule(stripANSI(el[i-1])) {
				continue
			}
			rest := el[i][idx+glen:]
			// skip spaces and non-dim SGR sequences before the first glyph
			for {
				rest = strings.TrimLeft(rest, "  ")
				loc := ansiRe.FindStringIndex(rest)
				if loc == nil || loc[0] != 0 {
					break
				}
				seq := rest[:loc[1]]
				if seq == "\x1b[2m" || strings.HasPrefix(seq, "\x1b[2;") || strings.Contains(seq, ";2m") {
					return true
				}
				rest = rest[loc[1]:]
			}
			if strings.TrimSpace(stripANSI(rest)) == "" {
				return true
			}
			return false
		}
	}
	t := strings.TrimSpace(box[0])
	return strings.HasPrefix(t, `Try "`) && strings.HasSuffix(t, `"`)
}

// markerOnInput reports whether the input box shows the marker, or a
// collapsed long paste ("[Pasted text #N …]") when the box was empty before.
func (s screenInfo) markerOnInput(marker string, wasEmpty bool) bool {
	if !s.HasInputBox || s.Placeholder {
		return false
	}
	txt := s.InputText()
	if strings.Contains(txt, marker) {
		return true
	}
	return wasEmpty && strings.Contains(txt, "[Pasted text #")
}

// Claude Code shows only the last six rows of a long draft in its input box
// (calibrated on 2.1.289), and wraps rows at the pane width. A paste of a few
// hundred characters into a narrow pane therefore scrolls its first line,
// which carries the marker, out of view. Pastes over 800 characters collapse
// into a "[Pasted text #N +k lines]" placeholder instead.

// normDraft drops every whitespace character: Claude wraps a draft at the
// box width and indents continuation rows, so only the other characters
// compare between the text Pyre pasted and what the box shows.
func normDraft(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// minTailMatch is the least number of non-space characters the box must show
// for a tail match (unless the whole text is shorter).
const minTailMatch = 40

// maxDraftTail bounds the normalised tail kept in the delivery journal for
// recognising a leftover draft later. Inline drafts are at most about 800
// characters (longer pastes collapse), so this covers every inline draft.
const maxDraftTail = 1024

// draftTail returns the normalised tail of text that the journal keeps.
func draftTail(text string) string {
	n := normDraft(text)
	if len(n) > maxDraftTail {
		n = n[len(n)-maxDraftTail:]
		for len(n) > 0 && !utf8RuneStart(n[0]) {
			n = n[1:]
		}
	}
	return n
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// tailMatch reports whether the visible box text vis (normalised) is the
// end of the normalised text norm: every non-space character the box shows
// is the end of Pyre's text, in order, and nothing else is visible. The box
// was empty before the paste and the cursor sits at the end of the paste,
// so the visible rows are the end of the draft.
func tailMatch(vis, norm string) bool {
	if vis == "" || !strings.HasSuffix(norm, vis) {
		return false
	}
	return len(vis) >= minTailMatch || vis == norm
}

// pasteVisible reports how the input box shows Pyre's paste of text whose
// marker is marker, after the box was seen empty: "marker" (the marker is
// visible), "collapsed" (Claude's long-paste placeholder), "tail" (the box
// shows only the end of the text; the marker scrolled out of view) or "".
func (s screenInfo) pasteVisible(marker, text string) string {
	if !s.HasInputBox || s.Placeholder {
		return ""
	}
	txt := s.InputText()
	switch {
	case strings.Contains(txt, marker):
		return "marker"
	case strings.Contains(txt, "[Pasted text #"):
		return "collapsed"
	case tailMatch(normDraft(txt), normDraft(text)):
		return "tail"
	}
	return ""
}

// showsDraft reports whether the box holds a leftover draft of a message:
// its marker is visible, or the visible text is the end of the journaled
// tail of the paste.
func (s screenInfo) showsDraft(marker, tail string) bool {
	if !s.HasInputBox || s.Placeholder || s.InputEmpty() {
		return false
	}
	txt := s.InputText()
	return strings.Contains(txt, marker) || (tail != "" && tailMatch(normDraft(txt), tail))
}
