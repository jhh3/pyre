// Package doing derives the short "doing now" phrase shown for a working
// run ("running tests", "editing main.go", "searching code") from a
// harness's in-flight tool call or item.
//
// Phrases are display text, never evidence. They must not leak secrets:
// commands are reduced to their first meaningful segment with environment
// assignments and credential-looking words removed, file paths to their
// base name, and every phrase is clipped to MaxLen runes.
package doing

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxLen is the longest phrase, in runes.
const MaxLen = 40

// Common phrases.
const (
	Thinking       = "thinking"
	SearchingCode  = "searching code"
	RunningTests   = "running tests"
	Subagent       = "running a subagent"
	WaitingOnAgent = "waiting on subagent"
	Browsing       = "browsing the web"
	Compacting     = "compacting context"
	Planning       = "planning"
)

// Clip normalizes whitespace and truncates s to MaxLen runes.
func Clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= MaxLen {
		return s
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:MaxLen-1]), " ") + "…"
}

// Base returns the last element of a slash- or backslash-separated path.
func Base(p string) string {
	p = strings.TrimRight(strings.TrimSpace(p), `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		p = p[i+1:]
	}
	return p
}

// File is "<verb> <base name>", or "<verb> a file" without a path.
func File(verb, path string) string {
	if b := Base(path); b != "" {
		return Clip(verb + " " + b)
	}
	return verb + " a file"
}

// Files is File for one path and "<verb> N files" for several.
func Files(verb string, paths []string) string {
	switch len(paths) {
	case 0:
		return verb + " files"
	case 1:
		return File(verb, paths[0])
	}
	return verb + " " + strconv.Itoa(len(paths)) + " files"
}

var (
	// shellWrapRe matches "bash -lc '<cmd>'" style wrappers (Codex reports
	// commands that way).
	shellWrapRe = regexp.MustCompile(`^(?:\S*/)?(?:ba|z|da|k)?sh\s+(?:-[a-z]*c[a-z]*)\s+(.+)$`)
	envAssignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	testRe      = regexp.MustCompile(`(?i)(^|[\s/])(` +
		`go(\s+-C\s+\S+)?\s+test|gotestsum|pytest|py\.test|python[0-9.]*\s+-m\s+(pytest|unittest)|tox|nox|` +
		`(npm|pnpm|yarn|bun)\s+(run\s+)?test|npx\s+(jest|vitest|playwright)|jest|vitest|mocha|` +
		`cargo\s+(test|nextest)|make\s+(\S+\s+)*(test|check)\b|ctest|` +
		`(\./)?gradlew?\s+(\S+\s+)*(test|check|connected\w*Test)|mvn\s+(\S+\s+)*test|rspec|` +
		`rake\s+test|mix\s+test|deno\s+test|swift\s+test|xcodebuild\s+(\S+\s+)*test|phpunit|dotnet\s+test)(\s|$|:)`)
	secretWordRe = regexp.MustCompile(`(?i)(token|secret|passw|bearer|api[_-]?key|authorization|cookie|credential)`)
	longOpaqueRe = regexp.MustCompile(`^[A-Za-z0-9+/=_\-.:]{32,}$`)
)

// Command is the phrase for a shell command: RunningTests when it looks like
// a test runner, otherwise "running: <first segment, truncated>".
func Command(cmd string) string {
	cmd = unwrapShell(strings.TrimSpace(cmd))
	if cmd == "" {
		return "running a command"
	}
	if testRe.MatchString(cmd) {
		return RunningTests
	}
	head := commandHead(cmd)
	if head == "" {
		return "running a command"
	}
	return Clip("running: " + head)
}

// unwrapShell strips one "sh -c '<cmd>'" wrapper and its quotes.
func unwrapShell(cmd string) string {
	if m := shellWrapRe.FindStringSubmatch(cmd); m != nil {
		inner := strings.TrimSpace(m[1])
		if n := len(inner); n >= 2 && (inner[0] == '\'' || inner[0] == '"') && inner[n-1] == inner[0] {
			inner = inner[1 : n-1]
		}
		return strings.TrimSpace(inner)
	}
	return cmd
}

// commandHead returns the first segment of a command line that is not a cd,
// without environment assignments, stopping at a credential-looking word.
func commandHead(cmd string) string {
	segs := splitSegments(cmd)
	for _, seg := range segs {
		fields := strings.Fields(seg)
		for len(fields) > 0 && (envAssignRe.MatchString(fields[0]) || fields[0] == "env" || fields[0] == "sudo" || fields[0] == "time" || fields[0] == "exec") {
			fields = fields[1:]
		}
		if len(fields) == 0 || fields[0] == "cd" || fields[0] == "pushd" || fields[0] == "set" || fields[0] == "export" || fields[0] == "source" || fields[0] == "." {
			continue
		}
		fields[0] = Base(fields[0])
		var out []string
		for _, f := range fields {
			if secretWordRe.MatchString(f) || longOpaqueRe.MatchString(f) || envAssignRe.MatchString(f) {
				break
			}
			out = append(out, f)
			if len(out) >= 6 {
				break
			}
		}
		if len(out) == 0 {
			return ""
		}
		return strings.Join(out, " ")
	}
	return ""
}

// splitSegments splits on &&, ||, ;, | and newlines (quotes are not parsed:
// the result is display text only).
func splitSegments(cmd string) []string {
	r := strings.NewReplacer("&&", "\n", "||", "\n", ";", "\n", "|", "\n")
	return strings.Split(r.Replace(cmd), "\n")
}

// ClaudeTool maps a Claude Code tool call to its phrase.
func ClaudeTool(name string, input map[string]any) string {
	s := func(k string) string { v, _ := input[k].(string); return v }
	switch name {
	case "Bash":
		return Command(s("command"))
	case "BashOutput", "KillShell", "KillBash":
		return "checking a background command"
	case "Edit", "Write", "MultiEdit":
		return File("editing", s("file_path"))
	case "NotebookEdit":
		return File("editing", s("notebook_path"))
	case "Read", "NotebookRead":
		p := s("file_path")
		if p == "" {
			p = s("notebook_path")
		}
		return File("reading", p)
	case "Grep", "Glob", "LS", "ToolSearch":
		return SearchingCode
	case "Task", "Agent":
		return Subagent
	case "WebFetch", "WebSearch":
		return Browsing
	case "TodoWrite", "TodoRead", "TaskCreate", "TaskUpdate", "TaskList", "TaskGet":
		return Planning
	case "Skill":
		return "using a skill"
	case "AskUserQuestion", "ExitPlanMode":
		return "asking you"
	case "":
		return Thinking
	}
	if strings.HasPrefix(name, "mcp__") {
		return MCP(strings.SplitN(strings.TrimPrefix(name, "mcp__"), "__", 2)[0])
	}
	return Clip("using " + name)
}

// MCP is the phrase for an MCP tool call on server.
func MCP(server string) string {
	server = strings.TrimSpace(server)
	if server == "" {
		return "using a tool"
	}
	// claude.ai connectors appear as "claude_ai_<Name>".
	server = strings.TrimPrefix(server, "claude_ai_")
	server = strings.TrimPrefix(server, "plugin_")
	return Clip("using " + server)
}

// Plural returns "<verb> N files" when n of the in-flight calls share verb
// ("reading", "editing"), else "".
func Plural(phrase string, n int) string {
	if n < 2 {
		return ""
	}
	for _, verb := range []string{"reading", "editing"} {
		if strings.HasPrefix(phrase, verb+" ") {
			return verb + " " + strconv.Itoa(n) + " files"
		}
	}
	if phrase == Subagent {
		return "running " + strconv.Itoa(n) + " subagents"
	}
	return ""
}

// Verb returns the family of a phrase for grouping parallel calls
// ("reading", "editing", or the phrase itself).
func Verb(phrase string) string {
	for _, verb := range []string{"reading", "editing"} {
		if strings.HasPrefix(phrase, verb+" ") {
			return verb
		}
	}
	return phrase
}

// Inflight tracks a run's in-flight calls (Claude tool uses, Codex items)
// and summarizes them as one phrase. It is not safe for concurrent use.
type Inflight struct {
	seq   int
	calls map[string]call
}

type call struct {
	phrase string
	seq    int
}

// Start records an in-flight call. An empty id is tracked under a synthetic
// one so a call without an id still shows until the next Clear.
func (f *Inflight) Start(id, phrase string) {
	if f.calls == nil {
		f.calls = map[string]call{}
	}
	f.seq++
	if id == "" {
		id = "#" + strconv.Itoa(f.seq)
	}
	f.calls[id] = call{phrase: phrase, seq: f.seq}
}

// Done removes a finished call.
func (f *Inflight) Done(id string) { delete(f.calls, id) }

// Clear forgets every call (turn ended or a new prompt began).
func (f *Inflight) Clear() { f.calls = nil }

// Len is the number of in-flight calls.
func (f *Inflight) Len() int { return len(f.calls) }

// Phrase summarizes the in-flight calls: the newest call's phrase, or
// "<verb> N files" when several calls of that family run in parallel.
// Empty when nothing is in flight.
func (f *Inflight) Phrase() string {
	var latest call
	for _, c := range f.calls {
		if c.seq > latest.seq {
			latest = c
		}
	}
	if latest.seq == 0 {
		return ""
	}
	v, n := Verb(latest.phrase), 0
	for _, c := range f.calls {
		if Verb(c.phrase) == v {
			n++
		}
	}
	if p := Plural(latest.phrase, n); p != "" {
		return p
	}
	return latest.phrase
}
