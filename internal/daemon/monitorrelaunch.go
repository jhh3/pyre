package daemon

import (
	"encoding/json"

	"github.com/jhh3/pyre/internal/contracts"
)

// Monitor relaunch (iss_…): a monitor (`pyre
// monitor add`, a shell session running `pyre monitor run`) has no native
// thread, but its whole behaviour is its command line. The node records
// that command line on the run (contracts.AdapterDataMonitorArgs), so a
// resume of a monitor, pyred's own after a reboot or crash
// (crashresume.go) or `pyre resume` / `pyre node resume` through the hub,
// starts a fresh run with the same arguments. The loop itself restores its
// deadline, last output and wake budget from its state file
// (internal/cli/monitor.go), so a relaunch neither restarts --for nor
// reports a spurious change. A monitor that ended deliberately exits with
// its run observed (not a crash-resume candidate), and its state file
// says so if it is resumed by hand.

// recordMonitorArgs records a monitor run's shell arguments.
func recordMonitorArgs(r *contracts.Run, s contracts.Session, args []string) {
	if r.Harness != contracts.HarnessShell || !s.IsMonitor() || len(args) == 0 {
		return
	}
	b, _ := json.Marshal(args)
	r.AdapterData[contracts.AdapterDataMonitorArgs] = string(b)
}

// monitorRelaunchable reports whether a resume of prev for session s is a
// monitor relaunch: a monitor whose previous run recorded its arguments.
func monitorRelaunchable(s contracts.Session, prev *contracts.Run) bool {
	return prev != nil && prev.Harness == contracts.HarnessShell && s.IsMonitor() && len(contracts.MonitorRelaunchArgs(*prev)) > 0
}
