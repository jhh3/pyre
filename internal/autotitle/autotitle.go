// Package autotitle gives sessions good automatic names. A session whose
// title nobody chose carries the reserved tag TagAuto; the node derives a
// better title from the run's native transcript (Claude Code's own
// conversation title, Codex's thread name, else a short title from the first
// user prompt) and reports it as an EventTitleSuggested event, which the hub
// applies only while the session is still automatic. A person's rename
// removes the tag and so stops auto-naming; an agent naming its own session
// replaces it with TagAgent (a person still wins over that).
package autotitle

import (
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/clipperhouse/uax29/v2/graphemes"

	"github.com/jhh3/pyre/internal/contracts"
)

// Reserved session tags (additive; no schema change).
const (
	TagAuto  = "title:auto"  // title is automatic; auto-naming may replace it
	TagAgent = "title:agent" // an agent named it (itself, the session that started it, or a peer)
)

// EventTitleSuggested is the node->hub event kind carrying a derived title
// (ReportEvent, Object = the session, Payload = Suggestion).
const EventTitleSuggested = "session.title_suggested"

// Source names where a derived title came from, best first.
const (
	SourceCustom = "claude_custom_title" // the person used /rename inside Claude
	SourceNative = "native_title"        // Claude's ai-title/summary or Codex thread name
	SourcePrompt = "first_prompt"        // derived from the first user prompt
)

// Suggestion is the event payload.
type Suggestion struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	Title     string `json:"title"`
	Source    string `json:"source"`
}

// IsAuto reports whether tags mark the title as automatic.
func IsAuto(tags []string) bool { return slices.Contains(tags, TagAuto) }

// Reserved reports whether a tag is one of the reserved title: tags (UIs hide them).
func Reserved(tag string) bool { return strings.HasPrefix(tag, "title:") }

// WithoutReserved returns tags minus the reserved title: tags.
func WithoutReserved(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if !Reserved(t) {
			out = append(out, t)
		}
	}
	return out
}

// MaxTitleRunes bounds any title Pyre derives.
const MaxTitleRunes = 60

// FromPrompt makes a short title from a user prompt: Pyre message markers and
// headers, slash commands, XML-ish tags, URLs and code are dropped; the first
// sentence is kept, at most six words. "" when nothing usable remains.
func FromPrompt(prompt string) string {
	s := prompt
	// A Pyre-delivered message: drop the header line ("[pyre-msg:…] from …:").
	if i := strings.Index(s, "[pyre-msg:"); i >= 0 {
		rest := s[i:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			s = s[:i] + rest[nl+1:]
		} else if j := strings.Index(rest, "]"); j >= 0 {
			s = s[:i] + rest[j+1:]
		}
	}
	s = stripTags(s)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[:i]
	}
	var words []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		heading := strings.HasPrefix(line, "#")
		line = strings.TrimLeft(line, "#>*-• \t")
		if line == "" || strings.HasPrefix(line, "/") || heading && len(strings.Fields(line)) < 3 {
			continue // empty, a slash command, or a bare section heading ("## Task")
		}
		cut := firstSentence(line)
		for _, w := range strings.Fields(cut) {
			if strings.Contains(w, "://") || strings.HasPrefix(w, "[pyre") {
				continue
			}
			w = strings.Trim(w, "\"'`*_()[]{}<>")
			if w == "" {
				continue
			}
			words = append(words, w)
			if len(words) == 6 {
				break
			}
		}
		if len(words) > 0 {
			break
		}
	}
	for len(words) > 1 && danglingWord[strings.ToLower(words[len(words)-1])] {
		words = words[:len(words)-1] // "… secret and" reads as cut off
	}
	if len(words) == 0 {
		return ""
	}
	t := strings.Join(words, " ")
	t = strings.TrimRight(t, ".,;:!?-–— ")
	r := []rune(t)
	if len(r) > MaxTitleRunes {
		t = strings.TrimSpace(string(r[:MaxTitleRunes]))
	}
	if t == "" {
		return ""
	}
	r = []rune(t)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

var danglingWord = map[string]bool{"and": true, "or": true, "the": true, "a": true, "an": true, "to": true, "of": true, "for": true,
	"with": true, "in": true, "on": true, "at": true, "by": true, "from": true, "into": true, "then": true, "but": true, "so": true, "my": true, "our": true, "your": true}

// Clean tidies a native title (Claude ai-title, Codex thread name).
func Clean(s string) string {
	s = strings.Join(strings.Fields(stripTags(s)), " ")
	s = strings.Trim(s, "\"'` ")
	if r := []rune(s); len(r) > MaxTitleRunes {
		s = strings.TrimSpace(string(r[:MaxTitleRunes-1])) + "…"
	}
	return s
}

func firstSentence(s string) string {
	for i, r := range s {
		if (r == '.' || r == '?' || r == '!') && (i+1 == len(s) || s[i+1] == ' ') {
			return s[:i]
		}
	}
	return s
}

var tagRe = regexp.MustCompile(`<(/?)([A-Za-z][\w:-]*)[^<>]*>`)

// stripTags removes <tag>…</tag> blocks of injected context (system
// reminders, command wrappers) and any remaining tags.
func stripTags(s string) string {
	for n := 0; n < 200; n++ {
		m := tagRe.FindStringSubmatchIndex(s)
		if m == nil {
			return s
		}
		end := m[1]
		if m[3] == m[2] { // opening tag: drop through its closing tag
			name := s[m[4]:m[5]]
			if k := strings.Index(s[m[1]:], "</"+name+">"); k >= 0 {
				end = m[1] + k + len(name) + 3
			}
		}
		s = s[:m[0]] + " " + s[end:]
	}
	return s
}

// AllowRename authorizes an explicit title change by p on session cur: the
// owner (and other non-session principals) always; an agent session on any
// session (owner decision, W-childnotify: e.g. a coordinator naming its
// reviewers), but only while the title is automatic or agent-chosen, never
// over a title a person set.
func AllowRename(p contracts.Principal, cur contracts.Session, title string) error {
	if strings.TrimSpace(title) == "" {
		return contracts.Errorf(contracts.ErrInvalid, "title cannot be empty")
	}
	// Any principal may rename any session (owner decision 2026-10-06);
	// only automatic titling defers to a title a person chose.
	return nil
}

// RenameTags returns tags after an explicit rename by p: the reserved title:
// tags are dropped (auto-naming stops); an agent's rename is marked title:agent.
func RenameTags(p contracts.Principal, tags []string) []string {
	out := WithoutReserved(tags)
	if p.Kind == contracts.PrincipalSession {
		out = append(out, TagAgent)
	}
	return out
}

// LaunchTags is StartTags plus title:agent when an agent session chose the
// title at launch: such a title stays renamable by agents (W-childnotify),
// unlike a title a person chose.
func LaunchTags(p contracts.Principal, title string, h contracts.Harness, tags []string) []string {
	tags = StartTags(title, h, tags)
	if p.Kind == contracts.PrincipalSession && strings.TrimSpace(title) != "" && !slices.Contains(tags, TagAgent) {
		tags = append(slices.Clone(tags), TagAgent)
	}
	return tags
}

// StartTags adds title:auto to an untitled, non-shell session's tags.
func StartTags(title string, h contracts.Harness, tags []string) []string {
	if strings.TrimSpace(title) != "" || h == contracts.HarnessShell || slices.Contains(tags, TagAuto) {
		return tags
	}
	return append(slices.Clone(tags), TagAuto)
}

// MaxIconBytes bounds a session icon (one grapheme cluster: an emoji with
// modifiers or a ZWJ sequence stays well under it).
const MaxIconBytes = 64

// ValidIcon checks a session icon (W-archive2): empty (no icon) or a
// single emoji — one grapheme cluster, one or two cells wide, not plain
// ASCII or a control character. It returns the trimmed icon.
func ValidIcon(icon string) (string, error) {
	icon = strings.TrimSpace(icon)
	if icon == "" {
		return "", nil
	}
	bad := contracts.Errorf(contracts.ErrInvalid, "icon must be a single emoji (one character, at most two cells wide), got %q", icon)
	if len(icon) > MaxIconBytes {
		return "", bad
	}
	// One grapheme cluster only (uax29, no terminal-width tables: pyred
	// must not link the TUI's width libraries). Emoji render 1–2 cells.
	seg := graphemes.FromString(icon)
	if !seg.Next() || seg.Value() != icon {
		return "", bad
	}
	for _, r := range icon {
		if r < 0x80 && r != 0x23 && r != 0x2a && (r < '0' || r > '9') || unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", bad // keycaps (#️⃣, 1️⃣) start with ASCII; letters never do
		}
	}
	if r := []rune(icon)[0]; r < 0x80 && len([]rune(icon)) == 1 {
		return "", bad // a bare digit, # or *
	}
	return icon, nil
}
