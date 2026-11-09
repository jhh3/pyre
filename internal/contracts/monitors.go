package contracts

import (
	"encoding/json"
	"strings"
)

// Monitors (additive): a monitor is a shell session started by `pyre
// monitor add` that runs a check on a timer and messages the session it
// watches for when the output changes. Monitors are plumbing, not work:
// sessions.list leaves them out unless ListSessionsRequest.IncludeMonitors
// is set, and the views of the sessions they watch for carry them in
// SessionView.Monitors instead. sessions.get and sessions.get_many return
// them as usual (SessionView.Monitor set).
const (
	// TagMonitor marks a monitor session.
	TagMonitor = "monitor"
	// TagMonitorToPrefix + session ID names the session a monitor wakes.
	TagMonitorToPrefix = "monitor-to:"
	// TagMonitorKeyPrefix + key identifies what a monitor watches (a hash
	// of its command, machine and directory), so a repeat of the same
	// `pyre monitor add` reuses the live monitor.
	TagMonitorKeyPrefix = "monitor-key:"
	// TagWaitGitHub marks a monitor that is a GitHub wait (`pyre wait
	// gh-pr` / `gh-run`): it outlives the session that created it (the
	// archive cascade skips it) and messages that session's parent once
	// the session has ended.
	TagWaitGitHub = "wait-gh"
	// MonitorTitlePrefix starts every monitor's title.
	MonitorTitlePrefix = "monitor: "
)

// MonitorRef is a live monitor in the view of the session it wakes.
type MonitorRef struct {
	SessionID string   `json:"session_id"`
	Title     string   `json:"title"`
	State     RunState `json:"state,omitempty"`
}

// IsMonitor reports whether s is a monitor: a shell session tagged
// TagMonitor (pyre monitor add has always set it). The title is never
// used, so a shell someone titled "monitor: ..." stays a session.
func (s Session) IsMonitor() bool {
	if s.Harness != HarnessShell {
		return false
	}
	for _, t := range s.Tags {
		if t == TagMonitor {
			return true
		}
	}
	return false
}

// IsGitHubWait reports whether s is a GitHub wait (a monitor tagged
// TagWaitGitHub).
func (s Session) IsGitHubWait() bool {
	if !s.IsMonitor() {
		return false
	}
	for _, t := range s.Tags {
		if t == TagWaitGitHub {
			return true
		}
	}
	return false
}

// MonitorLive reports whether a monitor whose run is in state st counts
// as watching (badges, reuse): starting, running or waiting. Unknown (the
// node lost contact), exiting and ended runs do not.
func MonitorLive(st RunState) bool {
	switch st {
	case RunStarting, RunRunning, RunWaitingInput, RunWaitingApproval, RunWaitingBackground:
		return true
	}
	return false
}

// MonitorTarget is the session monitor s wakes: its TagMonitorToPrefix
// tag, else its parent. "" when s is not a monitor.
func (s Session) MonitorTarget() string {
	if !s.IsMonitor() {
		return ""
	}
	for _, t := range s.Tags {
		if id, ok := strings.CutPrefix(t, TagMonitorToPrefix); ok && id != "" {
			return id
		}
	}
	return s.Parent()
}

// MonitorKey is s's TagMonitorKeyPrefix value ("" without one).
func (s Session) MonitorKey() string {
	for _, t := range s.Tags {
		if k, ok := strings.CutPrefix(t, TagMonitorKeyPrefix); ok {
			return k
		}
	}
	return ""
}

// AdapterDataMonitorArgs (Run.AdapterData, additive) records a monitor
// run's shell arguments (a JSON array: the `pyre monitor run ...` command
// line), so pyred's restart recovery, `pyre resume` and `pyre node resume`
// can relaunch a monitor whose process a reboot or crash killed with the
// same command, interval, flags and target. Nonsecret: the command is
// already in the monitor's title and screen. Runs started before it
// existed have none and cannot be relaunched (re-create them with
// `pyre monitor add`).
const AdapterDataMonitorArgs = "pyre.monitor_args"

// MonitorRelaunchArgs returns r's recorded monitor arguments (nil when none).
func MonitorRelaunchArgs(r Run) []string {
	raw := r.AdapterData[AdapterDataMonitorArgs]
	if raw == "" {
		return nil
	}
	var args []string
	if json.Unmarshal([]byte(raw), &args) != nil {
		return nil
	}
	return args
}
