package claude

// A fake `claude` used by the unit tests: the test binary re-executes itself
// with PYRE_TEST_ROLE=fakeclaude inside the isolated tmux server. It renders
// an Ink-like screen (input box between ─ rules, dim placeholder, dialogs),
// reads raw keys including bracketed paste, writes a transcript in Claude's
// format and runs the hook commands from --settings exactly like Claude
// (sh -c, JSON on stdin). Prompts containing FAKE_* directives drive
// scenarios. PYRE_TEST_ROLE=hook runs HookMain.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	switch os.Getenv("PYRE_TEST_ROLE") {
	case "hook":
		os.Exit(HookMain(os.Args[1:], os.Stdin, os.Stdout))
	case "fakeclaude":
		os.Exit(fakeClaudeMain(os.Args[1:]))
	case "reallaunch":
		os.Exit(realLaunchMain())
	case "pyremcp":
		os.Exit(pyreMCPMain())
	case "inboxwatch":
		os.Exit(inboxWatchMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

type fakeClaude struct {
	mu         sync.Mutex
	hooks      map[string]string
	sid        string
	cwd        string
	transcript string
	input      []byte
	output     []string
	busy       bool
	queue      []string
	dialog     string // "", "trust", "perm", "ask"
	trustYes   bool
	dialogCh   chan bool
	interrupt  chan struct{}
	toolSeq    int
	pasting    bool
}

func fakeClaudeMain(args []string) int {
	f := &fakeClaude{hooks: map[string]string{}}
	if p := os.Getenv("FAKE_ARGV_FILE"); p != "" {
		_ = os.WriteFile(p, []byte(strings.Join(args, "\n")), 0o600)
	}
	var settings, prompt, resume, pluginDir string
	for i := 0; i < len(args); i++ {
		if d, ok := strings.CutPrefix(args[i], "--plugin-dir="); ok {
			pluginDir = d
			continue
		}
		switch args[i] {
		case "--settings":
			i++
			settings = args[i]
		case "--session-id":
			i++
			f.sid = args[i]
		case "--resume":
			i++
			resume = args[i]
			f.sid = resume
		case "--permission-mode", "--append-system-prompt", "--model":
			i++
		default:
			if !strings.HasPrefix(args[i], "-") {
				prompt = args[i]
			}
		}
	}
	var s struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string } `json:"hooks"`
		} `json:"hooks"`
	}
	if b, err := os.ReadFile(settings); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	for ev, ms := range s.Hooks {
		if len(ms) > 0 && len(ms[0].Hooks) > 0 {
			f.hooks[ev] = ms[0].Hooks[0].Command
		}
	}
	f.cwd, _ = os.Getwd()
	if r, err := filepath.EvalSymlinks(f.cwd); err == nil {
		f.cwd = r
	}
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	if cfg == "" {
		home, _ := os.UserHomeDir()
		cfg = filepath.Join(home, ".claude")
	}
	f.transcript = filepath.Join(cfg, "projects", projectSlug(f.cwd), f.sid+".jsonl")
	if _, err := os.Stat(f.transcript); resume != "" && err != nil {
		// Like Claude: --resume reads the conversation from this config dir.
		os.Stdout.WriteString("No conversation found with session ID: " + resume + "\r\n")
		return 1
	}
	_ = os.MkdirAll(filepath.Dir(f.transcript), 0o700)

	stty := exec.Command("stty", "raw", "-echo")
	stty.Stdin = os.Stdin
	_ = stty.Run()
	os.Stdout.WriteString("\x1b[?2004h") // enable bracketed paste like Ink does

	if os.Getenv("FAKE_TRUST_DIALOG") == "1" {
		f.dialog = "trust"
	} else {
		f.start(resume != "")
	}
	f.render()
	if pluginDir != "" && os.Getenv("FAKE_PLUGIN_HOST") == "1" {
		go f.pluginHost(pluginDir)
	}
	if prompt != "" && f.dialog == "" {
		f.submit(prompt)
	}
	f.readLoop()
	return 0
}

func (f *fakeClaude) start(resumed bool) {
	src := "startup"
	if resumed {
		src = "resume"
	}
	f.hook("SessionStart", map[string]any{"source": src})
}

func (f *fakeClaude) base(ev string) map[string]any {
	return map[string]any{"hook_event_name": ev, "session_id": f.sid, "transcript_path": f.transcript, "cwd": f.cwd, "permission_mode": "default"}
}

func (f *fakeClaude) hook(ev string, extra map[string]any) []byte {
	cmd := f.hooks[ev]
	if cmd == "" {
		return nil
	}
	p := f.base(ev)
	for k, v := range extra {
		p[k] = v
	}
	b, _ := json.Marshal(p)
	c := exec.Command("/bin/sh", "-c", cmd)
	c.Stdin = strings.NewReader(string(b))
	out, _ := c.Output()
	return out
}

// hookAsync starts a hook and returns its process (for the PermissionRequest race).
func (f *fakeClaude) hookAsync(ev string, extra map[string]any) (*exec.Cmd, *strings.Builder) {
	p := f.base(ev)
	for k, v := range extra {
		p[k] = v
	}
	b, _ := json.Marshal(p)
	c := exec.Command("/bin/sh", "-c", "exec "+f.hooks[ev])
	c.Stdin = strings.NewReader(string(b))
	var out strings.Builder
	c.Stdout = &out
	_ = c.Start()
	return c, &out
}

func (f *fakeClaude) tx(rec map[string]any) {
	rec["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	rec["sessionId"] = f.sid
	b, _ := json.Marshal(rec)
	fh, err := os.OpenFile(f.transcript, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	fh.Write(append(b, '\n'))
	fh.Close()
}

func (f *fakeClaude) say(line string) {
	f.mu.Lock()
	f.output = append(f.output, line)
	if len(f.output) > 20 {
		f.output = f.output[len(f.output)-20:]
	}
	f.mu.Unlock()
	f.render()
}

func (f *fakeClaude) render() {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	b.WriteString(" FAKE Claude Code v0.0.0-test\r\n\r\n")
	for _, l := range f.output {
		b.WriteString(l + "\r\n")
	}
	rule := strings.Repeat("─", 60)
	rows := 0
	if n, err := strconv.Atoi(os.Getenv("FAKE_DRAFT_ROWS")); err == nil && n > 0 {
		// Like Claude Code: the rule spans the pane, a long draft wraps at
		// the box width and only its last rows are shown.
		rows = n
		rule = strings.Repeat("─", termWidth())
	}
	switch f.dialog {
	case "trust":
		b.WriteString(rule + "\r\n Accessing workspace:\r\n " + f.cwd + "\r\n")
		b.WriteString(" Quick safety check: Is this a project you created or one you trust?\r\n")
		if f.trustYes {
			b.WriteString("   No, exit\r\n ❯ Yes, I trust this folder\r\n")
		} else {
			b.WriteString(" ❯ No, exit\r\n   Yes, I trust this folder\r\n")
		}
		b.WriteString(" Enter to confirm · Esc to cancel\r\n")
	case "perm":
		b.WriteString(rule + "\r\n Bash command\r\n   ls\r\n Do you want to proceed?\r\n ❯ 1. Yes\r\n   2. No\r\n Esc to cancel\r\n")
	case "exitbg":
		b.WriteString(rule + "\r\n Background work is running\r\n The following will stop when you exit:\r\n\r\n monitor · New Pyre messages for this session\r\n\r\n")
		b.WriteString(" ❯ 1. Exit and stop tasks\r\n   2. Move to background and exit\r\n   3. Stay\r\n\r\n Enter to confirm · Esc to cancel\r\n")
	case "ask":
		b.WriteString(rule + "\r\n Which option?\r\n ❯ 1. Red\r\n   2. Blue\r\n Enter to select · Esc to cancel\r\n")
	default:
		b.WriteString(rule + "\r\n")
		if len(f.input) == 0 {
			b.WriteString("❯  \x1b[2mTry \"fake things\"\x1b[0m\r\n")
		} else if rows > 0 {
			vis := wrapDraft(string(f.input), termWidth()-4)
			if len(vis) > rows {
				vis = vis[len(vis)-rows:]
			}
			for i, l := range vis {
				if i == 0 {
					b.WriteString("❯  " + l + "\r\n")
				} else {
					b.WriteString("   " + l + "\r\n")
				}
			}
		} else {
			for i, l := range strings.Split(string(f.input), "\n") {
				if i == 0 {
					b.WriteString("❯  " + l + "\r\n")
				} else {
					b.WriteString("  " + l + "\r\n")
				}
			}
		}
		b.WriteString(rule + "\r\n")
		status := "  ? for shortcuts"
		if f.busy {
			status = "  ✻ Working… (esc to interrupt)"
		}
		b.WriteString(status)
	}
	os.Stdout.WriteString(b.String())
}

// wrapDraft wraps text at width runes per row (hard wrap; newlines kept).
func wrapDraft(text string, width int) []string {
	if width < 10 {
		width = 10
	}
	var out []string
	for _, l := range strings.Split(text, "\n") {
		r := []rune(l)
		for len(r) > width {
			out = append(out, string(r[:width]))
			r = r[width:]
		}
		out = append(out, string(r))
	}
	return out
}

func (f *fakeClaude) readLoop() {
	winch := make(chan os.Signal, 1)
	notifyResize(winch)
	go func() {
		for range winch {
			f.render()
		}
	}()
	ch := make(chan byte, 4096)
	go func() {
		r := bufio.NewReader(os.Stdin)
		for {
			c, err := r.ReadByte()
			if err != nil {
				close(ch)
				return
			}
			ch <- c
		}
	}()
	next := func(d time.Duration) (byte, bool) {
		select {
		case c, ok := <-ch:
			return c, ok
		case <-time.After(d):
			return 0, false
		}
	}
	for {
		c, ok := <-ch
		if !ok {
			return
		}
		switch c {
		case 0x1b:
			c2, ok := next(40 * time.Millisecond)
			if !ok {
				f.key("esc")
				continue
			}
			if c2 != '[' {
				continue
			}
			var seq []byte
			for {
				c3, ok := next(200 * time.Millisecond)
				if !ok {
					break
				}
				seq = append(seq, c3)
				if c3 >= 0x40 && c3 <= 0x7e {
					break
				}
			}
			switch string(seq) {
			case "200~":
				f.pasting = true
			case "201~":
				f.pasting = false
				// FAKE_PASTE_SUFFIX: text that lands in the box right after
				// Pyre's paste (someone else typing).
				if sfx := os.Getenv("FAKE_PASTE_SUFFIX"); sfx != "" {
					f.edit(func() { f.input = append(f.input, sfx...) })
				} else {
					f.render()
				}
			case "B":
				f.key("down")
			case "A":
				f.key("up")
			}
		case '\r', '\n':
			if f.pasting {
				f.edit(func() { f.input = append(f.input, '\n') })
			} else {
				f.key("enter")
			}
		case 0x15: // C-u: delete the current line
			f.edit(func() {
				i := strings.LastIndexByte(string(f.input), '\n')
				f.input = f.input[:i+1]
				if i < 0 {
					f.input = f.input[:0]
				}
			})
		case 0x7f, 0x08:
			f.edit(func() {
				if len(f.input) > 0 {
					f.input = f.input[:len(f.input)-1]
				}
			})
		default:
			if c >= 0x20 || c >= 0x80 {
				f.edit(func() { f.input = append(f.input, c) })
			}
		}
	}
}

// edit applies fn to the draft and redraws. Inside a bracketed paste the
// draft is drawn once, when the paste ends, as Claude Code does: drawing
// every pasted byte would show transient drafts (the paste's tail before
// text that follows it) that a slow machine lets a screen check observe.
func (f *fakeClaude) edit(fn func()) {
	f.mu.Lock()
	if f.dialog == "" {
		fn()
	}
	f.mu.Unlock()
	if !f.pasting {
		f.render()
	}
}

func (f *fakeClaude) key(k string) {
	f.mu.Lock()
	dialog := f.dialog
	f.mu.Unlock()
	switch dialog {
	case "trust":
		switch k {
		case "down", "up":
			f.mu.Lock()
			f.trustYes = !f.trustYes
			f.mu.Unlock()
			f.render()
		case "enter":
			f.mu.Lock()
			yes := f.trustYes
			f.dialog = ""
			f.mu.Unlock()
			if !yes {
				os.Exit(1)
			}
			f.start(false)
			f.render()
		}
		return
	case "exitbg":
		switch k {
		case "enter":
			f.hook("SessionEnd", map[string]any{"reason": "prompt_input_exit"})
			os.Exit(0)
		case "esc":
			f.mu.Lock()
			f.dialog = ""
			f.mu.Unlock()
			f.render()
		}
		return
	case "perm", "ask":
		switch k {
		case "enter":
			f.dialogCh <- true
		case "esc":
			f.dialogCh <- false
		}
		return
	}
	switch k {
	case "enter":
		f.mu.Lock()
		text := string(f.input)
		f.input = nil
		f.mu.Unlock()
		f.render()
		if strings.TrimSpace(text) != "" {
			f.submit(text)
		}
	case "esc":
		f.mu.Lock()
		ic := f.interrupt
		f.mu.Unlock()
		if ic != nil {
			select {
			case ic <- struct{}{}:
			default:
			}
		}
	}
}

func (f *fakeClaude) submit(text string) {
	if strings.TrimSpace(text) == "/exit" {
		// FAKE_BG_ON_EXIT: like Claude with a background task (Pyre's
		// message monitor) running, /exit first asks what to do with it.
		if os.Getenv("FAKE_BG_ON_EXIT") == "1" {
			f.mu.Lock()
			f.dialog = "exitbg"
			f.mu.Unlock()
			f.render()
			return
		}
		f.hook("SessionEnd", map[string]any{"reason": "prompt_input_exit"})
		os.Exit(0)
	}
	f.mu.Lock()
	busy := f.busy
	if busy {
		f.queue = append(f.queue, text)
	} else {
		f.busy = true
		f.interrupt = make(chan struct{}, 1)
	}
	f.mu.Unlock()
	f.hook("UserPromptSubmit", map[string]any{"prompt": text, "prompt_id": strconv.FormatInt(time.Now().UnixNano(), 36)})
	if busy {
		f.tx(map[string]any{"type": "queue-operation", "operation": "enqueue", "content": text})
		return
	}
	f.userRecord(text, false)
	go f.turn(text)
}

func (f *fakeClaude) userRecord(text string, meta bool) {
	rec := map[string]any{"type": "user", "uuid": fmt.Sprintf("u-%d", time.Now().UnixNano()),
		"message": map[string]any{"role": "user", "content": text}}
	if meta {
		rec["isMeta"] = true
	} else {
		rec["origin"] = map[string]any{"kind": "human"}
	}
	f.tx(rec)
	f.say("> " + strings.ReplaceAll(text, "\n", " "))
}

var (
	sleepRe = regexp.MustCompile(`FAKE_SLEEP_MS=(\d+)`)
	// FAKE_TOOL_MS: an approved FAKE_PERMISSION tool runs this long before
	// its PostToolUse (a long Bash command).
	toolMsRe = regexp.MustCompile(`FAKE_TOOL_MS=(\d+)`)
)

// wait sleeps d unless interrupted (returns false if interrupted).
func (f *fakeClaude) wait(d time.Duration) bool {
	f.mu.Lock()
	ic := f.interrupt
	f.mu.Unlock()
	select {
	case <-time.After(d):
		return true
	case <-ic:
		return false
	}
}

func (f *fakeClaude) endTurn(interrupted bool) {
	f.tx(map[string]any{"type": "system", "subtype": "turn_duration", "durationMs": 10})
	if interrupted {
		f.say("⎿ Interrupted by user")
	}
	f.mu.Lock()
	var next string
	if len(f.queue) > 0 {
		next = f.queue[0]
		f.queue = f.queue[1:]
		f.interrupt = make(chan struct{}, 1)
	} else {
		f.busy = false
	}
	f.mu.Unlock()
	f.render()
	if next != "" {
		f.tx(map[string]any{"type": "queue-operation", "operation": "dequeue"})
		f.userRecord(next, false)
		go f.turn(next)
	}
}

func (f *fakeClaude) toolID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.toolSeq++
	return fmt.Sprintf("toolu_fake%d", f.toolSeq)
}

func (f *fakeClaude) turn(text string) {
	if m := sleepRe.FindStringSubmatch(text); m != nil {
		n, _ := strconv.Atoi(m[1])
		if !f.wait(time.Duration(n) * time.Millisecond) {
			f.endTurn(true)
			return
		}
	}
	if strings.Contains(text, "FAKE_PERMISSION") {
		var toolTime time.Duration
		if m := toolMsRe.FindStringSubmatch(text); m != nil {
			n, _ := strconv.Atoi(m[1])
			toolTime = time.Duration(n) * time.Millisecond
		}
		if !f.permission(toolTime) {
			f.endTurn(true)
			return
		}
	}
	if strings.Contains(text, "FAKE_ASK") {
		id := f.toolID()
		f.hook("PreToolUse", map[string]any{"tool_name": "AskUserQuestion", "tool_use_id": id, "tool_input": map[string]any{"question": "Which option?"}})
		f.mu.Lock()
		f.dialog, f.dialogCh = "ask", make(chan bool)
		f.mu.Unlock()
		f.render()
		ok := <-f.dialogCh
		f.mu.Lock()
		f.dialog = ""
		f.mu.Unlock()
		if !ok {
			f.endTurn(true)
			return
		}
		f.hook("PostToolUse", map[string]any{"tool_name": "AskUserQuestion", "tool_use_id": id})
	}
	if strings.Contains(text, "FAKE_SUBAGENT") {
		id := f.toolID()
		f.hook("PreToolUse", map[string]any{"tool_name": "Agent", "tool_use_id": id, "tool_input": map[string]any{"subagent_type": "general-purpose", "prompt": "x"}})
		f.hook("SubagentStart", map[string]any{"agent_id": "agent_fake1", "agent_type": "general-purpose"})
		f.wait(100 * time.Millisecond)
		f.hook("SubagentStop", map[string]any{"agent_id": "agent_fake1", "agent_type": "general-purpose", "agent_transcript_path": "/dev/null"})
		f.hook("PostToolUse", map[string]any{"tool_name": "Agent", "tool_use_id": id})
	}
	if strings.Contains(text, "FAKE_LIMIT") {
		f.tx(map[string]any{"type": "assistant", "isApiErrorMessage": true, "message": map[string]any{"role": "assistant",
			"content": []any{map[string]any{"type": "text", "text": "Claude AI usage limit reached|1791200000"}}}})
		f.say("⎿ Claude AI usage limit reached|1791200000")
		f.hook("StopFailure", map[string]any{"error": "Claude AI usage limit reached|1791200000"})
		f.endTurn(false)
		return
	}
	f.reply("ok")
	bg := []any{}
	if strings.Contains(text, "FAKE_BG") {
		bg = []any{map[string]any{"id": "bg1"}}
	}
	out := f.hook("Stop", map[string]any{"last_assistant_message": "ok", "background_tasks": bg, "stop_hook_active": false})
	var dec struct{ Decision, Reason string }
	if json.Unmarshal(out, &dec) == nil && dec.Decision == "block" {
		f.userRecord(dec.Reason, true)
		f.say("⎿ Stop hook error: " + dec.Reason)
		f.reply("injected-ok")
		f.hook("Stop", map[string]any{"last_assistant_message": "injected-ok", "background_tasks": []any{}, "stop_hook_active": true})
	}
	f.endTurn(false)
}

func (f *fakeClaude) reply(s string) {
	f.tx(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": s}}}})
	f.say("⏺ " + s)
}

// permission runs PreToolUse + PermissionRequest; the broker hook races the
// TUI dialog like Claude: the first decision wins; a TUI "No" kills the
// waiting hook. Returns false when the turn was interrupted (Esc/No).
func (f *fakeClaude) permission(toolTime time.Duration) bool {
	id := f.toolID()
	ti := map[string]any{"command": "ls", "description": "list"}
	f.hook("PreToolUse", map[string]any{"tool_name": "Bash", "tool_use_id": id, "tool_input": ti})
	cmd, out := f.hookAsync("PermissionRequest", map[string]any{"tool_name": "Bash", "tool_input": ti, "permission_suggestions": []any{}})
	hookDone := make(chan string, 1)
	go func() {
		_ = cmd.Wait()
		var o struct {
			HookSpecificOutput struct {
				Decision struct{ Behavior string } `json:"decision"`
			} `json:"hookSpecificOutput"`
		}
		_ = json.Unmarshal([]byte(out.String()), &o)
		hookDone <- o.HookSpecificOutput.Decision.Behavior
	}()
	f.mu.Lock()
	f.dialog, f.dialogCh = "perm", make(chan bool)
	f.mu.Unlock()
	f.render()
	allow, proceed := false, true
	select {
	case b := <-hookDone:
		switch b {
		case "allow":
			allow = true
		case "deny": // the model is told it was denied; the turn continues
		default: // broker timed out: the TUI dialog decides
			allow = <-f.dialogCh
			proceed = allow
		}
	case tui := <-f.dialogCh:
		allow, proceed = tui, tui
		if !tui && cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGKILL) // Claude kills the waiting hook on "No"
		}
	}
	f.mu.Lock()
	f.dialog = ""
	f.mu.Unlock()
	f.render()
	if allow && toolTime > 0 {
		f.say("● Running ls…")
		if !f.wait(toolTime) {
			return false
		}
	}
	if allow {
		f.hook("PostToolUse", map[string]any{"tool_name": "Bash", "tool_use_id": id, "tool_input": ti, "tool_response": map[string]any{"stdout": "file"}})
	}
	return proceed
}
