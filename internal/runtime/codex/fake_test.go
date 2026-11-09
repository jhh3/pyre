package codex

// Fake `codex` for tests. The test binary re-executes itself:
//   PYRE_TEST_ROLE=fakeappserver: `app-server --listen unix://SOCK` speaking
//     the JSON-RPC subset the adapter uses over a unix-socket WebSocket, with
//     broadcast notifications and server->client approval requests where the
//     first answer wins (serverRequest/resolved), like codex 0.160.0.
//   PYRE_TEST_ROLE=faketui: `resume THREAD --remote unix://SOCK`, a minimal
//     second client (the native TUI stand-in) that subscribes to the thread.
// Prompts containing FAKE_* directives drive scenarios.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestMain(m *testing.M) {
	switch os.Getenv("PYRE_TEST_ROLE") {
	case "fakeappserver":
		os.Exit(fakeAppServerMain(os.Args[1:]))
	case "faketui":
		os.Exit(fakeTUIMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

type fConn struct {
	c  *websocket.Conn
	mu sync.Mutex
}

func (fc *fConn) send(v any) {
	b, _ := json.Marshal(v)
	fc.mu.Lock()
	defer fc.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = fc.c.Write(ctx, websocket.MessageText, b)
}

type fTurn struct {
	ID     string            `json:"id"`
	Status string            `json:"status"`
	Items  []json.RawMessage `json:"items"`
	Error  any               `json:"error,omitempty"`
	// StartedAt: fixed synthetic start (unix seconds) for transcript times.
	StartedAt int64 `json:"startedAt"`
	intr      chan struct{}
}

type fThread struct {
	id       string
	named    bool   // thread/name/set seen (does not write the rollout)
	mat      bool   // rollout written (thread/inject_items or a turn)
	owner    *fConn // connection that started it
	parent   string
	status   map[string]any
	turns    []*fTurn
	active   *fTurn
	subs     map[*fConn]bool
	unloaded bool            // thread/archive unloaded it (the real app-server keeps threads loaded otherwise)
	config   map[string]any  // the per-thread config the loaded thread runs commands with
	bg       map[string]*fBg // background commands (FAKE_BG_MS) still running, by item id
	// held: the process groups of FAKE_KEEP exec sessions the thread holds,
	// exited ones included (codex kills them all on clean and unload).
	held map[string]int
	// finals: local agents' reports queued for the next turn (FAKE_AGENT).
	finals []string
}

type fakeServer struct {
	mu       sync.Mutex
	threads  map[string]*fThread
	seq      int
	reqSeq   int
	pending  map[string]chan json.RawMessage // server request id -> first answer
	resolved map[string]bool
	conns    map[*fConn]bool
}

func (s *fakeServer) id(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s-%06d", prefix, s.seq)
}

func (s *fakeServer) broadcast(t *fThread, method string, params any) {
	s.mu.Lock()
	var subs []*fConn
	for c := range t.subs {
		subs = append(subs, c)
	}
	s.mu.Unlock()
	for _, c := range subs {
		c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	}
}

func (s *fakeServer) setStatus(t *fThread, st map[string]any) {
	s.mu.Lock()
	t.status = st
	s.mu.Unlock()
	s.broadcast(t, "thread/status/changed", map[string]any{"threadId": t.id, "status": st})
}

func (s *fakeServer) threadJSON(t *fThread, withTurns bool) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[string]any{"id": t.id, "status": t.status, "cwd": "/fake", "fakeSubscribers": len(t.subs),
		"fakeRunId": t.config["shell_environment_policy.set.PYRE_RUN_ID"]} // what the thread's commands see
	if t.parent != "" {
		m["parentThreadId"] = t.parent
	}
	if withTurns {
		m["turns"] = t.turns
	}
	return m
}

func (s *fakeServer) addItem(t *fThread, tr *fTurn, it map[string]any) {
	b, _ := json.Marshal(it)
	s.mu.Lock()
	tr.Items = append(tr.Items, b)
	s.mu.Unlock()
	p := map[string]any{"item": it, "threadId": t.id, "turnId": tr.ID, "startedAtMs": time.Now().UnixMilli()}
	s.broadcast(t, "item/started", p)
	s.broadcast(t, "item/completed", p)
}

func fakeAppServerMain(args []string) int {
	sock := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--listen" && i+1 < len(args) {
			sock = strings.TrimPrefix(args[i+1], "unix://")
		}
	}
	if sock == "" {
		fmt.Println("no --listen")
		return 2
	}
	f, _ := os.OpenFile(filepath.Join(filepath.Dir(sock), "fake-starts.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	fmt.Fprintf(f, "%d %s\n", os.Getpid(), os.Getenv("CODEX_HOME"))
	f.Close()
	fakeConfigLog = filepath.Join(filepath.Dir(sock), "fake-config.log")
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		fmt.Println("listen:", err)
		return 1
	}
	s := &fakeServer{threads: map[string]*fThread{}, pending: map[string]chan json.RawMessage{}, resolved: map[string]bool{}, conns: map[*fConn]bool{}}
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
		<-ch
		_ = ln.Close()
		os.Exit(0)
	}()
	fmt.Println("fake codex app-server listening")
	_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(64 << 20)
		fc := &fConn{c: c}
		s.mu.Lock()
		s.conns[fc] = true
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.conns, fc)
			for _, t := range s.threads {
				delete(t.subs, fc)
			}
			s.mu.Unlock()
		}()
		for {
			_, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			var m rpcMessage
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			if m.Method == "" && len(m.ID) > 0 { // answer to a server request
				s.mu.Lock()
				ch := s.pending[string(m.ID)]
				s.mu.Unlock()
				if ch != nil {
					select {
					case ch <- m.Result:
					default: // late answers are ignored
					}
				}
				continue
			}
			if len(m.ID) == 0 {
				continue // notification (initialized)
			}
			go s.handle(fc, m)
		}
	}))
	return 0
}

func (s *fakeServer) reply(fc *fConn, id json.RawMessage, result any) {
	fc.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *fakeServer) fail(fc *fConn, id json.RawMessage, msg string) {
	fc.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32600, "message": msg}})
}

// fakeTurnStartFault logs a turn/start's clientUserMessageId to
// $CODEX_HOME/fake-turnstart.log and returns the next fault scripted in
// $CODEX_HOME/fake-turnstart-faults (one per line: "rpc" refuses, "drop"
// closes the connection unanswered, "" passes), consuming it.
func fakeTurnStartFault(cid string) string {
	home := os.Getenv("CODEX_HOME")
	if f, err := os.OpenFile(filepath.Join(home, "fake-turnstart.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintln(f, cid)
		f.Close()
	}
	path := filepath.Join(home, "fake-turnstart-faults")
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return ""
	}
	first, rest, _ := strings.Cut(string(b), "\n")
	_ = os.WriteFile(path, []byte(rest), 0o600)
	return strings.TrimSpace(first)
}

// fakeConfigLog records "<method> <config JSON>" for thread/start and
// thread/resume (per-thread config overrides).
var fakeConfigLog string

func logConfig(method string, p map[string]any) {
	if fakeConfigLog == "" {
		return
	}
	b, _ := json.Marshal(p["config"])
	f, err := os.OpenFile(fakeConfigLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "%s %s\n", method, b)
	f.Close()
}

var fSleepRe = regexp.MustCompile(`FAKE_SLEEP_MS=(\d+)`)

// FAKE_BG_MS=<ms>:<name> starts a background command (codex 0.163 unified
// exec) in the turn: item/started now, item/completed for it, carrying the
// turn that started it, after <ms> even when that turn has ended. The
// rollout records it like codex: exec_command yielding its session id, and
// the item_completed event at the exit.
var fBgRe = regexp.MustCompile(`FAKE_BG_MS=(\d+):([a-z0-9]+)`)

// FAKE_POLL=<name>: at the end of the turn (after FAKE_SLEEP_MS) the model
// polls background command <name> with write_stdin (rollout only, as codex
// emits nothing for a poll that finds the process exited).
var fPollRe = regexp.MustCompile(`FAKE_POLL=([a-z0-9]+)`)

// FAKE_EXECS=<n>: before any FAKE_BG_MS command, the model runs n quick
// exec_command calls that return their exit (no yield), like the dozens
// of cat/sed/rg calls of a long codex turn.
var fExecsRe = regexp.MustCompile(`FAKE_EXECS=(\d+)`)

// quickExecs runs FAKE_EXECS's commands in turn tr.
func (s *fakeServer) quickExecs(t *fThread, tr *fTurn, text string) {
	m := fExecsRe.FindStringSubmatch(text)
	if m == nil {
		return
	}
	n, _ := strconv.Atoi(m[1])
	for i := 0; i < n; i++ {
		id, name, pid := fmt.Sprintf("exec-q-%d", i), fmt.Sprintf("q-%d", i), strconv.Itoa(30000+i)
		it := map[string]any{"type": "commandExecution", "id": id, "command": "/bin/zsh -lc '" + name + "'",
			"commandActions": []any{map[string]any{"type": "unknown", "command": name}}, "cwd": "/fake/work",
			"status": "inProgress", "source": "unifiedExecStartup", "processId": pid}
		s.broadcast(t, "item/started", map[string]any{"item": it, "threadId": t.id, "turnId": tr.ID})
		s.rolloutTool(t, "call-"+id, `{"cmd":"`+name+`"}`, "Process exited with code 0\nOutput:\nok\n")
		now := time.Now()
		s.rollout(t, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "item_completed", "thread_id": t.id, "turn_id": tr.ID,
			"item": map[string]any{"type": "CommandExecution", "id": id, "process_id": pid, "status": "completed", "exit_code": 0,
				"command": []string{"/bin/zsh", "-lc", name}, "cwd": "file:///fake/work", "source": "unified_exec_startup",
				"aggregated_output": "ok\n", "duration": map[string]any{"secs": 0, "nanos": 20000000}},
			"started_at_ms": now.UnixMilli(), "completed_at_ms": now.UnixMilli()}})
		done := map[string]any{}
		for k, v := range it {
			done[k] = v
		}
		done["status"], done["exitCode"], done["aggregatedOutput"], done["durationMs"] = "completed", 0, "ok\n", 20
		s.broadcast(t, "item/completed", map[string]any{"item": done, "threadId": t.id, "turnId": tr.ID})
	}
}

type fBg struct {
	timer *time.Timer
	turn  string
	item  map[string]any
	start time.Time
	pgid  int // FAKE_KEEP: the real command's process group
}

// fBgPid is background command name's process (session) id.
func fBgPid(name string) string {
	n := 0
	for _, c := range name {
		n = n*31 + int(c)
	}
	return strconv.Itoa(40000 + n%9000)
}

var fRolloutMu sync.Mutex

// rollout appends one line to the thread's rollout under CODEX_HOME.
func (s *fakeServer) rollout(t *fThread, line map[string]any) {
	fRolloutMu.Lock()
	defer fRolloutMu.Unlock()
	day := time.Now()
	dir := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", day.Format("2006"), day.Format("01"), day.Format("02"))
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "rollout-2026-10-23T00-00-00-"+t.id+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	line["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(line)
	_, _ = f.Write(append(b, '\n'))
}

func (s *fakeServer) rolloutTool(t *fThread, callID, args string, out string) {
	s.rollout(t, map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "name": "exec_command", "call_id": callID, "arguments": args}})
	s.rollout(t, map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": callID, "output": out}})
}

func (s *fakeServer) startBg(t *fThread, tr *fTurn, text string) {
	for _, m := range fBgRe.FindAllStringSubmatch(text, -1) {
		ms, _ := strconv.Atoi(m[1])
		name, id := m[2], "exec-bg-"+m[2]
		it := map[string]any{"type": "commandExecution", "id": id, "command": "/bin/zsh -lc 'bg-" + name + "'",
			"commandActions": []any{map[string]any{"type": "unknown", "command": "bg-" + name}}, "cwd": "/fake/work",
			"status": "inProgress", "source": "unifiedExecStartup", "processId": fBgPid(name)}
		s.broadcast(t, "item/started", map[string]any{"item": it, "threadId": t.id, "turnId": tr.ID})
		s.rolloutTool(t, "call-"+id, `{"cmd":"bg-`+name+`","yield_time_ms":1000}`, "Process running with session ID "+fBgPid(name)+"\nOutput:\n")
		s.mu.Lock()
		if t.bg == nil {
			t.bg = map[string]*fBg{}
		}
		t.bg[id] = &fBg{turn: tr.ID, item: it, start: time.Now(), timer: time.AfterFunc(time.Duration(ms)*time.Millisecond, func() {
			s.endBg(t, id, 0, "completed", "bg "+name+" done\n")
		})}
		s.mu.Unlock()
	}
}

// endBg reports a background command's exit (once).
func (s *fakeServer) endBg(t *fThread, id string, exit int, status, out string) {
	s.mu.Lock()
	b := t.bg[id]
	delete(t.bg, id)
	s.mu.Unlock()
	if b == nil {
		return
	}
	done := map[string]any{}
	for k, v := range b.item {
		done[k] = v
	}
	done["status"], done["exitCode"], done["aggregatedOutput"], done["durationMs"] = status, exit, out, 1234
	now := time.Now()
	s.rollout(t, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "item_completed", "thread_id": t.id, "turn_id": b.turn,
		"item": map[string]any{"type": "CommandExecution", "id": id, "process_id": done["processId"], "status": status, "exit_code": exit,
			"command": []string{"/bin/zsh", "-lc", "bg-" + strings.TrimPrefix(id, "exec-bg-")}, "cwd": "file:///fake/work", "source": "unified_exec_startup",
			"aggregated_output": out, "duration": map[string]any{"secs": 1, "nanos": 234000000}},
		"started_at_ms": b.start.UnixMilli(), "completed_at_ms": now.UnixMilli()}})
	s.broadcast(t, "item/completed", map[string]any{"item": done, "threadId": t.id, "turnId": b.turn})
}

// FAKE_KEEP=<ms>:<name> starts a real background command (codex 0.163
// unified exec, pipe mode, its own process group) whose top shell leaves a
// child (`sleep 600`, pid in $CODEX_HOME/keep-<name>.child) in its group
// and exits after <ms>. The thread holds the exec session after the exit,
// so its clean and unload kill the group (killpg SIGKILL), as Codex does.
// thread/backgroundTerminals/list shows it, with its osPid, while the top
// shell runs.
var fKeepRe = regexp.MustCompile(`FAKE_KEEP=(\d+):([a-z0-9]+)`)

func (s *fakeServer) startKeep(t *fThread, tr *fTurn, text string) {
	for _, m := range fKeepRe.FindAllStringSubmatch(text, -1) {
		ms, _ := strconv.Atoi(m[1])
		name, id := m[2], "exec-keep-"+m[2]
		childFile := filepath.Join(os.Getenv("CODEX_HOME"), "keep-"+name+".child")
		cmd := exec.Command("/bin/sh", "-c", fmt.Sprintf(`sleep 600 & echo $! > %q; sleep %.3f`, childFile, float64(ms)/1000))
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			continue
		}
		pgid := cmd.Process.Pid
		it := map[string]any{"type": "commandExecution", "id": id, "command": "/bin/zsh -lc 'keep-" + name + "'",
			"commandActions": []any{map[string]any{"type": "unknown", "command": "keep-" + name}}, "cwd": "/fake/work",
			"status": "inProgress", "source": "unifiedExecStartup", "processId": fBgPid(name)}
		s.mu.Lock()
		if t.bg == nil {
			t.bg = map[string]*fBg{}
		}
		if t.held == nil {
			t.held = map[string]int{}
		}
		t.held[id] = pgid
		t.bg[id] = &fBg{turn: tr.ID, item: it, start: time.Now(), timer: time.AfterFunc(24*time.Hour, func() {}), pgid: pgid}
		s.mu.Unlock()
		s.broadcast(t, "item/started", map[string]any{"item": it, "threadId": t.id, "turnId": tr.ID})
		s.rolloutTool(t, "call-"+id, `{"cmd":"keep-`+name+`","yield_time_ms":250}`, "Process running with session ID "+fBgPid(name)+"\nOutput:\n")
		go func() {
			_ = cmd.Wait()
			s.endBg(t, id, 0, "completed", "keep "+name+" done\n")
		}()
	}
}

// killHeld kills the process groups of the exec sessions t holds (codex's
// terminate_all_processes / clean), exited ones included.
func (s *fakeServer) killHeld(t *fThread) {
	if t == nil {
		return
	}
	s.mu.Lock()
	held := t.held
	t.held = nil
	s.mu.Unlock()
	for _, pg := range held {
		_ = syscall.Kill(-pg, syscall.SIGKILL)
	}
}

func (s *fakeServer) handle(fc *fConn, m rpcMessage) {
	var p map[string]any
	_ = json.Unmarshal(m.Params, &p)
	tid, _ := p["threadId"].(string)
	s.mu.Lock()
	t := s.threads[tid]
	s.mu.Unlock()
	switch m.Method {
	case "initialize":
		s.reply(fc, m.ID, map[string]any{"userAgent": "fake/0", "codexHome": os.Getenv("CODEX_HOME")})
	case "thread/inject_items":
		if t != nil {
			s.mu.Lock()
			t.mat = true
			s.mu.Unlock()
		}
		s.reply(fc, m.ID, map[string]any{})
	case "thread/name/set":
		if t != nil {
			s.mu.Lock()
			t.named = true
			s.mu.Unlock()
		}
		s.reply(fc, m.ID, map[string]any{})
	case "hooks/list": // the hooks in $CODEX_HOME/fake-hooks.json (absent: an older Codex without the method)
		b, err := os.ReadFile(filepath.Join(os.Getenv("CODEX_HOME"), "fake-hooks.json"))
		if err != nil {
			s.fail(fc, m.ID, "method not found: hooks/list")
			return
		}
		s.reply(fc, m.ID, map[string]any{"data": []any{map[string]any{"cwd": "/fake", "hooks": json.RawMessage(b), "errors": []any{}, "warnings": []any{}}}})
	case "config/value/write":
		logConfig(m.Method, map[string]any{"config": p})
		s.reply(fc, m.ID, map[string]any{"status": "ok", "version": "v1", "filePath": filepath.Join(os.Getenv("CODEX_HOME"), "config.toml")})
	case "thread/start":
		logConfig(m.Method, p)
		s.mu.Lock()
		cfg, _ := p["config"].(map[string]any)
		t = &fThread{id: s.id("thr"), status: map[string]any{"type": "idle"}, subs: map[*fConn]bool{fc: true}, owner: fc, config: cfg}
		s.threads[t.id] = t
		s.mu.Unlock()
		// The thread's record on disk, so a restarted fake resumes it.
		_ = os.MkdirAll(filepath.Join(os.Getenv("CODEX_HOME"), "fake-threads"), 0o700)
		_ = os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "fake-threads", t.id), nil, 0o600)
		s.reply(fc, m.ID, map[string]any{"thread": s.threadJSON(t, false), "approvalPolicy": p["approvalPolicy"], "sandbox": p["sandbox"]})
		s.broadcast(t, "thread/started", map[string]any{"thread": s.threadJSON(t, false)})
	case "thread/resume":
		logConfig(m.Method, p)
		if _, err := os.Stat(filepath.Join(os.Getenv("CODEX_HOME"), "fake-threads", tid)); t == nil && tid != "" && err == nil {
			// Started on an app-server that died: cold-load it, idle.
			s.mu.Lock()
			if t = s.threads[tid]; t == nil {
				t = &fThread{id: tid, mat: true, status: map[string]any{"type": "idle"}, subs: map[*fConn]bool{}}
				s.threads[tid] = t
			}
			s.mu.Unlock()
		}
		if t == nil {
			s.fail(fc, m.ID, "no rollout found for thread id "+tid)
			return
		}
		s.mu.Lock()
		unmaterialized := fc != t.owner && !t.mat && len(t.turns) == 0
		s.mu.Unlock()
		if unmaterialized { // codex 0.160.0: no rollout before the first turn or a name
			s.fail(fc, m.ID, "no rollout found for thread id "+tid)
			return
		}
		// codex 0.163 resume_running_thread: overrides (any config) on a
		// loaded, idle thread nobody subscribes to shut it down and
		// cold-resume it with them, killing its background commands (exit
		// -1). A loaded thread with a subscriber (the caller's own
		// connection included) attaches and keeps its config. An unloaded
		// thread cold-loads with the resume's config.
		s.mu.Lock()
		cfg, _ := p["config"].(map[string]any)
		var kill []string
		rebuilt := false
		if t.unloaded {
			t.config = cfg
		} else if cfg != nil && len(t.subs) == 0 && t.active == nil {
			t.config = cfg
			rebuilt = true
			for id, b := range t.bg {
				b.timer.Stop()
				kill = append(kill, id)
			}
			if f, err := os.OpenFile(filepath.Join(os.Getenv("CODEX_HOME"), "fake-rebuilds.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				fmt.Fprintf(f, "%s killed=%d\n", t.id, len(kill))
				f.Close()
			}
		}
		t.subs[fc] = true
		t.unloaded = false
		s.mu.Unlock()
		if rebuilt {
			s.killHeld(t) // the rebuild shuts the thread down
		}
		for _, id := range kill {
			s.endBg(t, id, -1, "failed", "")
		}
		s.reply(fc, m.ID, map[string]any{"thread": s.threadJSON(t, false)})
	case "thread/unsubscribe":
		if t != nil {
			s.mu.Lock()
			delete(t.subs, fc)
			s.mu.Unlock()
		}
		s.reply(fc, m.ID, map[string]any{})
	case "thread/turns/list":
		if t == nil {
			s.fail(fc, m.ID, "unknown thread")
			return
		}
		s.mu.Lock()
		turns := append([]*fTurn{}, t.turns...)
		if p["sortDirection"] == "desc" {
			for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
				turns[i], turns[j] = turns[j], turns[i]
			}
		}
		// Pages of `limit` turns; the cursor is the index of the next turn.
		from, _ := strconv.Atoi(strings.TrimPrefix(fmt.Sprint(p["cursor"]), "c"))
		if from > len(turns) {
			s.mu.Unlock()
			s.fail(fc, m.ID, "invalid cursor")
			return
		}
		next := ""
		if lim, _ := p["limit"].(float64); lim > 0 && from+int(lim) < len(turns) {
			turns, next = turns[from:from+int(lim)], fmt.Sprintf("c%d", from+int(lim))
		} else {
			turns = turns[from:]
		}
		b, _ := json.Marshal(turns)
		s.mu.Unlock()
		if f, err := os.OpenFile(filepath.Join(os.Getenv("CODEX_HOME"), "fake-turns-list.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			fmt.Fprintf(f, "%v %d\n", p["cursor"], len(turns))
			f.Close()
		}
		res := map[string]any{"data": json.RawMessage(b)}
		if next != "" {
			res["nextCursor"] = next
		}
		s.reply(fc, m.ID, res)
	case "turn/start", "turn/steer":
		if t == nil {
			s.fail(fc, m.ID, "thread not loaded")
			return
		}
		text := ""
		if in, ok := p["input"].([]any); ok && len(in) > 0 {
			text, _ = in[0].(map[string]any)["text"].(string)
		}
		cid, _ := p["clientUserMessageId"].(string)
		if fail := fakeTurnStartFault(cid); fail == "rpc" {
			s.fail(fc, m.ID, "synthetic refusal")
			return
		} else if fail == "drop" { // outcome unknown to the client
			_ = fc.c.CloseNow()
			return
		}
		s.mu.Lock()
		active := t.active
		s.mu.Unlock()
		user := map[string]any{"type": "userMessage", "id": "itm-" + strconv.FormatInt(time.Now().UnixNano(), 36), "content": []any{map[string]any{"type": "text", "text": text}}}
		if cid != "" {
			user["clientId"] = cid
		}
		if m.Method == "turn/steer" {
			exp, _ := p["expectedTurnId"].(string)
			if active == nil {
				s.fail(fc, m.ID, "no active turn to steer")
				return
			}
			if exp != active.ID {
				s.fail(fc, m.ID, "expected active turn id `"+exp+"` but found `"+active.ID+"`")
				return
			}
			s.reply(fc, m.ID, map[string]any{"turnId": active.ID})
			s.addItem(t, active, user)
			return
		}
		if active != nil { // turn/start on a busy thread joins the active turn
			s.reply(fc, m.ID, map[string]any{"turn": map[string]any{"id": active.ID, "status": "inProgress"}})
			s.addItem(t, active, user)
			return
		}
		s.mu.Lock()
		tr := &fTurn{ID: s.id("turn"), Status: "inProgress", Items: []json.RawMessage{}, StartedAt: 1791100000, intr: make(chan struct{}, 1)}
		t.turns = append(t.turns, tr)
		t.active = tr
		s.mu.Unlock()
		s.rollout(t, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": tr.ID}})
		s.reply(fc, m.ID, map[string]any{"turn": map[string]any{"id": tr.ID, "status": "inProgress"}})
		s.setStatus(t, map[string]any{"type": "active", "activeFlags": []string{}})
		s.broadcast(t, "turn/started", map[string]any{"threadId": t.id, "turn": map[string]any{"id": tr.ID, "status": "inProgress"}})
		if !strings.Contains(text, "FAKE_NO_ITEM") {
			s.addItem(t, tr, user)
		}
		go s.runTurn(t, tr, text)
	case "turn/interrupt":
		s.mu.Lock()
		var tr *fTurn
		if t != nil {
			tr = t.active
		}
		s.mu.Unlock()
		if tr == nil || tr.ID != p["turnId"] {
			return // interrupting a finished turn never responds
		}
		s.reply(fc, m.ID, map[string]any{})
		select {
		case tr.intr <- struct{}{}:
		default:
		}
	case "thread/backgroundTerminals/list":
		var data []map[string]any
		if t != nil {
			s.mu.Lock()
			for id, b := range t.bg {
				e := map[string]any{"itemId": id, "processId": b.item["processId"], "command": b.item["command"], "cwd": "/fake/work", "osPid": nil}
				if b.pgid > 0 {
					e["osPid"] = b.pgid
				}
				data = append(data, e)
			}
			s.mu.Unlock()
		}
		s.reply(fc, m.ID, map[string]any{"data": data, "nextCursor": nil})
	case "thread/backgroundTerminals/clean":
		// Kills the thread's background commands: each reports its exit.
		if f, err := os.OpenFile(filepath.Join(os.Getenv("CODEX_HOME"), "fake-cleans.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			fmt.Fprintln(f, tid)
			f.Close()
		}
		s.killHeld(t)
		var kill []string
		if t != nil {
			s.mu.Lock()
			for id, b := range t.bg {
				b.timer.Stop()
				kill = append(kill, id)
			}
			s.mu.Unlock()
		}
		s.reply(fc, m.ID, map[string]any{})
		// Like codex's exit watcher, the exits arrive after the reply.
		time.AfterFunc(100*time.Millisecond, func() {
			for _, id := range kill {
				s.endBg(t, id, -1, "failed", "")
			}
		})
	case "thread/loaded/list":
		s.mu.Lock()
		ids := []string{}
		for id, th := range s.threads {
			if !th.unloaded && th.parent == "" {
				ids = append(ids, id)
			}
		}
		s.mu.Unlock()
		s.reply(fc, m.ID, map[string]any{"data": ids})
	case "thread/read":
		if t == nil {
			s.fail(fc, m.ID, "no thread "+tid)
			return
		}
		s.reply(fc, m.ID, map[string]any{"thread": s.threadJSON(t, false)})
	case "thread/archive", "thread/unarchive":
		logConfig(m.Method, map[string]any{"config": map[string]any{"threadId": tid}})
		if t != nil && m.Method == "thread/archive" {
			s.killHeld(t) // the thread's shutdown terminates its processes
			s.mu.Lock()
			t.unloaded = true
			t.subs = map[*fConn]bool{} // the thread is shut down
			s.mu.Unlock()
		}
		s.reply(fc, m.ID, map[string]any{})
	case "account/rateLimits/read":
		s.reply(fc, m.ID, map[string]any{"rateLimits": map[string]any{"limitId": "codex", "primary": map[string]any{"usedPercent": 12, "windowDurationMins": 300, "resetsAt": 1791200000}, "secondary": map[string]any{"usedPercent": 40, "windowDurationMins": 10080, "resetsAt": 1791600000}}})
	case "account/read":
		if r, _ := p["refreshToken"].(bool); r {
			// A managed-auth refresh: leave evidence in the home.
			_ = os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "fake-refreshed"), []byte("1"), 0o600)
		}
		s.reply(fc, m.ID, map[string]any{"account": map[string]any{"type": "chatgpt", "email": "synthetic@example.invalid", "planType": "pro"}, "requiresOpenaiAuth": true})
	default:
		s.fail(fc, m.ID, "method not implemented in fake: "+m.Method)
	}
}

func (s *fakeServer) finish(t *fThread, tr *fTurn, status string, errObj any) {
	s.mu.Lock()
	tr.Status = status
	tr.Error = errObj
	t.active = nil
	s.mu.Unlock()
	if status == "interrupted" {
		s.rollout(t, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "turn_aborted", "turn_id": tr.ID, "reason": "interrupted"}})
	} else {
		s.rollout(t, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "task_complete", "turn_id": tr.ID}})
	}
	s.setStatus(t, map[string]any{"type": "idle"})
	s.broadcast(t, "turn/completed", map[string]any{"threadId": t.id, "turn": map[string]any{"id": tr.ID, "status": status, "error": errObj}})
}

// FAKE_AGENT=<ms>:<name> spawns a local collaboration agent that finishes
// after <ms>, as codex 0.163 reports it: item/completed subAgentActivity
// (kind completed) on the root, with the spawning turn's id. Its report
// (the rollout's FINAL_ANSWER agent_message) reaches the model in the turn
// running then, else at the next turn's start. FAKE_AGENTU: finishing in
// a running turn, its report misses that turn (the turn's last model call
// was already made).
var fAgentRe = regexp.MustCompile(`FAKE_AGENT(U?)=(\d+):([a-z0-9_]+)`)

func (s *fakeServer) startAgents(t *fThread, tr *fTurn, text string) {
	for _, m := range fAgentRe.FindAllStringSubmatch(text, -1) {
		ms, _ := strconv.Atoi(m[2])
		late, path := m[1] == "U", "/root/"+m[3]
		child := s.idLocked("thr")
		s.addItem(t, tr, map[string]any{"type": "subAgentActivity", "id": "call-spawn-" + m[3], "kind": "started", "agentThreadId": child, "agentPath": path})
		time.AfterFunc(time.Duration(ms)*time.Millisecond, func() {
			s.mu.Lock()
			active := t.active != nil && !late
			if !active {
				t.finals = append(t.finals, path)
			}
			s.mu.Unlock()
			if active {
				s.rolloutFinal(t, path)
			}
			it := map[string]any{"type": "subAgentActivity", "id": "subagent-completed-" + s.idLocked("turn"), "kind": "completed", "agentThreadId": child, "agentPath": path}
			p := map[string]any{"item": it, "threadId": t.id, "turnId": tr.ID}
			s.broadcast(t, "item/started", p)
			s.broadcast(t, "item/completed", p)
		})
	}
}

func (s *fakeServer) idLocked(prefix string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id(prefix)
}

func (s *fakeServer) rolloutFinal(t *fThread, path string) {
	s.rollout(t, map[string]any{"type": "response_item", "payload": map[string]any{"type": "agent_message", "author": path, "recipient": "/root",
		"content": []any{map[string]any{"type": "input_text", "text": "Message Type: FINAL_ANSWER\nTask name: /root\nSender: " + path + "\nPayload:\ndone"}}}})
}

func (s *fakeServer) runTurn(t *fThread, tr *fTurn, text string) {
	s.mu.Lock()
	finals := t.finals
	t.finals = nil
	s.mu.Unlock()
	for _, p := range finals {
		s.rolloutFinal(t, p)
	}
	s.quickExecs(t, tr, text)
	s.startBg(t, tr, text)
	s.startAgents(t, tr, text)
	s.startKeep(t, tr, text)
	if m := fSleepRe.FindStringSubmatch(text); m != nil {
		n, _ := strconv.Atoi(m[1])
		s.addItem(t, tr, map[string]any{"type": "commandExecution", "id": "exec-1", "command": "sleep", "status": "inProgress"})
		select {
		case <-time.After(time.Duration(n) * time.Millisecond):
		case <-tr.intr:
			s.finish(t, tr, "interrupted", nil)
			return
		}
	}
	if strings.Contains(text, "FAKE_APPROVAL") {
		s.mu.Lock()
		s.reqSeq++
		rid := strconv.Itoa(s.reqSeq - 1) // codex numbers server requests from 0
		ch := make(chan json.RawMessage, 1)
		s.pending[rid] = ch
		s.mu.Unlock()
		s.setStatus(t, map[string]any{"type": "active", "activeFlags": []string{"waitingOnApproval"}})
		req := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(rid), "method": "item/commandExecution/requestApproval",
			"params": map[string]any{"threadId": t.id, "turnId": tr.ID, "itemId": "exec-appr", "startedAtMs": time.Now().UnixMilli(),
				"command": "touch approved.txt", "availableDecisions": []any{"accept", map[string]any{"acceptWithExecpolicyAmendment": map[string]any{"execpolicy_amendment": []string{"touch"}}}, "cancel"}}}
		s.mu.Lock()
		var subs []*fConn
		for c := range t.subs {
			subs = append(subs, c)
		}
		s.mu.Unlock()
		for _, c := range subs {
			c.send(req)
		}
		var ans json.RawMessage
		select {
		case ans = <-ch:
		case <-tr.intr:
			s.finish(t, tr, "interrupted", nil)
			return
		}
		s.broadcast(t, "serverRequest/resolved", map[string]any{"threadId": t.id, "requestId": json.RawMessage(rid)})
		s.setStatus(t, map[string]any{"type": "active", "activeFlags": []string{}})
		var d struct{ Decision any }
		_ = json.Unmarshal(ans, &d)
		if d.Decision == "accept" {
			s.addItem(t, tr, map[string]any{"type": "commandExecution", "id": "exec-appr", "command": "touch approved.txt", "status": "completed", "exitCode": 0})
		}
	}
	if strings.Contains(text, "FAKE_SUBAGENT") {
		s.mu.Lock()
		child := &fThread{id: s.id("thr"), parent: t.id, status: map[string]any{"type": "idle"}, subs: map[*fConn]bool{}}
		for c := range t.subs {
			child.subs[c] = true
		}
		s.threads[child.id] = child
		s.mu.Unlock()
		s.broadcast(child, "thread/started", map[string]any{"thread": s.threadJSON(child, false)})
		s.addItem(t, tr, map[string]any{"type": "subAgentActivity", "id": "call-sa", "kind": "started", "agentThreadId": child.id, "agentPath": "worker"})
		s.broadcast(child, "turn/completed", map[string]any{"threadId": child.id, "turn": map[string]any{"id": "turn-child", "status": "completed"}})
		s.rolloutFinal(t, "worker") // its report reaches this turn
		s.addItem(t, tr, map[string]any{"type": "subAgentActivity", "id": "sa-done", "kind": "completed", "agentThreadId": child.id, "agentPath": "worker"})
	}
	if strings.Contains(text, "FAKE_CAPACITY") {
		// server, codex 0.166 (iss_…): a commentary
		// message mid-work, then the turn fails at model capacity.
		s.addItem(t, tr, map[string]any{"type": "agentMessage", "id": "msg-c-" + tr.ID, "phase": "commentary", "text": "I'm now testing the probe with a real MP4."})
		s.finish(t, tr, "failed", map[string]any{"message": "Selected model is at capacity. Please try a different model.", "codexErrorInfo": "serverOverloaded"})
		return
	}
	if strings.Contains(text, "FAKE_LIMIT") {
		s.finish(t, tr, "failed", map[string]any{"message": "You've hit your usage limit.", "codexErrorInfo": "usageLimitExceeded"})
		return
	}
	for _, m := range fPollRe.FindAllStringSubmatch(text, -1) {
		s.rolloutTool(t, "call-poll-"+m[1], `{"session_id":`+fBgPid(m[1])+`,"chars":""}`, "Process exited with code 0\nOutput:\n")
	}
	reply := "ok"
	if _, rest, ok := strings.Cut(text, "FAKE_ECHO\n"); ok { // FAKE_ECHO: the answer is the rest of the prompt
		reply = rest
	}
	s.addItem(t, tr, map[string]any{"type": "agentMessage", "id": "msg-" + tr.ID, "text": reply})
	s.finish(t, tr, "completed", nil)
}

// fakeTUIMain is the native-TUI stand-in: subscribes to the thread on the
// app-server and prints its notifications.
func fakeTUIMain(args []string) int {
	var thread, sock string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "resume":
			if i+1 < len(args) {
				thread = args[i+1]
			}
		case "--remote":
			if i+1 < len(args) {
				sock = strings.TrimPrefix(args[i+1], "unix://")
			}
		}
	}
	if os.Getenv("PYRE_FAKE_HOOK_REVIEW") == "1" {
		fakeHookReview()
	}
	sc := bufio.NewScanner(os.Stdin)
	if os.Getenv("PYRE_FAKE_TRUST_PROMPT") == "1" {
		// Codex 0.160.1's folder-trust screen (verbatim layout).
		fmt.Print("  Folder access\n  " + os.Getenv("PWD") + "\n  Trust this folder? Codex can read, edit, and run files here, subject to your permission settings.\n" +
			"› 1. Trust and continue\n  2. Back to Agent Command Center\n  enter continue · esc back\n")
		if !sc.Scan() {
			return 1
		}
		fmt.Print("\033[2J\033[H")
		fmt.Println("FAKE TRUST ANSWERED")
	}
	c, err := dialRPC(context.Background(), sock, func(m string, _ json.RawMessage) { fmt.Println("notify", m) }, nil)
	if err != nil {
		fmt.Println("dial:", err)
		return 1
	}
	_ = c.call(context.Background(), "initialize", initializeParams{ClientInfo: clientInfo{Name: "fake-tui", Version: "0"}}, nil)
	if err := c.call(context.Background(), "thread/resume", threadResumeParams{ThreadID: thread}, nil); err != nil {
		fmt.Println("FAKE CODEX TUI resume failed:", err)
		return 1
	}
	fmt.Println("FAKE CODEX TUI attached to", thread)
	if os.Getenv("PYRE_FAKE_TRUST_PROMPT") == "1" {
		fmt.Println("› Ask Codex to do anything")
	}
	for sc.Scan() {
	}
	return 0
}

// fakeHookReview draws Codex 0.166's "Hooks need review" screen (layout of
// testdata/codex-0.166-hooks-review.txt) in raw mode: Up/Down move the
// selection, Enter answers it with the selected row.
func fakeHookReview() {
	tty := func(args ...string) {
		c := exec.Command("stty", args...)
		c.Stdin = os.Stdin
		_ = c.Run()
	}
	tty("raw", "-echo")
	defer tty("sane")
	rows := []string{"Review hooks", "Trust all and continue", "Continue without trusting (hooks won't run)"}
	sel := 0
	draw := func() {
		fmt.Print("\033[2J\033[H  Hooks need review\r\n  1 hook is new or changed.\r\n  Hooks can run outside the sandbox after you trust them.\r\n\r\n")
		for i, r := range rows {
			m := "  "
			if i == sel {
				m = "› "
			}
			fmt.Printf("%s%d. %s\r\n", m, i+1, r)
		}
		fmt.Print("\r\n  enter confirm · esc skip\r\n")
	}
	draw()
	buf := make([]byte, 16)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return
		}
		in := strings.NewReplacer("\x1bOA", "\x1b[A", "\x1bOB", "\x1b[B").Replace(string(buf[:n]))
		sel = max(0, sel-strings.Count(in, "\x1b[A"))
		sel = min(len(rows)-1, sel+strings.Count(in, "\x1b[B"))
		if strings.ContainsAny(in, "\r\n") {
			fmt.Print("\033[2J\033[H")
			fmt.Printf("FAKE HOOKS ANSWERED: %s\r\n", rows[sel])
			return
		}
		draw()
	}
}
