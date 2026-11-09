package codex

import (
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/secretegress"
)

// maxSecurityAlerts bounds the alerts a thread keeps (the daemon reports
// each once).
const maxSecurityAlerts = 64

// checkCommand flags a started command that sends a credential out or uses
// a planted decoy (decision I79). Codex runs no Pyre hook, but the
// app-server reports every command before it runs; flag only, it runs.
// It fails open: a panic means no alert, never a stalled thread.
func (ts *threadState) checkCommand(it item, at time.Time) {
	defer func() { _ = recover() }()
	if it.Command == "" || it.ID == "" {
		return
	}
	f := secretegress.Check(it.Command, ts.decoys)
	if f == nil {
		return
	}
	id := "codex:" + ts.threadID + ":" + it.ID
	for _, x := range ts.security {
		if x.AlertID == id {
			return
		}
	}
	ts.security = append(ts.security, f.Alert(id, "commandExecution", it.Command, ts.decoys, at))
	if n := len(ts.security); n > maxSecurityAlerts {
		ts.security = append([]contracts.SecurityAlert(nil), ts.security[n-maxSecurityAlerts:]...)
	}
}
