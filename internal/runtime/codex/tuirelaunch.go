package codex

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Stale TUI after an app-server restart (iss_…).
//
// The native TUI (`codex resume --remote unix://<sock>`) talks to the
// app-server directly, not through pyred. When that app-server dies and
// Pyre starts a new one on the same socket (serverlost.go, or the next
// delivery), the TUI shows "Reconnect failed — check the endpoint, then
// relaunch" and stays that way: Codex never retries. Pyre kept driving the
// thread on the new server, so the CLI and MCP said the run was healthy
// while the terminal the owner looked at (web or native attach) was dead.
//
// Each TUI start records the app-server instance ("as<pid>") it was started
// against in <state>/tui-server. Observe compares that with the server Pyre
// is connected to and relaunches a live TUI pane that belongs to an earlier
// instance. The record is a file so a pyred restart in between (an upgrade)
// still finds the mismatch. A dead pane (the human quit) is left for Attach
// to respawn; a TUI started before this record existed is assumed current.

const tuiServerFile = "tui-server"

// instanceNow is the app-server instance hs is connected to ("" = none).
func (hs *homeServer) instanceNow() string {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if hs.client == nil || !hs.client.alive() {
		return ""
	}
	return hs.instance
}

// noteTUI records that rr's TUI was just started against the current
// app-server.
func (a *Adapter) noteTUI(rr *runRef) {
	inst := rr.hs.instanceNow()
	if inst == "" {
		return
	}
	rr.tuiFor.Store(inst)
	_ = os.WriteFile(filepath.Join(rr.stateDir, tuiServerFile), []byte(inst), 0o600)
}

// tuiInstance is the app-server instance rr's TUI was started against: the
// in-memory value, else the state file ("" = unknown).
func (rr *runRef) tuiInstance() string {
	if v, _ := rr.tuiFor.Load().(string); v != "" {
		return v
	}
	b, err := os.ReadFile(filepath.Join(rr.stateDir, tuiServerFile))
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(string(b))
	if v != "" {
		rr.tuiFor.Store(v)
	}
	return v
}

// relaunchStaleTUI relaunches rr's TUI when it belongs to an app-server
// that is gone. Cheap when nothing changed (no tmux call); the relaunch
// runs in the background, one at a time per run.
func (a *Adapter) relaunchStaleTUI(rr *runRef) {
	if a.o.DisableTUI {
		return
	}
	cur := rr.hs.instanceNow()
	if cur == "" {
		return
	}
	was := rr.tuiInstance()
	if was == cur {
		return
	}
	if was == "" { // started before Pyre kept the record: assume current
		rr.tuiFor.Store(cur)
		_ = os.WriteFile(filepath.Join(rr.stateDir, tuiServerFile), []byte(cur), 0o600)
		return
	}
	if !rr.tuiRelaunch.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer rr.tuiRelaunch.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		p, err := a.srv.Pane(ctx, rr.meta.RunID)
		if err != nil || p.Dead {
			return // no TUI, or the human quit it: Attach starts a fresh one
		}
		if c := rr.hs.connected(); c != nil {
			a.ensureTrust(ctx, c, rr)
		}
		if err := a.respawnTUI(ctx, rr); err != nil {
			slog.Warn("codex: relaunch TUI after app-server restart", "run", rr.meta.RunID, "err", err)
			return
		}
		slog.Info("codex: relaunched TUI on the restarted app-server", "run", rr.meta.RunID, "was", was, "now", cur)
	}()
}
