package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// ProtocolRange is an inclusive range of node protocol versions.
type ProtocolRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

func (r ProtocolRange) String() string { return fmt.Sprintf("%d..%d", r.Min, r.Max) }

// LocalProtocol is this build's range.
func LocalProtocol() ProtocolRange {
	return ProtocolRange{Min: contracts.MinProtocolVersion, Max: contracts.ProtocolVersion}
}

// CheckProtocolCompat refuses a pyred whose protocol range does not overlap
// the hub's, with an actionable recovery step.
func CheckProtocolCompat(pyredVersion string, node, hub ProtocolRange) error {
	if node.Min > node.Max || hub.Min > hub.Max || node.Max <= 0 || hub.Max <= 0 {
		return contracts.Errorf(contracts.ErrInvalid, "invalid protocol range (node %s, hub %s)", node, hub)
	}
	if node.Min <= hub.Max && hub.Min <= node.Max {
		return nil
	}
	var action string
	if node.Min > hub.Max {
		action = fmt.Sprintf("Upgrade the hub to a build that supports protocol %d before activating this pyred, or keep the current pyred.", node.Min)
	} else {
		action = fmt.Sprintf("Stage a pyred that supports protocol %d (or newer), or roll the hub back.", hub.Min)
	}
	return contracts.Errorf(contracts.ErrProtocolMismatch, "pyred %s speaks protocol %s; hub supports %s. %s Not activated; the running daemon is unchanged.", pyredVersion, node, hub, action).
		WithDetail("hub_protocol", hub).WithDetail("node_protocol", node)
}

// pyredVersionInfo is the expected output of `pyred version --json`
// (requested from WP06).
type pyredVersionInfo struct {
	Version     string `json:"version"`
	Protocol    int    `json:"protocol"`
	MinProtocol int    `json:"min_protocol"`
}

func (m *Manager) defaultProtocolProbe(ctx context.Context, exe string) (ProtocolRange, string, error) {
	hctx, cancel := context.WithTimeout(ctx, m.opt.HealthTimeout)
	defer cancel()
	out, stderr, err := m.opt.Runner.Run(hctx, "", healthEnv, exe, "version", "--json")
	if err != nil {
		return ProtocolRange{}, "", fmt.Errorf("%s version --json: %v %s", exe, err, trim(stderr))
	}
	var v pyredVersionInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &v); err != nil || v.Protocol == 0 {
		return ProtocolRange{}, "", fmt.Errorf("%s version --json: no protocol field", exe)
	}
	if v.MinProtocol == 0 {
		v.MinProtocol = v.Protocol
	}
	return ProtocolRange{Min: v.MinProtocol, Max: v.Protocol}, v.Version, nil
}

// DrainPlan describes the daemon self-update sequence. The actual swap is
// performed by the daemon/service packaging (WP06/WP20); this package stages,
// verifies and switches the pointer.
type DrainPlan struct {
	Tool       string   `json:"tool"`
	From       string   `json:"from"`
	To         string   `json:"to"`
	Steps      []string `json:"steps"`
	Rollback   []string `json:"rollback"`
	LiveRuns   []string `json:"live_runs"` // survive the swap (tmux/app-server are not children of pyred)
	Compatible bool     `json:"compatible"`
}

// PlanDaemonSwap returns the drain/restart plan for activating a staged pyred.
func (m *Manager) PlanDaemonSwap(ctx context.Context, to string) (DrainPlan, error) {
	m.mu.Lock()
	st, err := m.load()
	m.mu.Unlock()
	if err != nil {
		return DrainPlan{}, err
	}
	_, rec := findRec(st, "pyred", to)
	if rec == nil || rec.State != StateStaged {
		return DrainPlan{}, contracts.Errorf(contracts.ErrNotFound, "pyred %s is not staged", to)
	}
	from := ""
	if a := activeRec(st, "pyred"); a != nil {
		from = a.Version
	}
	plan := DrainPlan{Tool: "pyred", From: from, To: to, Compatible: true}
	if m.opt.HubProtocol != nil {
		if hub, ok := m.opt.HubProtocol(); ok {
			rng, v, err := m.opt.ProtocolProbe(ctx, m.exePath(*rec))
			if err != nil {
				return DrainPlan{}, contracts.Errorf(contracts.ErrConflict, "cannot read protocol range of pyred %s: %v", to, err)
			}
			if err := CheckProtocolCompat(orDefault(v, to), rng, hub); err != nil {
				return DrainPlan{}, err
			}
		}
	}
	for _, r := range m.activeRuns(ctx) {
		plan.LiveRuns = append(plan.LiveRuns, r.RunID)
	}
	plan.Steps = []string{
		"update.activate pyred " + to + ": verify protocol overlap with the hub and switch <state>/tools/pyred/current (new daemon starts from it)",
		"daemon: stop accepting new commands; finish in-flight command acks; flush the report outbox",
		"daemon: persist every RunHandle (runs live in Pyre-owned tmux servers / codex app-servers and are not children of pyred)",
		"service manager (launchd/systemd user unit, WP20): restart pyred from <state>/tools/pyred/current/<exe>",
		"new daemon: NodeHello with live_runs; Recover() each RunHandle without relaunching; resume report outbox from its last OriginSeq",
		"health: reconnected to the hub and all live runs recovered within the drain timeout",
	}
	plan.Rollback = []string{
		"if the new daemon does not become healthy: update.rollback pyred (switches current back to " + orDefault(from, "the previous version") + ")",
		"service manager restarts the previous daemon, which Recover()s the same runs",
	}
	return plan, nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
