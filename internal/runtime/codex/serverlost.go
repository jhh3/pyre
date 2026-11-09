package codex

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Lost app-server (iss_…).
//
// A run's turns and background commands live in the app-server shared by
// its CODEX_HOME. When that process dies, the active turn ends without
// turn/completed and the background commands end without item/completed,
// so no wake can fire, and nothing restarted the server until the next
// delivery. On server (Oct 2026) the kernel OOM-killed it one second after a
// turn's code-mode script yielded a watcher command: the turn never
// finished, the watcher died with it, and the session sat idle for six
// minutes until its parent wrote to it.
//
// Observe finds the server gone while a run's thread had a turn active or
// commands running and restarts it (reviveLost). ensure, seeing a new
// app-server process where the one it was connected to ran, records what
// each such thread lost, and the record goes out as a wake turn
// (bgwake.go) once the thread is idle on the new server. Not reported:
// stopped and archived runs (as for wakes), threads Pyre was closing, and
// a loss pyred did not see (it restarted meanwhile: its memory of what ran
// is gone).

// serverLoss is what one thread lost with the app-server.
type serverLoss struct {
	At       time.Time // when the connection to the dead app-server failed
	Turn     bool      // a turn was running
	Cmds     []string  // the commands that were running
	attempts int
}

// lostMaxCmds commands are listed in a notice; the rest are counted.
const lostMaxCmds = 8

// noteServerLost records what ts lost with an app-server that died at at.
// Called with ts.mu held, before the new connection resets the thread.
func (ts *threadState) noteServerLost(at time.Time) {
	if ts.closing || (ts.status.Type != "active" && len(ts.runningCmds) == 0) {
		return
	}
	l := &serverLoss{At: at, Turn: ts.status.Type == "active"}
	for _, c := range ts.runningCmds {
		l.Cmds = append(l.Cmds, c)
	}
	slices.Sort(l.Cmds)
	ts.lost = l
	ts.wakes, ts.inTurn = nil, nil // their turn and processes are gone with it
	ts.agentWakes, ts.agentHeld = nil, nil
}

// reviveLost restarts the run's dead app-server in the background (once
// at a time) when the run's thread had work in it. It reports whether a
// restart is under way.
func (a *Adapter) reviveLost(rr *runRef) bool {
	rr.ts.mu.Lock()
	busy := !rr.ts.closing && (rr.ts.status.Type == "active" || len(rr.ts.runningCmds) > 0)
	rr.ts.mu.Unlock()
	rr.hs.mu.Lock()
	seen := rr.hs.instance != "" // this pyred was connected to the one that died
	rr.hs.mu.Unlock()
	if !busy || !seen || stopped(rr.stateDir) || !rr.reviving.CompareAndSwap(false, true) {
		return rr.reviving.Load()
	}
	go func() {
		defer rr.reviving.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if f := a.o.WakeAllowed; f != nil && !f(ctx, rr.meta.SessionID) {
			return
		}
		_, _ = a.client(ctx, rr, true)
	}()
	return true
}

// wakeText is a wake turn's input: the lost app-server first, then the
// background completions.
func wakeText(lost *serverLoss, batch []bgDone, agents []agentDone) string {
	var parts []string
	if lost != nil {
		parts = append(parts, lostNotice(lost))
	}
	if len(batch) > 0 {
		parts = append(parts, wakeNotice(batch))
	}
	if len(agents) > 0 {
		parts = append(parts, agentNotice(agents))
	}
	return strings.Join(parts, "\n\n")
}

func lostNotice(l *serverLoss) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[pyre] The Codex app-server running this session exited at %s and Pyre restarted it.", l.At.Local().Format("15:04:05 MST"))
	if l.Turn {
		b.WriteString(" Your turn was cut off: its last tool call may not have finished and its result never reached you.")
	}
	switch len(l.Cmds) {
	case 0:
	case 1:
		fmt.Fprintf(&b, " This command was still running and Codex lost it: `%s`.", oneLine(l.Cmds[0], 300))
	default:
		fmt.Fprintf(&b, " These %d commands were still running and Codex lost them:", len(l.Cmds))
		for i, c := range l.Cmds {
			if i == lostMaxCmds {
				fmt.Fprintf(&b, "\n(and %d more)", len(l.Cmds)-lostMaxCmds)
				break
			}
			fmt.Fprintf(&b, "\n%d. `%s`", i+1, oneLine(c, 300))
		}
		b.WriteString("\n")
	}
	if len(l.Cmds) > 0 {
		if len(l.Cmds) == 1 {
			b.WriteString(" ")
		}
		if parentDeathSIGTERM {
			b.WriteString("Each command's top process got SIGTERM; its child processes may still be running.")
		} else {
			b.WriteString("Some of their processes may still be running.")
		}
	}
	b.WriteString(" Check what the work got done (its output files, units, logs), then redo what is still needed. Pyre cannot tell why the app-server exited; an out-of-memory kill is the usual cause.")
	return b.String()
}
