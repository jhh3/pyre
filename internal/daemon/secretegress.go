package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/secretegress"
)

// securityReportedDir (in the run dir) holds one marker per reported
// alert, so a pyred restart that re-reads a run's events reports none twice.
const securityReportedDir = "security-reported"

// plantDecoys gives a run its own decoy credentials (decision I79): fresh
// values in env (never replacing a variable already there) and in the run
// dir, where the hook and the Codex adapter look for them. Best effort: a
// run without decoys still runs, and the secret check still works.
func (d *Daemon) plantDecoys(env map[string]string, runDir string) {
	if runDir == "" {
		return
	}
	dec := secretegress.NewDecoys()
	if err := secretegress.WriteDecoys(runDir, dec); err != nil {
		d.cfg.Logger.Info("decoy credentials not planted", "run_dir", runDir, "err", err)
		return
	}
	dec.Plant(env)
}

// applySecurity reports a run's flagged tool calls to the hub (event
// security.alert: audited, and a high one raises an urgent inbox item with a
// push). Each AlertID is reported once. Flag only: nothing is stopped.
func (d *Daemon) applySecurity(ctx context.Context, lr *liveRun, alerts []contracts.SecurityAlert) {
	if len(alerts) == 0 || d.rep == nil {
		return
	}
	rec, _ := lr.snapshot()
	dir := d.runDir(rec.Run.RunID)
	if rec.Handle != nil && rec.Handle.StateDir != "" {
		dir = rec.Handle.StateDir
	}
	for _, a := range alerts {
		if a.AlertID == "" {
			continue
		}
		sum := sha256.Sum256([]byte(a.AlertID))
		key := hex.EncodeToString(sum[:12])
		lr.mu.Lock()
		seen := lr.securitySeen[key]
		lr.mu.Unlock()
		if seen {
			continue
		}
		marker := filepath.Join(dir, securityReportedDir, key)
		if _, err := os.Stat(marker); err == nil {
			lr.markSecurity(key)
			continue
		}
		a.RunID, a.SessionID, a.Harness = rec.Run.RunID, rec.Run.SessionID, rec.Run.Harness
		a.NodeID, a.NodeName = d.nodeID, d.cfg.NodeName
		d.cfg.Logger.Warn("security alert: a tool call looks like a credential leaving the machine",
			"severity", a.Severity, "rule", a.Rule, "session", a.SessionID, "run", a.RunID, "tool", a.Tool, "reason", a.Reason)
		payload, err := json.Marshal(a)
		if err != nil {
			continue
		}
		ev := contracts.Event{Kind: contracts.EvSecurityAlert, Object: contracts.ObjectRef{Kind: contracts.KindRun, ID: a.RunID},
			SessionID: a.SessionID, PayloadVersion: 1, Payload: payload, OccurredAt: a.At}
		if ev.OccurredAt.IsZero() {
			ev.OccurredAt = d.now().UTC()
		}
		if _, err := d.rep.Enqueue(ctx, contracts.ReportEvent, ev); err != nil {
			d.cfg.Logger.Info("security alert: report not queued; retried next observation", "run", a.RunID, "err", err)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(marker), 0o700); err == nil {
			_ = os.WriteFile(marker, nil, 0o600)
		}
		lr.markSecurity(key)
	}
}

func (lr *liveRun) markSecurity(key string) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if lr.securitySeen == nil {
		lr.securitySeen = map[string]bool{}
	}
	lr.securitySeen[key] = true
}
