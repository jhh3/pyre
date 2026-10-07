// Package claudeplugin builds the per-run Claude Code plugin that rings
// Pyre's inbox "doorbell" (S-D, SOTA review #4) and implements the watcher
// behind `pyred inbox-watch`.
//
// The plugin is loaded for one session with `claude --plugin-dir=<dir>`. Its
// background monitor (Claude Code `experimental.monitors`, verified on
// 2.1.289) runs the watcher; every stdout line becomes a notification that
// wakes the session — idle, or with a human attached — without typing into
// the TUI. The line never carries a message body: the agent reads mail with
// the pyre `inbox` tool and acks it, and that tool call (seen by the
// PostToolUse hook) is the acceptance evidence.
package claudeplugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Name is the plugin name (components are namespaced "pyre-session:…").
const Name = "pyre-session"

// MonitorName is the monitor's name within the plugin.
const MonitorName = "pyre-inbox"

// MonitorDescription is the monitor's description; Claude shows it in the
// notification ("Monitor event: …") and in Stop's background_tasks.
const MonitorDescription = "New Pyre messages for this session"

// Generate writes the plugin into dir (created 0700) with one monitor
// running monitorCommand, a shell command line (quote it with
// tmux.ShellJoin). It returns dir.
func Generate(dir, monitorCommand string) (string, error) {
	if monitorCommand == "" {
		return "", fmt.Errorf("claudeplugin: empty monitor command")
	}
	for _, d := range []string{dir, filepath.Join(dir, ".claude-plugin"), filepath.Join(dir, "monitors")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", err
		}
	}
	manifest := map[string]any{
		"name":        Name,
		"version":     "1.0.0",
		"description": "Pyre session doorbell: notifies the agent when Pyre messages arrive (generated per run; do not edit)",
		"author":      map[string]string{"name": "Pyre"},
	}
	monitors := []map[string]string{{
		"name":        MonitorName,
		"command":     monitorCommand,
		"description": MonitorDescription,
	}}
	if err := writeJSON(filepath.Join(dir, ".claude-plugin", "plugin.json"), manifest); err != nil {
		return "", err
	}
	if err := writeJSON(filepath.Join(dir, "monitors", "monitors.json"), monitors); err != nil {
		return "", err
	}
	return dir, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// IsNotification reports whether a transcript user-record text is a Claude
// monitor notification from this plugin's monitor.
func IsNotification(text string) bool {
	return strings.Contains(text, "<task-notification>") && strings.Contains(text, "New Pyre messages for this session")
}

// StateFile is the watcher's state (cursor, notified ids) in the run dir.
const StateFile = "doorbell.json"

// NotifiedAt reports when the run's watcher last notified the agent about
// messageID (from <runDir>/doorbell.json).
func NotifiedAt(runDir, messageID string) (time.Time, bool) {
	b, err := os.ReadFile(filepath.Join(runDir, StateFile))
	if err != nil {
		return time.Time{}, false
	}
	var st struct {
		Notified map[string]int64 `json:"notified"`
	}
	if json.Unmarshal(b, &st) != nil {
		return time.Time{}, false
	}
	t, ok := st.Notified[messageID]
	if !ok || t <= 0 {
		return time.Time{}, false
	}
	return time.Unix(t, 0), true
}

// PIDFile is written by the watcher into the run dir so the adapter can
// tell whether the doorbell is alive: Claude Code 2.1.289 does not restart
// a plugin monitor that exits or is killed.
const PIDFile = "doorbell.pid"

// WritePID records the current process as the run's watcher.
func WritePID(runDir string) {
	if runDir == "" {
		return
	}
	_ = os.WriteFile(filepath.Join(runDir, PIDFile), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600)
}

// RemovePID removes the run's watcher pid file if it still names this
// process (a newer watcher may have replaced it).
func RemovePID(runDir string) {
	if runDir == "" {
		return
	}
	p := filepath.Join(runDir, PIDFile)
	if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) == fmt.Sprint(os.Getpid()) {
		_ = os.Remove(p)
	}
}
