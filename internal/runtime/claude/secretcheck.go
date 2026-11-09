package claude

import (
	"encoding/json"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/secretegress"
)

// maxSecurityAlerts bounds the alerts a run's state keeps (the daemon
// reports each once; older ones were reported already).
const maxSecurityAlerts = 64

// securityCheck flags a PreToolUse call that sends a credential out or
// uses a planted decoy (decision I79). Flag only: the hook's answer is
// unchanged and the call runs. It fails open: any panic means no alert.
func securityCheck(stateDir string, rec hookRecord, p map[string]any) (out *contracts.SecurityAlert) {
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	d := secretegress.ReadDecoys(stateDir)
	ti, _ := p["tool_input"].(map[string]any)
	cmd := str(ti, "command") // Bash, PowerShell
	var f *secretegress.Finding
	if cmd != "" {
		f = secretegress.Check(cmd, d)
	}
	if f == nil && len(d.Env) > 0 && ti != nil {
		// Any other tool (an MCP call, a file write, a fetch) holding a
		// decoy's value.
		b, _ := json.Marshal(ti)
		if f = secretegress.CheckValues(string(b), d); f != nil {
			cmd = string(b)
		}
	}
	if f == nil {
		return nil
	}
	a := f.Alert(rec.ID, rec.ToolName, cmd, d, rec.At)
	return &a
}

// addSecurity keeps an alert from the event log for Observe.
func (rs *runState) addSecurity(a contracts.SecurityAlert) {
	for _, x := range rs.security {
		if x.AlertID == a.AlertID {
			return
		}
	}
	rs.security = append(rs.security, a)
	if n := len(rs.security); n > maxSecurityAlerts {
		rs.security = append([]contracts.SecurityAlert(nil), rs.security[n-maxSecurityAlerts:]...)
	}
}
