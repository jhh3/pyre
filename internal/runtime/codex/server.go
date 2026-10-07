package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// homeServer supervises the single Pyre-owned `codex app-server` for one
// CODEX_HOME binding. It runs in the node's isolated tmux server (so it
// survives daemon restarts) and listens on <dir>/codex.sock in a 0700 dir.
// Runs referencing the home are recorded as files in <dir>/refs/ so the
// refcount survives restarts; the server is stopped when the last ref goes.
type homeServer struct {
	a       *Adapter
	home    string // CODEX_HOME ("" = the node's default home)
	exe     string // codex executable ("" = the adapter's default)
	key     string
	dir     string
	sock    string
	session string // tmux session hosting the app-server

	startMu  sync.Mutex // serializes ensure/stop
	mu       sync.Mutex // guards the fields below; never held across RPCs
	client   *rpcClient
	instance string
	threads  map[string]*threadState // thread id -> state (run threads)
	parent   map[string]string       // subagent thread id -> parent thread id
}

func homeKey(home string) string {
	if home == "" {
		return "default"
	}
	sum := sha256.Sum256([]byte(home))
	return hex.EncodeToString(sum[:6])
}

// serverKey keys one app-server: a CODEX_HOME plus the codex executable it
// runs. The adapter's default executable keeps the plain home key (servers
// started before executables were keyed stay reachable).
func serverKey(home, exe string) string {
	if exe == "" {
		return homeKey(home)
	}
	sum := sha256.Sum256([]byte(home + "\x00" + exe))
	return "x" + hex.EncodeToString(sum[:6])
}

// serverExe normalizes a run's pinned codex executable: "" when it is the
// adapter's default binary (same file), else its absolute path. A run pinned
// to a different version gets its own app-server on the same CODEX_HOME, so
// activating a new Codex version never moves a running thread to another
// binary, and a new run never lands on the old version's shared server.
func (a *Adapter) serverExe(exe string) string {
	if exe == "" {
		return ""
	}
	def := a.codexBin()
	if exe == def {
		return ""
	}
	re, err1 := filepath.EvalSymlinks(exe)
	rd, err2 := filepath.EvalSymlinks(def)
	if err1 == nil && err2 == nil && re == rd {
		return ""
	}
	return exe
}

func (a *Adapter) homeServer(home string) (*homeServer, error) {
	return a.homeServerFor(home, "")
}

func (a *Adapter) homeServerFor(home, exe string) (*homeServer, error) {
	exe = a.serverExe(exe)
	key := serverKey(home, exe)
	a.mu.Lock()
	defer a.mu.Unlock()
	if hs := a.homes[key]; hs != nil {
		return hs, nil
	}
	dir := filepath.Join(a.o.StateRoot, "codex", key)
	sock := filepath.Join(dir, "codex.sock")
	if err := tmux.ValidateSocketPath(sock); err != nil {
		return nil, err
	}
	hs := &homeServer{a: a, home: home, exe: exe, key: key, dir: dir, sock: sock, session: "codex-as-" + key,
		threads: map[string]*threadState{}, parent: map[string]string{}}
	a.homes[key] = hs
	return hs, nil
}

// bin is the codex executable this server (and its runs' TUIs) use.
func (hs *homeServer) bin() string {
	if hs.exe != "" {
		return hs.exe
	}
	return hs.a.codexBin()
}

// homeFile records which CODEX_HOME a server directory belongs to, so
// HomeBusy finds every server on a home (one per executable) after a
// daemon restart.
const homeFile = "home"

func (hs *homeServer) refsDir() string { return filepath.Join(hs.dir, "refs") }

var refNameRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,200}$`)

func (hs *homeServer) addRef(name string) error {
	if !refNameRe.MatchString(name) {
		return fmt.Errorf("codex: bad ref name %q", name)
	}
	if err := tmux.PrepareDir(hs.refsDir()); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(hs.refsDir(), name), []byte(time.Now().UTC().Format(time.RFC3339)), 0o600)
}

// dropRef removes a ref and returns the number remaining.
func (hs *homeServer) dropRef(name string) int {
	_ = os.Remove(filepath.Join(hs.refsDir(), name))
	ents, _ := os.ReadDir(hs.refsDir())
	return len(ents)
}

func (hs *homeServer) argv() []string {
	if hs.a.o.AppServerCommand != nil {
		return hs.a.o.AppServerCommand(hs.home, hs.sock)
	}
	argv := []string{hs.bin(), "app-server", "--listen", "unix://" + hs.sock}
	return append(argv, hs.a.o.AppServerArgs...)
}

func (hs *homeServer) env() map[string]string {
	env := map[string]string{}
	for k, v := range hs.a.o.AppServerEnv {
		env[k] = v
	}
	if hs.home != "" {
		env["CODEX_HOME"] = hs.home
	}
	return env
}

// running reports whether the app-server pane is alive.
func (hs *homeServer) running(ctx context.Context) (bool, int) {
	p, err := hs.a.srv.Pane(ctx, hs.session)
	if err != nil || p.Dead {
		return false, 0
	}
	return true, p.PID
}

// ensure starts the app-server if needed and returns a connected client.
func (hs *homeServer) ensure(ctx context.Context, unset []string) (*rpcClient, error) {
	hs.startMu.Lock()
	defer hs.startMu.Unlock()
	hs.mu.Lock()
	old := hs.client
	hs.mu.Unlock()
	if old != nil && old.alive() {
		if ok, _ := hs.running(ctx); ok {
			return old, nil
		}
	}
	if old != nil {
		old.close()
		hs.mu.Lock()
		hs.client = nil
		hs.mu.Unlock()
	}
	ok, _ := hs.running(ctx)
	if !ok {
		if err := tmux.PrepareDir(hs.dir); err != nil {
			return nil, err
		}
		_ = os.WriteFile(filepath.Join(hs.dir, homeFile), []byte(hs.home), 0o600)
		_ = hs.a.srv.KillSession(ctx, hs.session) // dead pane, if any
		if st, err := os.Lstat(hs.sock); err == nil && st.Mode()&os.ModeType != 0 {
			_ = os.Remove(hs.sock) // stale socket/symlink from a crashed instance
		}
		spec := tmux.SessionSpec{Name: hs.session, Cwd: hs.dir, Env: hs.env(), Unset: unset, Argv: hs.argv(), Width: 200, Height: 50}
		if err := hs.a.srv.NewSession(ctx, spec); err != nil {
			return nil, fmt.Errorf("start codex app-server: %w", err)
		}
	}
	var c *rpcClient
	var err error
	deadline := time.Now().Add(hs.a.o.StartWait)
	for {
		c, err = dialRPC(ctx, hs.sock, hs.onNotify, hs.onRequest)
		if err == nil {
			break
		}
		if alive, _ := hs.running(ctx); !alive || time.Now().After(deadline) || ctx.Err() != nil {
			screen, _ := hs.a.srv.CapturePane(ctx, hs.session, tmux.CaptureOptions{Join: true})
			return nil, fmt.Errorf("connect codex app-server %s: %v (pane: %s)", hs.sock, err, oneLine(screen, 300))
		}
		time.Sleep(200 * time.Millisecond)
	}
	ictx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ip := initializeParams{ClientInfo: clientInfo{Name: "pyre", Version: "wp07"},
		Capabilities: initializeCapabilities{ExperimentalAPI: true, OptOutNotificationMethods: []string{
			"item/agentMessage/delta", "item/reasoning/summaryTextDelta", "item/reasoning/textDelta",
			"item/reasoning/summaryPartAdded", "item/commandExecution/outputDelta", "item/plan/delta",
			"item/fileChange/outputDelta", "thread/tokenUsage/updated",
		}}}
	if err := c.call(ictx, "initialize", ip, nil); err != nil {
		c.close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if err := c.notify(ictx, "initialized", nil); err != nil {
		c.close()
		return nil, err
	}
	_, pid := hs.running(ctx)
	hs.mu.Lock()
	hs.instance = "as" + strconv.Itoa(pid)
	hs.client = c
	var known []*threadState
	for _, ts := range hs.threads {
		known = append(known, ts)
	}
	hs.mu.Unlock()
	// Re-subscribe threads known from before a reconnect (idle threads
	// unload; pending approvals are re-sent on resume).
	for _, ts := range known {
		var r threadResponse
		ts.mu.Lock()
		cfg := ts.config
		ts.mu.Unlock()
		if err := c.call(ictx, "thread/resume", threadResumeParams{ThreadID: ts.threadID, ExcludeTurns: true, Config: cfg}, &r); err == nil {
			ts.mu.Lock()
			ts.setStatus(r.Thread.Status, time.Now())
			ts.mu.Unlock()
		}
	}
	return c, nil
}

// connected returns the current client if alive (no start).
func (hs *homeServer) connected() *rpcClient {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if hs.client != nil && hs.client.alive() {
		return hs.client
	}
	return nil
}

func (hs *homeServer) register(ts *threadState) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	hs.threads[ts.threadID] = ts
}

func (hs *homeServer) unregister(threadID string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	delete(hs.threads, threadID)
}

// touch advances the change stamp of the run whose thread (or subagent
// thread) a notification or request names.
func (hs *homeServer) touch(params json.RawMessage) {
	var p struct {
		ThreadID string `json:"threadId"`
	}
	if json.Unmarshal(params, &p) != nil || p.ThreadID == "" {
		return
	}
	if ts, _ := hs.stateFor(p.ThreadID); ts != nil {
		ts.changes.Add(1)
	}
}

// stateFor routes a notification to its run thread (directly or through a
// subagent's parent).
func (hs *homeServer) stateFor(threadID string) (*threadState, bool) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if ts := hs.threads[threadID]; ts != nil {
		return ts, false
	}
	if p := hs.parent[threadID]; p != "" {
		if ts := hs.threads[p]; ts != nil {
			return ts, true
		}
	}
	return nil, false
}

func (hs *homeServer) onNotify(method string, params json.RawMessage) {
	now := time.Now()
	hs.touch(params)
	switch method {
	case "account/rateLimits/updated":
		if f := hs.a.o.OnRateLimits; f != nil {
			raw := append(json.RawMessage(nil), params...)
			go f(hs.home, raw)
		}
	case "thread/started":
		var n threadStartedNotification
		if json.Unmarshal(params, &n) == nil && n.Thread.ParentThreadID != "" {
			hs.mu.Lock()
			hs.parent[n.Thread.ID] = n.Thread.ParentThreadID
			hs.mu.Unlock()
		}
	case "thread/status/changed":
		var n statusNotification
		if json.Unmarshal(params, &n) != nil {
			return
		}
		if ts, child := hs.stateFor(n.ThreadID); ts != nil && !child {
			ts.mu.Lock()
			ts.setStatus(n.Status, now)
			ts.mu.Unlock()
		}
	case "thread/closed":
		var n threadIDParams
		if json.Unmarshal(params, &n) == nil {
			if ts, child := hs.stateFor(n.ThreadID); ts != nil && !child {
				ts.mu.Lock()
				ts.setStatus(threadStatus{Type: "notLoaded"}, now)
				ts.mu.Unlock()
			}
		}
	case "turn/started":
		var n turnNotification
		if json.Unmarshal(params, &n) == nil {
			if ts, child := hs.stateFor(n.ThreadID); ts != nil && !child {
				ts.mu.Lock()
				ts.activeTurn = n.Turn.ID
				if ts.status.Type != "active" {
					ts.setStatus(threadStatus{Type: "active"}, now)
					ts.activeTurn = n.Turn.ID
				}
				ts.mu.Unlock()
			}
		}
	case "turn/completed":
		var n turnNotification
		if json.Unmarshal(params, &n) == nil {
			if ts, child := hs.stateFor(n.ThreadID); ts != nil {
				ts.mu.Lock()
				if child {
					for _, s := range ts.subagents {
						if s.ThreadID == n.ThreadID && s.End.IsZero() && n.Turn.Status != "inProgress" {
							s.End = now
						}
					}
				} else {
					ts.onTurnCompleted(n.Turn, now)
				}
				ts.mu.Unlock()
			}
		}
	case "item/started", "item/completed":
		var n itemNotification
		if json.Unmarshal(params, &n) != nil {
			return
		}
		var it item
		if json.Unmarshal(n.Item, &it) != nil {
			return
		}
		if ts, child := hs.stateFor(n.ThreadID); ts != nil && !child {
			ts.mu.Lock()
			ts.onItem(it, n.TurnID, method == "item/started", now)
			if it.Type == "subAgentActivity" && it.AgentThreadID != "" {
				hs.mu.Lock()
				hs.parent[it.AgentThreadID] = ts.threadID
				hs.mu.Unlock()
			}
			ts.mu.Unlock()
		}
	case "serverRequest/resolved":
		var n resolvedNotification
		if json.Unmarshal(params, &n) != nil {
			return
		}
		if ts, _ := hs.stateFor(n.ThreadID); ts != nil {
			ts.mu.Lock()
			for _, pa := range ts.approvals {
				if string(pa.rpcID) == string(n.RequestID) && pa.approval.State == "pending" {
					pa.approval.State = "resolved"
					res := &approvalResolution{at: now}
					if !pa.submitted.IsZero() {
						res.source, res.effect, res.option = "pyre", "effective", pa.option
					} else {
						res.source = "native"
					}
					res.apply(&pa.approval)
				}
			}
			ts.mu.Unlock()
		}
	case "error":
		var n errorNotification
		if json.Unmarshal(params, &n) == nil && !n.WillRetry {
			if ts, child := hs.stateFor(n.ThreadID); ts != nil && !child {
				ts.mu.Lock()
				if le := limitFromError(n.Error, now); le != nil {
					ts.limit = le
				}
				ts.mu.Unlock()
			}
		}
	}
}

var approvalMethods = map[string]bool{
	"item/commandExecution/requestApproval": true,
	"item/fileChange/requestApproval":       true,
	"item/permissions/requestApproval":      true,
	"item/tool/requestUserInput":            true,
}

// onRequest records approval/question requests. Other server requests are
// left unanswered so a connected native TUI can handle them.
func (hs *homeServer) onRequest(id json.RawMessage, method string, params json.RawMessage) {
	if !approvalMethods[method] {
		return
	}
	var p approvalParams
	if json.Unmarshal(params, &p) != nil {
		return
	}
	ts, _ := hs.stateFor(p.ThreadID)
	if ts == nil {
		return
	}
	ts.changes.Add(1)
	hs.mu.Lock()
	inst := hs.instance
	hs.mu.Unlock()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	pa := newPending(ts, inst, id, method, p, time.Now())
	rid := pa.approval.RequestID
	if _, ok := ts.approvals[rid]; !ok {
		ts.order = append(ts.order, rid)
	}
	ts.approvals[rid] = pa
}

// stop terminates the app-server (SIGTERM drains; then kill-session).
func (hs *homeServer) stop(ctx context.Context) {
	hs.startMu.Lock()
	defer hs.startMu.Unlock()
	hs.mu.Lock()
	if hs.client != nil {
		hs.client.close()
		hs.client = nil
	}
	hs.mu.Unlock()
	// The npm launcher (node) and the native codex child share the pane's
	// process group: SIGTERM drains, SIGKILL after 5s.
	_ = hs.a.srv.TerminateSession(ctx, hs.session, 5*time.Second)
}

var errNoServer = errors.New("codex app-server not running")
