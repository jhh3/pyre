//go:build !windows

package agy

// A fake `agy` for the unit tests: the test binary re-executes itself with
// PYRE_TEST_ROLE=fakeagy inside the isolated tmux server. It draws agy
// 1.4.2's prompt layout (input box between full-width rules, footer "? for
// shortcuts" / "esc to cancel"), reads raw keys including bracketed paste,
// loads hooks.json from the --add-dir folder's .agents/ and runs its
// handlers like agy (sh -c, camelCase JSON on stdin), and writes the
// conversation transcript under FAKE_AGY_HOME/brain/<id>/. Prompts holding
// FAKE_* words drive scenarios. PYRE_TEST_ROLE=hook runs HookMain.

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestMain(m *testing.M) {
	switch os.Getenv("PYRE_TEST_ROLE") {
	case "hook":
		os.Exit(HookMain(os.Args[1:], os.Stdin, os.Stdout))
	case "fakeagy":
		os.Exit(fakeAgyMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

type fakeAgy struct {
	mu       sync.Mutex
	hooks    map[string][]string
	home     string
	conv     string
	cwd      string
	ws       []string
	model    string
	output   []string
	input    []byte
	pasting  bool
	busy     bool
	queue    []string
	dialog   string // "", "trust", "perm"
	trustYes bool
	permCh   chan bool
	cancel   chan struct{}
	step     int
	started  bool
}

func fakeAgyMain(args []string) int {
	f := &fakeAgy{hooks: map[string][]string{}, home: os.Getenv("FAKE_AGY_HOME"), model: "Fake Model · medium"}
	if p := os.Getenv("FAKE_ARGV_FILE"); p != "" {
		_ = os.WriteFile(p, []byte(strings.Join(args, "\n")), 0o600)
	}
	prompt := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--add-dir":
			i++
			f.ws = append(f.ws, args[i])
		case "--conversation":
			i++
			f.conv = args[i]
		case "--prompt-interactive", "-i":
			i++
			prompt = args[i]
		case "--model":
			i++
			f.model = args[i] + " · medium"
		case "--effort":
			i++
		}
	}
	f.cwd, _ = os.Getwd()
	for _, w := range f.ws {
		var spec map[string]map[string]json.RawMessage
		b, err := os.ReadFile(filepath.Join(w, ".agents", "hooks.json"))
		if err != nil || json.Unmarshal(b, &spec) != nil {
			continue
		}
		for _, evs := range spec {
			for ev, raw := range evs {
				var flat []struct{ Command string }
				var grouped []struct {
					Hooks []struct{ Command string }
				}
				if json.Unmarshal(raw, &grouped) == nil && len(grouped) > 0 && len(grouped[0].Hooks) > 0 {
					for _, g := range grouped {
						for _, h := range g.Hooks {
							f.hooks[ev] = append(f.hooks[ev], h.Command)
						}
					}
				} else if json.Unmarshal(raw, &flat) == nil {
					for _, h := range flat {
						if h.Command != "" {
							f.hooks[ev] = append(f.hooks[ev], h.Command)
						}
					}
				}
			}
		}
	}
	if f.conv != "" {
		if _, err := os.Stat(filepath.Join(f.home, "conversations", f.conv+".db")); err != nil {
			os.Stdout.WriteString("Error: conversation " + f.conv + " not found\r\n")
			return 1
		}
		f.started = true
		f.output = append(f.output, "(resumed "+f.conv+")")
	}
	stty := exec.Command("stty", "raw", "-echo")
	stty.Stdin = os.Stdin
	_ = stty.Run()
	os.Stdout.WriteString("\x1b[?2004h")
	if os.Getenv("FAKE_TRUST") == "1" {
		f.dialog, f.trustYes = "trust", true
	}
	f.render()
	if prompt != "" {
		go func() {
			for {
				f.mu.Lock()
				d := f.dialog
				f.mu.Unlock()
				if d == "" {
					f.submit(prompt)
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
		}()
	}
	f.readLoop()
	return 0
}

func newConvID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func (f *fakeAgy) transcript() string {
	return filepath.Join(f.home, "brain", f.conv, ".system_generated", "logs", "transcript_full.jsonl")
}

func (f *fakeAgy) hook(ev string, extra map[string]any) {
	p := map[string]any{"conversationId": f.conv, "workspacePaths": f.ws, "transcriptPath": f.transcript(), "modelName": "fake-model"}
	for k, v := range extra {
		p[k] = v
	}
	b, _ := json.Marshal(p)
	for _, cmd := range f.hooks[ev] {
		c := exec.Command("/bin/sh", "-c", cmd)
		c.Stdin = strings.NewReader(string(b))
		_, _ = c.Output()
	}
}

func (f *fakeAgy) tx(rec map[string]any) {
	f.mu.Lock()
	rec["step_index"] = f.step
	f.step++
	f.mu.Unlock()
	rec["status"] = "DONE"
	rec["created_at"] = time.Now().UTC().Format(time.RFC3339)
	b, _ := json.Marshal(rec)
	_ = os.MkdirAll(filepath.Dir(f.transcript()), 0o700)
	fh, err := os.OpenFile(f.transcript(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	fh.Write(append(b, '\n'))
	fh.Close()
}

func (f *fakeAgy) say(line string) {
	f.mu.Lock()
	f.output = append(f.output, line)
	if len(f.output) > 20 {
		f.output = f.output[len(f.output)-20:]
	}
	f.mu.Unlock()
	f.render()
}

func fakeTermWidth() int {
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, 1, uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if e != 0 || ws.Col == 0 {
		return 200
	}
	return int(ws.Col)
}

func (f *fakeAgy) render() {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	b.WriteString("   FAKE Antigravity CLI 0.0.0-test\r\n\r\n")
	if f.dialog == "trust" {
		b.WriteString("Accessing workspace:\r\n" + f.cwd + "\r\nDo you trust the contents of this project?\r\n")
		b.WriteString("Antigravity CLI requires permission to read, edit, and execute files here.\r\n")
		if f.trustYes {
			b.WriteString("> Yes, I trust this folder\r\n  No, exit\r\n")
		} else {
			b.WriteString("  Yes, I trust this folder\r\n> No, exit\r\n")
		}
		b.WriteString("  ↑/↓ Navigate · enter Confirm\r\n")
		os.Stdout.WriteString(b.String())
		return
	}
	for _, l := range f.output {
		b.WriteString(l + "\r\n")
	}
	rule := strings.Repeat("─", fakeTermWidth())
	for _, q := range f.queue {
		b.WriteString("▸ " + q + " (alt+m to send immediately)\r\n")
	}
	if f.dialog == "perm" {
		b.WriteString(rule + "\r\nRequesting permission for:\r\n   touch x\r\nRun this command?\r\n> 1. Yes, run command\r\n  2. No, cancel\r\n")
		b.WriteString("  ↑/↓ Navigate · tab Amend\r\nesc to cancel")
		os.Stdout.WriteString(b.String())
		return
	}
	b.WriteString(rule + "\r\n")
	lines := strings.Split(string(f.input), "\n")
	for i, l := range lines {
		if i == 0 {
			b.WriteString(strings.TrimRight("> "+l, " ") + "\r\n")
		} else {
			b.WriteString("  " + l + "\r\n")
		}
	}
	b.WriteString(rule + "\r\n")
	footer := "? for shortcuts"
	switch {
	case f.busy:
		footer = "esc to cancel"
	case len(f.input) > 0:
		footer = ""
	}
	b.WriteString(footer + strings.Repeat(" ", 20) + f.model)
	os.Stdout.WriteString(b.String())
}

func (f *fakeAgy) readLoop() {
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
				f.render()
			case "A":
				f.key("up")
			case "B":
				f.key("down")
			}
		case '\r', '\n':
			if f.pasting {
				f.edit(func() { f.input = append(f.input, '\n') })
			} else {
				f.key("enter")
			}
		case 0x15:
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
			if c >= 0x20 {
				f.edit(func() { f.input = append(f.input, c) })
			}
		}
	}
}

func (f *fakeAgy) edit(fn func()) {
	f.mu.Lock()
	if f.dialog == "" {
		fn()
	}
	f.mu.Unlock()
	if !f.pasting {
		f.render()
	}
}

func (f *fakeAgy) key(k string) {
	f.mu.Lock()
	dialog := f.dialog
	f.mu.Unlock()
	switch dialog {
	case "trust":
		switch k {
		case "up", "down":
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
			f.render()
		}
		return
	case "perm":
		switch k {
		case "enter":
			f.permCh <- true
		case "esc":
			f.permCh <- false
		}
		return
	}
	switch k {
	case "enter":
		f.mu.Lock()
		text := string(f.input)
		f.input = nil
		busy := f.busy
		if busy && strings.TrimSpace(text) != "" {
			f.queue = append(f.queue, text)
		}
		f.mu.Unlock()
		if strings.TrimSpace(text) == "/exit" {
			os.Exit(0)
		}
		if !busy && strings.TrimSpace(text) != "" {
			go f.submit(text)
		}
		f.render()
	case "esc":
		f.mu.Lock()
		if f.busy && f.cancel != nil {
			close(f.cancel)
			f.cancel = nil
		}
		f.mu.Unlock()
	}
}

// submit runs one turn like agy: a new conversation gets its id and
// SessionStart on the first prompt; PreInvocation, the steps, then
// PostInvocation and Stop. FAKE_SLOW keeps the turn busy for 4s (Escape
// cancels it without a Stop hook, as agy does); FAKE_PERM opens a
// permission prompt.
func (f *fakeAgy) submit(text string) {
	f.mu.Lock()
	f.busy = true
	cancel := make(chan struct{})
	f.cancel = cancel
	newConv := !f.started
	if newConv {
		f.started = true
		f.conv = newConvID()
	}
	f.mu.Unlock()
	if newConv {
		_ = os.MkdirAll(filepath.Join(f.home, "conversations"), 0o700)
		_ = os.WriteFile(filepath.Join(f.home, "conversations", f.conv+".db"), nil, 0o600)
		f.hook("SessionStart", nil)
	}
	f.say("> " + strings.ReplaceAll(text, "\n", " "))
	f.tx(map[string]any{"source": "USER_EXPLICIT", "type": "USER_INPUT",
		"content": "<USER_REQUEST>\n" + text + "\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: now.\n</ADDITIONAL_METADATA>"})
	f.hook("PreInvocation", map[string]any{"invocationNum": 0})
	f.render()
	if strings.Contains(text, "FAKE_PERM") {
		f.mu.Lock()
		f.dialog, f.permCh = "perm", make(chan bool)
		ch := f.permCh
		f.mu.Unlock()
		f.render()
		<-ch
		f.mu.Lock()
		f.dialog = ""
		f.mu.Unlock()
	}
	if strings.Contains(text, "FAKE_SLOW") {
		select {
		case <-cancel:
			f.mu.Lock()
			f.busy = false
			f.mu.Unlock()
			f.say("  ⎿  Interrupted · What should Antigravity CLI do instead?")
			return
		case <-time.After(4 * time.Second):
		}
	}
	f.tx(map[string]any{"source": "MODEL", "type": "PLANNER_RESPONSE", "tool_calls": []map[string]any{{"name": "run_command", "args": map[string]any{"CommandLine": "true", "toolSummary": "check"}}}})
	f.hook("PostToolUse", map[string]any{"toolCall": map[string]any{"name": "run_command", "args": map[string]any{"CommandLine": "true"}}, "stepIdx": 1})
	reply := "ok: " + strings.ReplaceAll(text, "\n", " ")
	f.tx(map[string]any{"source": "MODEL", "type": "PLANNER_RESPONSE", "content": reply})
	f.say("  " + reply)
	f.hook("PostInvocation", map[string]any{"invocationNum": 0})
	f.hook("Stop", map[string]any{"executionNum": 1, "terminationReason": "NO_TOOL_CALL", "fullyIdle": true})
	f.mu.Lock()
	f.busy = false
	var q []string
	q, f.queue = f.queue, nil
	f.mu.Unlock()
	f.render()
	for _, t := range q {
		f.submit(t)
	}
}
