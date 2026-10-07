package tui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Keymap is the effective key configuration: Vim-style navigation, a
// Space leader with a which-key popup, and per-view shortcuts. Defaults can
// be overridden in ~/.config/pyre/keys.toml (see KeysFileFormat).
type Keymap struct {
	Leader     string        // key that starts leader sequences (default "space")
	JKEscape   bool          // typing "jk" quickly leaves text inputs (default on)
	WhichKey   time.Duration // delay before the leader popup appears
	SeqTimeout time.Duration // how long a non-leader prefix (g…) waits
	JKTimeout  time.Duration // max gap between j and k for jk-escape
	// OptionArrows says where ESC b / ESC f (what macOS Terminal.app sends
	// for option+left/right without "Use Option as Meta key") act as
	// alt+left / alt+right: "list" (default: everywhere except inside the
	// embedded terminal, where they stay the harness's word motions),
	// "always" (inside the terminal too) or "off".
	OptionArrows string
	bind         map[string][][]string
	Source       string   // file the overrides came from, if any
	Warnings     []string // problems found in the file (unknown actions, bad keys)
}

// actionDef describes one bindable action.
type actionDef struct {
	id      string // "scope.name"
	group   string // which-key/help group
	desc    string
	keys    []string // default bindings
	special bool     // handled outside the dispatcher (shown in help only)
}

func (a actionDef) scope() string { return a.id[:strings.IndexByte(a.id, '.')] }

// actionDefs is the single source of truth for defaults, the which-key
// popup, the help cheat-sheet and `pyre keys`.
var actionDefs = []actionDef{
	// global
	{id: "global.finder", group: "Find", desc: "find anything (sessions, projects, docs, …)", keys: []string{"<leader><leader>", "ctrl+p"}},
	{id: "global.cmdline", group: "Find", desc: "command line (:attach, :send, …)", keys: []string{":", "ctrl+k"}},
	{id: "global.filter", group: "Find", desc: "filter the current list", keys: []string{"/"}},
	{id: "global.search", group: "Find", desc: "search everything: every transcript on every machine, memory, docs", keys: []string{"<leader>/"}},
	{id: "global.search_transcript", group: "Find", desc: "search the selected session's transcript", keys: []string{"<leader>t"}},
	{id: "global.switcher", group: "Switch", desc: "quick switcher: type to find a session, enter switches (works inside the terminal too)", keys: []string{"alt+s", "<leader>s"}},
	{id: "global.sessions", group: "Go to", desc: "Sessions", keys: []string{"S", "1"}},
	{id: "global.projects", group: "Go to", desc: "Projects", keys: []string{"P", "2", "<leader>p"}},
	{id: "global.machines", group: "Go to", desc: "Machines", keys: []string{"M", "3", "<leader>m"}},
	{id: "global.accounts", group: "Go to", desc: "Accounts", keys: []string{"A", "4", "<leader>a"}},
	{id: "global.grid", group: "Go to", desc: "Grid: toggle the Sessions grid view (every live session as a live tile)", keys: []string{"G", "<leader>g"}},
	{id: "global.digest", group: "Go to", desc: "While you were away: what each agent did since you last looked", keys: []string{"<leader>w"}},
	{id: "global.connections", group: "Go to", desc: "Connections", keys: []string{"<leader>c"}},
	{id: "global.environments", group: "Go to", desc: "Environments & updates", keys: []string{"<leader>e"}},
	{id: "global.activity", group: "Go to", desc: "Activity: what Pyre did on its own (account switches, …)", keys: []string{"<leader>l"}},
	{id: "global.new", group: "Act", desc: "launch a new session", keys: []string{"<leader>n"}},
	{id: "global.refresh", group: "Act", desc: "refresh now", keys: []string{"ctrl+r", "<leader>r"}},
	{id: "global.help", group: "Help", desc: "keys & help (type to search)", keys: []string{"?"}},
	{id: "global.back", group: "Move", desc: "back / close / clear filter", keys: []string{"esc"}},
	{id: "global.quit", group: "Help", desc: "quit (sessions keep running); back in sub-views", keys: []string{"q"}},
	// navigation (any list)
	{id: "nav.down", group: "Move", desc: "down", keys: []string{"j", "down"}},
	{id: "nav.up", group: "Move", desc: "up", keys: []string{"k", "up"}},
	{id: "nav.top", group: "Move", desc: "first item", keys: []string{"g g", "home"}},
	{id: "nav.bottom", group: "Move", desc: "last item", keys: []string{"end"}}, // G opens the Grid
	{id: "nav.half_down", group: "Move", desc: "half page down", keys: []string{"ctrl+d", "pgdown"}},
	{id: "nav.half_up", group: "Move", desc: "half page up", keys: []string{"ctrl+u", "pgup"}},
	{id: "nav.pane_next", group: "Move", desc: "focus the next pane", keys: []string{"l", "tab"}},
	{id: "nav.pane_prev", group: "Move", desc: "focus the previous pane", keys: []string{"h", "shift+tab"}},
	{id: "nav.match_next", group: "Find", desc: "next match (while filtering)", keys: []string{"n"}},
	{id: "nav.match_prev", group: "Find", desc: "previous match (while filtering)", keys: []string{"N"}},
	// sessions
	{id: "sessions.attach", group: "Session", desc: "type into the session's terminal (the pane takes the keyboard)", keys: []string{"enter"}},
	{id: "sessions.attach_full", group: "Session", desc: "attach full-screen (tmux; detach with ctrl+b d)", keys: []string{"F"}},
	{id: "sessions.info", group: "Session", desc: "session details (project, directory, run, delivery)", keys: []string{"i"}},
	{id: "sessions.answer", group: "Session", desc: "approve / answer the pending request", keys: []string{"a"}},
	{id: "sessions.message", group: "Session", desc: "message (queue or steer); replies to its message for you", keys: []string{"m"}},
	{id: "sessions.dismiss", group: "Session", desc: "dismiss its message for you (acknowledge, no reply)", keys: []string{"d"}},
	{id: "sessions.history", group: "Session", desc: "message history & reconcile (delivery states of messages to/from this session)", keys: []string{"H"}},
	{id: "sessions.interrupt", group: "Session", desc: "interrupt (confirms)", keys: []string{"!"}},
	{id: "sessions.stop", group: "Session", desc: "stop (confirms)", keys: []string{"x"}},
	{id: "sessions.archive", group: "Session", desc: "archive (live: stop + archive, confirms); on a machine row: all its ended sessions", keys: []string{"X"}},
	{id: "sessions.worktrees", group: "Session", desc: "review / remove the worktrees the session just archived created (offered in a toast; never pops up on its own)", keys: []string{"W"}},
	{id: "sessions.toggle_ended", group: "Session", desc: "show / hide ended (exited, failed) sessions", keys: []string{"."}},
	{id: "sessions.usage", group: "Session", desc: "show / hide each session's memory and CPU (process tree) in the list", keys: []string{"u"}},
	{id: "sessions.read", group: "Session", desc: "read the transcript full-screen", keys: []string{"r"}},
	{id: "sessions.yank", group: "Session", desc: "copy the session ID (OSC 52)", keys: []string{"y"}},
	{id: "sessions.rename", group: "Session", desc: "rename (your name stays; automatic naming stops)", keys: []string{"R"}},
	{id: "sessions.project", group: "Session", desc: "open or associate a project", keys: []string{"p"}},
	{id: "sessions.new", group: "Session", desc: "new session: a repo, or a path like ~/scratch", keys: []string{"n"}},
	{id: "sessions.pin", group: "Session", desc: "pin / unpin (pinned sessions stay on top, synced to every client)", keys: []string{"*"}},
	{id: "sessions.group", group: "Session", desc: "group by project → attention → machine → repo → none (cycles; remembered)", keys: []string{"g b"}},
	{id: "sessions.sort", group: "Session", desc: "sort inside groups: name → attention (needs you first) → activity (cycles; remembered; also :sort)", keys: []string{"g s"}},
	{id: "sessions.view", group: "Session", desc: "Sessions view: sidenav (list + terminal) → table → grid (cycles; remembered; also :view)", keys: []string{"v"}},
	{id: "sessions.fold", group: "Session", desc: "collapse / expand the group, or a parent session's children", keys: []string{"z a"}},
	// switching (single chords with a modifier work from anywhere, inside the
	// embedded terminal too; see switch.go)
	{id: "sessions.prev", group: "Switch", desc: "previous session in the list (typing focus follows)", keys: []string{"alt+up"}},
	{id: "sessions.next", group: "Switch", desc: "next session in the list (typing focus follows)", keys: []string{"alt+down"}},
	{id: "sessions.last", group: "Switch", desc: "back to the previously viewed session (toggles)", keys: []string{"alt+left"}},
	{id: "sessions.attention", group: "Switch", desc: "next session that needs you (cycles)", keys: []string{"alt+right"}},
	// embedded terminal (checked before the dispatcher while the pane has focus)
	{id: "term.unfocus", group: "Terminal", desc: "leave the terminal: focus back to the list (the view stays live)", keys: []string{"ctrl+]"}},
	// digest ("While you were away")
	{id: "digest.open", group: "Digest", desc: "open the line's session or transcript moment", keys: []string{"enter"}},
	{id: "digest.transcript", group: "Digest", desc: "open the transcript at that moment", keys: []string{"r"}},
	// projects
	{id: "projects.open", group: "Projects", desc: "open project home", keys: []string{"enter"}},
	{id: "projects.new", group: "Projects", desc: "new session: a repo, or a path like ~/scratch", keys: []string{"n"}},
	{id: "project.next_section", group: "Project", desc: "next section", keys: []string{"right", "]"}},
	{id: "project.prev_section", group: "Project", desc: "previous section", keys: []string{"left", "["}},
	{id: "project.open", group: "Project", desc: "open / read the selected item", keys: []string{"enter"}},
	{id: "project.edit", group: "Project", desc: "edit the document in $EDITOR", keys: []string{"e"}},
	{id: "project.comment", group: "Project", desc: "post to the project thread", keys: []string{"c"}},
	{id: "project.refresh", group: "Project", desc: "refresh", keys: []string{"r"}},
	{id: "artifact.comment", group: "Review", desc: "comment on this exact version", keys: []string{"c"}},
	{id: "artifact.scroll_down", group: "Review", desc: "scroll content down", keys: []string{"J"}},
	{id: "artifact.scroll_up", group: "Review", desc: "scroll content up", keys: []string{"K"}},
	{id: "doc.revisions", group: "Document", desc: "toggle revisions", keys: []string{"v"}},
	{id: "doc.edit", group: "Document", desc: "edit in $EDITOR (conflicts preserved)", keys: []string{"e"}},
	{id: "messages.new", group: "Messages", desc: "new message", keys: []string{"m"}},
	{id: "messages.reconcile", group: "Messages", desc: "reconcile / reissue", keys: []string{"R"}},
	{id: "messages.cancel", group: "Messages", desc: "cancel a queued message", keys: []string{"c"}},
	{id: "manage.refresh", group: "Manage", desc: "refresh", keys: []string{"r"}},
	{id: "manage.rename", group: "Manage", desc: "rename the selected account (Accounts)", keys: []string{"R"}},
	{id: "manage.login", group: "Manage", desc: "log a machine into Codex with a device code (Accounts)", keys: []string{"L"}},
	{id: "manage.activity", group: "Manage", desc: "activity log (from Accounts: account entries)", keys: []string{"a"}},
	{id: "manage.filter", group: "Manage", desc: "cycle the Activity filter: all · accounts · sessions · fleet", keys: []string{"f"}},
	{id: "reader.search", group: "Reader", desc: "search the transcript", keys: []string{"/"}},
	// composer and text inputs (handled by the inputs; listed for help)
	{id: "composer.send", group: "Composer", desc: "send", keys: []string{"enter", "ctrl+s"}, special: true},
	{id: "composer.newline", group: "Composer", desc: "new line", keys: []string{"shift+enter", "alt+enter", "ctrl+j"}, special: true},
	{id: "composer.history", group: "Composer", desc: "reverse-search sent messages", keys: []string{"ctrl+r"}, special: true},
	{id: "composer.mention", group: "Composer", desc: "mention a session or project", keys: []string{"@"}, special: true},
	{id: "composer.leave", group: "Composer", desc: "leave without sending (draft kept)", keys: []string{"esc", "j k"}, special: true},
	{id: "picker.move", group: "Pickers", desc: "move in finder / completion lists", keys: []string{"ctrl+j", "ctrl+k", "up", "down"}, special: true},
	{id: "picker.attach", group: "Pickers", desc: "attach the session under the cursor (finder)", keys: []string{"ctrl+a"}, special: true},
	{id: "cmdline.complete", group: "Pickers", desc: "complete command (cmdline)", keys: []string{"tab"}, special: true},
	{id: "cmdline.history", group: "Pickers", desc: "command history", keys: []string{"ctrl+p", "ctrl+n"}, special: true},
}

func actionByID(id string) (actionDef, bool) {
	for _, a := range actionDefs {
		if a.id == id {
			return a, true
		}
	}
	return actionDef{}, false
}

// DefaultKeymap returns the built-in key configuration.
func DefaultKeymap() *Keymap {
	km := &Keymap{Leader: "space", JKEscape: true, WhichKey: 300 * time.Millisecond, SeqTimeout: time.Second,
		JKTimeout: 300 * time.Millisecond, OptionArrows: "list", bind: map[string][][]string{}}
	for _, a := range actionDefs {
		if a.special {
			continue
		}
		for _, k := range a.keys {
			if seq, err := parseKeySeq(k); err == nil {
				km.bind[a.id] = append(km.bind[a.id], seq)
			}
		}
	}
	return km
}

// DefaultKeysPath is $XDG_CONFIG_HOME/pyre/keys.toml or
// ~/.config/pyre/keys.toml.
func DefaultKeysPath() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "pyre", "keys.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "pyre", "keys.toml")
}

// LoadKeymap applies the overrides in path (missing file = defaults). The
// file is never created. Problems are collected in Warnings; a syntax
// error returns the defaults and the error.
func LoadKeymap(path string) (*Keymap, error) {
	km := DefaultKeymap()
	if path == "" || path == "none" || path == "-" || path == os.DevNull {
		return km, nil
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return km, nil
	}
	if err != nil {
		return km, err
	}
	defer f.Close()
	if err := km.apply(f); err != nil {
		return DefaultKeymap(), fmt.Errorf("%s: %w", path, err)
	}
	km.Source = path
	return km, nil
}

// KeysFileFormat documents keys.toml.
const KeysFileFormat = `# ~/.config/pyre/keys.toml — Pyre key overrides (optional; never created
# automatically). Only the settings and actions you list change.
leader = "space"              # key that starts <leader> sequences
jk_escape = true              # "jk" typed quickly leaves text inputs (like Esc)
which_key_delay_ms = 300      # delay before the leader popup appears
sequence_timeout_ms = 1000    # how long g… waits for its second key
macos_option_arrows = "list"  # ESC b / ESC f (Terminal.app option+←/→) switch
                              # sessions like alt+←/→: "list" (not inside the
                              # embedded terminal), "always" or "off"

[keys]
# action = "key" or ["key", "key"]; the list REPLACES that action's
# defaults. Keys: j, G, enter, esc, tab, shift+tab, ctrl+d, alt+enter,
# space, up, down, pgdown, …; sequences: "g g", "<leader>s",
# "<leader><leader>". Vim notation also works: <c-d>, <s-tab>, <cr>.
# An empty list [] unbinds the action. Run "pyre keys" (or press ?) to
# see every action and its effective keys.
"sessions.attach" = ["enter", "o"]
"term.unfocus" = "ctrl+]"
"global.finder" = ["<leader><leader>", "ctrl+p"]
`

func (km *Keymap) apply(r io.Reader) error {
	sc := bufio.NewScanner(r)
	section := ""
	lineNo := 0
	pending := ""
	for sc.Scan() {
		lineNo++
		line := stripComment(sc.Text())
		if strings.TrimSpace(line) == "" && pending == "" {
			continue
		}
		if pending != "" {
			pending += " " + strings.TrimSpace(line)
			if !strings.Contains(line, "]") {
				continue
			}
			line, pending = pending, ""
		}
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") && !strings.Contains(t, "=") {
			section = strings.Trim(t, "[] ")
			if section != "keys" && section != "settings" {
				km.Warnings = append(km.Warnings, fmt.Sprintf("line %d: unknown section [%s]", lineNo, section))
			}
			continue
		}
		eq := strings.IndexByte(t, '=')
		if eq < 0 {
			return fmt.Errorf("line %d: expected key = value", lineNo)
		}
		key := unquote(strings.TrimSpace(t[:eq]))
		val := strings.TrimSpace(t[eq+1:])
		if strings.HasPrefix(val, "[") && !strings.Contains(val, "]") {
			pending = t
			continue
		}
		if section == "keys" {
			if err := km.setKeys(key, val, lineNo); err != nil {
				return err
			}
			continue
		}
		if err := km.setSetting(key, val, lineNo); err != nil {
			return err
		}
	}
	if pending != "" {
		return fmt.Errorf("unterminated array")
	}
	return sc.Err()
}

func (km *Keymap) setSetting(key, val string, line int) error {
	switch key {
	case "leader":
		s, err := tomlString(val)
		if err != nil {
			return fmt.Errorf("line %d: %v", line, err)
		}
		seq, err := parseKeySeq(s)
		if err != nil || len(seq) != 1 {
			return fmt.Errorf("line %d: leader must be a single key", line)
		}
		km.Leader = seq[0]
	case "jk_escape":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("line %d: jk_escape must be true or false", line)
		}
		km.JKEscape = b
	case "macos_option_arrows":
		v, err := tomlString(val)
		if err != nil || (v != "list" && v != "always" && v != "off") {
			return fmt.Errorf(`line %d: macos_option_arrows must be "list", "always" or "off"`, line)
		}
		km.OptionArrows = v
	case "which_key_delay_ms", "sequence_timeout_ms", "jk_timeout_ms":
		n, err := strconv.Atoi(val)
		if err != nil || n < 0 {
			return fmt.Errorf("line %d: %s must be a number of milliseconds", line, key)
		}
		d := time.Duration(n) * time.Millisecond
		switch key {
		case "which_key_delay_ms":
			km.WhichKey = d
		case "sequence_timeout_ms":
			km.SeqTimeout = d
		default:
			km.JKTimeout = d
		}
	default:
		km.Warnings = append(km.Warnings, fmt.Sprintf("line %d: unknown setting %q", line, key))
	}
	return nil
}

func (km *Keymap) setKeys(action, val string, line int) error {
	a, ok := actionByID(action)
	if !ok || a.special {
		km.Warnings = append(km.Warnings, fmt.Sprintf("line %d: unknown or fixed action %q (see pyre keys)", line, action))
		return nil
	}
	var keys []string
	if strings.HasPrefix(val, "[") {
		inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(val, "["), "]"))
		for _, part := range splitTOMLArray(inner) {
			s, err := tomlString(part)
			if err != nil {
				return fmt.Errorf("line %d: %v", line, err)
			}
			keys = append(keys, s)
		}
	} else {
		s, err := tomlString(val)
		if err != nil {
			return fmt.Errorf("line %d: %v", line, err)
		}
		keys = []string{s}
	}
	var seqs [][]string
	for _, k := range keys {
		seq, err := parseKeySeq(k)
		if err != nil {
			km.Warnings = append(km.Warnings, fmt.Sprintf("line %d: %s: %v", line, action, err))
			continue
		}
		seqs = append(seqs, seq)
	}
	km.bind[action] = seqs
	return nil
}

func stripComment(s string) string {
	inStr := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr != 0:
			if c == '\\' && inStr == '"' {
				i++
			} else if c == inStr {
				inStr = 0
			}
		case c == '"' || c == '\'':
			inStr = c
		case c == '#':
			return s[:i]
		}
	}
	return s
}

func splitTOMLArray(s string) []string {
	var out []string
	inStr := byte(0)
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr != 0:
			if c == '\\' && inStr == '"' {
				i++
			} else if c == inStr {
				inStr = 0
			}
		case c == '"' || c == '\'':
			inStr = c
		case c == ',':
			if p := strings.TrimSpace(s[start:i]); p != "" {
				out = append(out, p)
			}
			start = i + 1
		}
	}
	if p := strings.TrimSpace(s[start:]); p != "" {
		out = append(out, p)
	}
	return out
}

func unquote(s string) string {
	if v, err := tomlString(s); err == nil {
		return v
	}
	return s
}

func tomlString(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'':
		return s[1 : len(s)-1], nil
	case len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"':
		v, err := strconv.Unquote(s)
		if err != nil {
			return "", fmt.Errorf("bad string %s", s)
		}
		return v, nil
	}
	return "", fmt.Errorf("expected a quoted string, got %s", s)
}

// namedKeys are multi-character key names (Bubble Tea v2 spelling).
var namedKeys = map[string]bool{
	"enter": true, "esc": true, "tab": true, "space": true, "backspace": true, "delete": true,
	"up": true, "down": true, "left": true, "right": true, "home": true, "end": true,
	"pgup": true, "pgdown": true, "insert": true,
	"f1": true, "f2": true, "f3": true, "f4": true, "f5": true, "f6": true, "f7": true, "f8": true, "f9": true, "f10": true, "f11": true, "f12": true,
}

var vimNames = map[string]string{
	"cr": "enter", "enter": "enter", "return": "enter", "esc": "esc", "tab": "tab", "space": "space",
	"bs": "backspace", "up": "up", "down": "down", "left": "left", "right": "right",
	"home": "home", "end": "end", "pageup": "pgup", "pagedown": "pgdown", "leader": "<leader>",
}

// parseKeySeq parses a binding into key tokens; "<leader>" stays symbolic.
func parseKeySeq(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty key")
	}
	var out []string
	for _, tok := range strings.Fields(s) {
		for tok != "" {
			if tok[0] == '<' {
				end := strings.IndexByte(tok, '>')
				if end < 0 {
					return nil, fmt.Errorf("unclosed < in %q", s)
				}
				k, err := vimKey(tok[1:end])
				if err != nil {
					return nil, err
				}
				out = append(out, k)
				tok = tok[end+1:]
				continue
			}
			if namedKeys[tok] || (strings.Contains(tok, "+") && len(tok) > 2) {
				if err := validKey(tok); err != nil {
					return nil, err
				}
				out = append(out, tok)
				break
			}
			// characters, one key each ("gg" = g g)
			r := []rune(tok)
			out = append(out, string(r[0]))
			tok = string(r[1:])
		}
	}
	return out, nil
}

func validKey(k string) error {
	parts := strings.Split(k, "+")
	base := parts[len(parts)-1]
	for _, p := range parts[:len(parts)-1] {
		switch p {
		case "ctrl", "alt", "shift", "super", "meta", "hyper":
		default:
			return fmt.Errorf("unknown modifier %q in %q", p, k)
		}
	}
	if !namedKeys[base] && len([]rune(base)) != 1 {
		return fmt.Errorf("unknown key %q", k)
	}
	return nil
}

func vimKey(inner string) (string, error) {
	l := strings.ToLower(inner)
	if v, ok := vimNames[l]; ok {
		return v, nil
	}
	if len(l) > 2 && l[1] == '-' {
		mod := map[byte]string{'c': "ctrl+", 'a': "alt+", 'm': "alt+", 's': "shift+"}[l[0]]
		if mod == "" {
			return "", fmt.Errorf("unknown key <%s>", inner)
		}
		rest := inner[2:]
		if v, ok := vimNames[strings.ToLower(rest)]; ok {
			rest = v
		} else if len([]rune(rest)) == 1 {
			rest = strings.ToLower(rest)
		}
		return mod + rest, nil
	}
	return "", fmt.Errorf("unknown key <%s>", inner)
}

// expand resolves the symbolic leader.
func (km *Keymap) expand(seq []string) []string {
	out := make([]string, len(seq))
	for i, k := range seq {
		if k == "<leader>" {
			k = km.Leader
		}
		out[i] = k
	}
	return out
}

// lookup finds the binding for the pending keys in the given scopes (in
// priority order). exact is the action bound to exactly these keys; more
// reports that longer bindings start with them.
func (km *Keymap) lookup(scopes []string, keys []string) (exact string, more bool) {
	for _, sc := range scopes {
		for _, a := range actionDefs {
			if a.special || a.scope() != sc {
				continue
			}
			for _, seq := range km.bind[a.id] {
				s := km.expand(seq)
				if len(s) < len(keys) || !equalPrefix(s, keys) {
					continue
				}
				if len(s) == len(keys) {
					if exact == "" {
						exact = a.id
					}
				} else {
					more = true
				}
			}
		}
	}
	return exact, more
}

func equalPrefix(seq, keys []string) bool {
	for i := range keys {
		if seq[i] != keys[i] {
			return false
		}
	}
	return true
}

// keysFor renders an action's effective bindings ("j / down").
func (km *Keymap) keysFor(id string) string {
	if a, ok := actionByID(id); ok && a.special {
		return strings.Join(a.keys, " / ")
	}
	var out []string
	for _, seq := range km.bind[id] {
		out = append(out, displaySeq(seq))
	}
	return strings.Join(out, " / ")
}

func displaySeq(seq []string) string {
	var b strings.Builder
	for i, k := range seq {
		if k == "<leader>" {
			k = "␣"
		} else if k == "space" {
			k = "space"
		}
		if i > 0 && !(seq[i-1] == "<leader>") {
			b.WriteString(" ")
		}
		b.WriteString(k)
	}
	return b.String()
}

// whichKeyEntry is one line of the leader popup.
type whichKeyEntry struct {
	key, desc string
	group     bool
}

// next lists the keys that can follow the pending prefix.
func (km *Keymap) next(scopes []string, keys []string) []whichKeyEntry {
	seen := map[string]bool{}
	var out []whichKeyEntry
	for _, sc := range scopes {
		for _, a := range actionDefs {
			if a.special || a.scope() != sc {
				continue
			}
			for _, seq := range km.bind[a.id] {
				s := km.expand(seq)
				if len(s) <= len(keys) || !equalPrefix(s, keys) {
					continue
				}
				k := s[len(keys)]
				if seen[k] {
					continue
				}
				seen[k] = true
				e := whichKeyEntry{key: k, desc: a.desc}
				if len(s) > len(keys)+1 {
					e.desc, e.group = "+more", true
				}
				out = append(out, e)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].key) < strings.ToLower(out[j].key) })
	return out
}

// Reference renders every action with its effective keys, grouped, for
// `pyre keys` and the help overlay.
func (km *Keymap) Reference() string {
	var b strings.Builder
	if km.Source != "" {
		fmt.Fprintf(&b, "# overrides from %s\n", km.Source)
	}
	fmt.Fprintf(&b, "leader = %q   jk_escape = %v   which_key_delay_ms = %d\n", km.Leader, km.JKEscape, km.WhichKey.Milliseconds())
	group := ""
	defs := append([]actionDef(nil), actionDefs...)
	rank := map[string]int{}
	for i, g := range helpGroupOrder {
		rank[g] = i
	}
	sort.SliceStable(defs, func(i, j int) bool { return rank[defs[i].group] < rank[defs[j].group] })
	for _, a := range defs {
		if a.group != group {
			group = a.group
			fmt.Fprintf(&b, "\n%s\n", group)
		}
		keys := km.keysFor(a.id)
		if keys == "" {
			keys = "(unbound)"
		}
		fmt.Fprintf(&b, "  %-22s %-28s %s\n", keys, a.id, a.desc)
	}
	for _, w := range km.Warnings {
		fmt.Fprintf(&b, "\nwarning: %s", w)
	}
	return b.String()
}

// KeymapReference loads path (DefaultKeysPath when empty) and renders the
// effective key map — the body of `pyre keys`.
func KeymapReference(path string) (string, error) {
	if path == "" {
		path = DefaultKeysPath()
	}
	km, err := LoadKeymap(path)
	return km.Reference(), err
}
