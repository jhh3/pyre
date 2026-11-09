package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/credentials"
	"github.com/jhh3/pyre/internal/memguard"
	"github.com/jhh3/pyre/internal/resumenote"
	"github.com/jhh3/pyre/internal/supervisor"
	"github.com/jhh3/pyre/internal/sysstats"
)

// The low-memory watchdog (memguard, decision I66): the guard's roots are
// the Pyre tmux servers (live runs' sockets plus Config.GuardSockets) and
// each live run's own process; a stopped process is audited
// (node.memory_guard_kill through a node.automation event) and noted to its
// session.

// newMemoryGuard returns the guard, or nil when it is off or unsupported.
func (d *Daemon) newMemoryGuard() *memguard.Guard {
	if d.cfg.MemoryGuard == nil || !memguard.Supported() {
		return nil
	}
	return memguard.New(memguard.Options{Config: *d.cfg.MemoryGuard, Roots: d.guardRoots, OnKill: d.onGuardKill,
		Logf: func(f string, a ...any) { d.cfg.Logger.Warn(fmt.Sprintf(f, a...)) }})
}

func (d *Daemon) memoryGuardLoop(ctx context.Context) {
	g := d.newMemoryGuard()
	if g == nil {
		return
	}
	d.cfg.Logger.Info("memory guard on", "trigger", g.Config().Describe(runtime.GOOS))
	g.Run(ctx)
}

// guardRoots lists the process roots of Pyre's runs on this node.
func (d *Daemon) guardRoots(ctx context.Context) memguard.Roots {
	var r memguard.Roots
	for _, rr := range d.runRoots(ctx) {
		r.Runs = append(r.Runs, memguard.RunRoot{PID: rr.PID, RunID: rr.RunID, SessionID: rr.SessionID})
	}
	sockets := append([]string(nil), d.cfg.GuardSockets...)
	for _, lr := range d.liveRuns() {
		rec, _ := lr.snapshot()
		if rec.Handle != nil && rec.Handle.TmuxSocket != "" && termhostFor(*rec.Handle) == nil {
			sockets = append(sockets, rec.Handle.TmuxSocket)
		}
	}
	seen := map[string]bool{}
	for _, s := range sockets {
		if seen[s] {
			continue
		}
		seen[s] = true
		if _, err := os.Stat(s); err != nil {
			continue
		}
		if pid, err := (supervisor.Tmux{Socket: s, Timeout: 3 * time.Second}).ServerPID(ctx); err == nil && pid > 0 {
			r.Hosts = append(r.Hosts, pid)
		}
	}
	return r
}

// A command line can carry credentials in many shapes; the audit and the
// note get it with the transcript patterns (API keys, JWTs, ...), Pyre's
// own patterns, secret-looking flags and variables (--password X,
// --token=X, API_KEY=X) and URL credentials removed.
var (
	guardFlagRe   = regexp.MustCompile(`(?i)(--?[a-z0-9_.-]*(?:token|secret|passw(?:or)?d|pass|api[_-]?key|auth|credential|cookie)[a-z0-9_.-]*(?:=|\s+))\S+`)
	guardEnvRe    = regexp.MustCompile(`(?i)\b([a-z0-9_]*(?:token|secret|password|passwd|api_?key|credential)[a-z0-9_]*=)\S+`)
	guardURLCreds = regexp.MustCompile(`(://[^/\s:@]+:)[^/\s@]+@`)
)

func guardRedact(s string) string {
	s = redact(credentials.RedactTranscript(s))
	s = guardFlagRe.ReplaceAllString(s, "${1}[redacted]")
	s = guardEnvRe.ReplaceAllString(s, "${1}[redacted]")
	return guardURLCreds.ReplaceAllString(s, "${1}[redacted]@")
}

// guardDetail is the audit text of one guard action.
func guardDetail(k memguard.Kill) string {
	what := guardRedact(strings.TrimSpace(k.Command))
	if what == "" {
		what = k.Name
	}
	if len(what) > 160 {
		what = what[:160] + "…"
	}
	s := fmt.Sprintf("pid %d `%s` rss %s", k.PID, what, sysstats.HumanMB(float64(k.RSS>>20)))
	if k.Swap > 0 {
		s += " swap " + sysstats.HumanMB(float64(k.Swap>>20))
	}
	return s + "; " + k.Outcome + "; node " + k.Node
}

// onGuardKill reports a resolved guard action: log, audit and the note.
func (d *Daemon) onGuardKill(k memguard.Kill) {
	detail := guardDetail(k)
	d.cfg.Logger.Warn("memory guard stopped a process", "pid", k.PID, "name", k.Name, "session", k.SessionID, "run", k.RunID, "detail", detail)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a := contracts.NodeAutomation{RunID: k.RunID, SessionID: k.SessionID, Action: contracts.AutomationMemoryGuardKill, Detail: detail}
	if payload, err := json.Marshal(a); err == nil {
		ev := contracts.Event{Kind: contracts.EvNodeAutomation, Object: contracts.ObjectRef{Kind: contracts.KindRun, ID: k.RunID},
			SessionID: k.SessionID, PayloadVersion: 1, Payload: payload, OccurredAt: d.now().UTC()}
		if _, err := d.rep.Enqueue(ctx, contracts.ReportEvent, ev); err != nil {
			d.cfg.Logger.Info("memory guard: audit report not queued", "pid", k.PID, "err", err)
		}
	}
	if k.SessionID == "" || strings.HasPrefix(k.Outcome, "signal failed") {
		return
	}
	go d.sendGuardNote(k)
}

// sendGuardNote tells the session, as the session itself (its stored
// token, like the crash-resume note), through the local API (the outbox
// holds it while the hub is unreachable). The message ID is stable per
// process, so a retried send is one message.
func (d *Daemon) sendGuardNote(k memguard.Kill) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tok, ok, err := d.st.tokenForSession(ctx, k.SessionID)
	if err != nil || !ok {
		d.cfg.Logger.Info("memory guard: no session credential on this node; no note sent", "session", k.SessionID)
		return
	}
	node := d.cfg.NodeName
	if node == "" {
		node, _ = os.Hostname()
	}
	k.Command = guardRedact(k.Command)
	key := resumenote.IDPart(k.SessionID) + "-" + strconv.Itoa(k.PID) + "-" + strconv.FormatInt(k.Chosen.Unix(), 36)
	self := contracts.Principal{Kind: contracts.PrincipalSession, ID: k.SessionID}
	sctx := WithBearer(contracts.WithPrincipal(ctx, self), tok)
	req := contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_memguard-" + key},
		MessageID: "msg_memguard-" + key, Recipient: contracts.Address{Kind: contracts.AddrSession, ID: k.SessionID},
		Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: memguard.Note(k, node, runtime.GOOS)}}
	if _, err := d.LocalAPI().SendMessage(sctx, req); err != nil {
		d.noteErr("memory guard note to session %s: %v", k.SessionID, err)
	}
}

// memoryProtection describes the node's memory protection for status
// (NodeResources.MemoryProtection).
func (d *Daemon) memoryProtection(ctx context.Context) string {
	var parts []string
	if l := d.cfg.RunScopes; l != nil && runtime.GOOS == "linux" {
		switch dec, ok := supervisor.CachedRunScope(*l); {
		case !ok:
			parts = append(parts, "per-run scopes not decided yet")
		case dec.Use:
			parts = append(parts, "per-run scopes "+dec.Scope.Describe())
		default:
			parts = append(parts, "no per-run scopes ("+dec.Reason+")")
		}
	}
	switch {
	case d.cfg.MemoryGuard == nil:
		parts = append(parts, "guard off")
	case !memguard.Supported():
		parts = append(parts, "guard unsupported on "+runtime.GOOS)
	default:
		parts = append(parts, "guard on ("+d.cfg.MemoryGuard.Describe(runtime.GOOS)+")")
	}
	return strings.Join(parts, "; ")
}
