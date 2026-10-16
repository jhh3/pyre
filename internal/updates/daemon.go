package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/osutil"
)

// Daemon self-update (the in-product `pyre update ... --tool pyred` path).
//
// The service manager (launchd/systemd unit written by install.sh) always
// starts the installed pyred, the "launcher" (e.g.
// ~/.local/share/pyre/current/bin/pyred). Activating a staged pyred switches
// <state>/tools/pyred/current and records the launcher version it was
// activated over (versionRecord.Base) plus a handoff record; the daemon then
// restarts itself by exec'ing the launcher, which hands off (exec, same PID)
// to the active managed pyred. Runs live in Pyre-owned tmux servers and
// Codex app-servers outside pyred and are recovered by the new daemon.
//
// Safety:
//   - First boot of a newly activated version is "booting" until that daemon
//     reports healthy (connected to its hub, or started when standalone).
//     If it does not become healthy within the handoff timeout it marks the
//     handoff failed and execs the launcher; if it crashes, the service
//     manager restarts the launcher. Either way the launcher finds a handoff
//     that never became healthy and rolls back: the version is marked failed
//     and the previous managed version (or the launcher itself) runs.
//   - A managed pyred is used only over the launcher version it was
//     activated over. When install.sh/upgrade.sh installs a different
//     launcher, the managed activation is superseded (no silent downgrade).

// Handoff states.
const (
	HandoffPending    = "pending"     // activated, not booted yet
	HandoffBooting    = "booting"     // launcher exec'd it; health not yet reported
	HandoffHealthy    = "healthy"     // reported healthy once; later boots exec it directly
	HandoffFailed     = "failed"      // did not become healthy; the launcher rolls back
	HandoffRolledBack = "rolled_back" // the launcher reverted it
)

// Handoff is <state>/tools/pyred/handoff.json.
type Handoff struct {
	Version   string    `json:"version"`
	Base      string    `json:"base"`               // launcher version the activation was made over
	Previous  string    `json:"previous,omitempty"` // managed version to revert to ("" = the launcher)
	State     string    `json:"state"`
	Attempts  int       `json:"attempts"`
	Detail    string    `json:"detail,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// BootDecision tells the launcher what to run.
type BootDecision struct {
	// Exec is the managed pyred to exec; "" = the launcher runs itself.
	Exec    string
	Version string
	// Watch is set when the exec'd daemon must prove health (first boot).
	Watch bool
	// Notes are log lines describing the decision (superseded or rolled-back
	// activations).
	Notes []string
	// Result is the update.result to report once the daemon is connected
	// (an automatic rollback), or nil.
	Result *UpdateResult
}

func (m *Manager) handoffPath() string { return filepath.Join(m.toolDir("pyred"), "handoff.json") }

// ReadHandoff returns the handoff record, or nil.
func (m *Manager) ReadHandoff() *Handoff {
	b, err := os.ReadFile(m.handoffPath())
	if err != nil {
		return nil
	}
	var h Handoff
	if json.Unmarshal(b, &h) != nil || h.Version == "" {
		return nil
	}
	return &h
}

func (m *Manager) writeHandoff(h Handoff) error {
	h.UpdatedAt = m.opt.Clock.Now()
	b, _ := json.MarshalIndent(h, "", "  ")
	return writeFileAtomic(m.handoffPath(), append(b, '\n'), 0o600)
}

func (m *Manager) clearHandoff() { _ = os.Remove(m.handoffPath()) }

// MarkDaemonHealthy records that the running managed pyred version became
// healthy (later boots exec it without a watchdog).
func (m *Manager) MarkDaemonHealthy(version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.ReadHandoff()
	if h == nil || h.Version != version {
		return nil
	}
	h.State, h.Detail = HandoffHealthy, ""
	return m.writeHandoff(*h)
}

// MarkDaemonFailed records that the running managed pyred version did not
// become healthy; the launcher rolls it back on its next start.
func (m *Manager) MarkDaemonFailed(version, detail string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.ReadHandoff()
	if h == nil || h.Version != version {
		return nil
	}
	h.State, h.Detail = HandoffFailed, detail
	return m.writeHandoff(*h)
}

// ActivePyred returns the active managed pyred version ("" = none).
func (m *Manager) ActivePyred() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, err := m.load()
	if err != nil {
		return ""
	}
	if r := activeRec(st, "pyred"); r != nil {
		return r.Version
	}
	return ""
}

// DaemonBoot is called by the launcher pyred (version self) before it starts
// serving. It decides whether to hand off to the active managed pyred,
// rolling back a handoff that never became healthy and superseding an
// activation made over a different launcher.
func (m *Manager) DaemonBoot(self string) (BootDecision, error) {
	var dec BootDecision
	for range 3 { // a rollback may hand off to the previous managed version
		m.mu.Lock()
		st, err := m.load()
		m.mu.Unlock()
		if err != nil {
			return dec, err
		}
		act := activeRec(st, "pyred")
		if act == nil || act.Version == self {
			return dec, nil
		}
		if act.Base != self {
			v, base := act.Version, act.Base
			if base == "" {
				base = "an unknown launcher"
			}
			note := fmt.Sprintf("managed pyred %s was activated over %s, but this launcher is %s (installed or upgraded since): not handed off; %s is now previous", v, base, self, v)
			_ = m.mutate(func(st *stateFile) error {
				if _, r := findRec(st, "pyred", v); r != nil && r.State == StateActive {
					r.State = StatePrevious
				}
				return nil
			})
			_ = m.revertCurrent("pyred", "")
			m.clearHandoff()
			dec.Notes = append(dec.Notes, note)
			return dec, nil
		}
		exe := m.exePath(*act)
		if st, err := os.Stat(exe); err != nil || !osutil.IsExecutable(exe, st) {
			dec.Notes = append(dec.Notes, fmt.Sprintf("managed pyred %s has no executable at %s; rolling back", act.Version, exe))
			m.rollbackHandoff(act.Version, "executable missing", self, &dec)
			continue
		}
		h := m.ReadHandoff()
		if h != nil && h.Version == act.Version {
			switch h.State {
			case HandoffHealthy:
				dec.Exec, dec.Version = exe, act.Version
				return dec, nil
			case HandoffBooting, HandoffFailed:
				detail := h.Detail
				if detail == "" {
					detail = "the daemon exited or was restarted before it reported healthy"
				}
				dec.Notes = append(dec.Notes, fmt.Sprintf("managed pyred %s did not become healthy (%s); rolling back", act.Version, detail))
				m.rollbackHandoff(act.Version, detail, self, &dec)
				continue
			}
		}
		nh := Handoff{Version: act.Version, Base: self, State: HandoffBooting, Attempts: 1}
		if h != nil && h.Version == act.Version {
			nh.Previous, nh.Attempts = h.Previous, h.Attempts+1
		}
		if err := m.writeHandoff(nh); err != nil {
			return dec, err
		}
		dec.Exec, dec.Version, dec.Watch = exe, act.Version, true
		return dec, nil
	}
	return dec, nil
}

// rollbackHandoff reverts a managed pyred that did not become healthy to the
// handoff's previous managed version (or to none: the launcher).
func (m *Manager) rollbackHandoff(version, detail, self string, dec *BootDecision) {
	prev := ""
	if h := m.ReadHandoff(); h != nil && h.Version == version {
		prev = h.Previous
	}
	_ = m.mutate(func(st *stateFile) error {
		ok := false
		if prev != "" {
			if _, r := findRec(st, "pyred", prev); r != nil && r.State == StatePrevious {
				ok = true
			}
		}
		if !ok {
			prev = ""
		}
		for i := range st.Tools["pyred"] {
			r := &st.Tools["pyred"][i]
			switch {
			case r.Version == version:
				r.State, r.Health = StateFailed, "failed: "+detail
			case r.Version == prev:
				r.State = StateActive
			}
		}
		return nil
	})
	_ = m.revertCurrent("pyred", prev)
	_ = os.RemoveAll(m.versionDir("pyred", version)) // never ran healthy; not a rollback target
	if prev != "" {
		_ = m.writeHandoff(Handoff{Version: prev, Base: self, State: HandoffPending})
	} else {
		m.clearHandoff()
	}
	dec.Result = &UpdateResult{Tool: "pyred", Version: version, Outcome: "failed_reverted", Health: "failed: " + detail,
		RevertedTo: prev, Detail: "daemon handoff did not become healthy; reverted to " + orLauncher(prev)}
	// The launcher may exec another pyred right away: persist the result for
	// whichever daemon wires its reporter next (FlushPendingResults).
	m.appendPending(*dec.Result)
}

func (m *Manager) pendingPath() string {
	return filepath.Join(m.toolDir("pyred"), "pending-results.json")
}

func (m *Manager) appendPending(r UpdateResult) {
	var list []UpdateResult
	if b, err := os.ReadFile(m.pendingPath()); err == nil {
		_ = json.Unmarshal(b, &list)
	}
	list = append(list, r)
	b, _ := json.Marshal(list)
	_ = writeFileAtomic(m.pendingPath(), b, 0o600)
}

// FlushPendingResults reports update.results recorded at boot (automatic
// pyred rollbacks made by the launcher before any reporter existed) and
// clears them. The reporter is the node's durable outbox.
func (m *Manager) FlushPendingResults(ctx context.Context) int {
	b, err := os.ReadFile(m.pendingPath())
	if err != nil {
		return 0
	}
	var list []UpdateResult
	_ = json.Unmarshal(b, &list)
	for _, r := range list {
		m.report(ctx, contracts.EvUpdateResult, r)
	}
	_ = os.Remove(m.pendingPath())
	return len(list)
}

func orLauncher(v string) string {
	if v == "" {
		return "the installed pyred (launcher)"
	}
	return v
}
