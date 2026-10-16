package claudeplugin

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// RemindFile is the request pyred leaves in the run dir when it parks
// doorbell-announced mail instead of pasting it (stale held mail,
// W-delivery2): the watcher rings one summary line for the ids still unread
// and removes the file.
const RemindFile = "doorbell-remind.json"

type remindReq struct {
	IDs []string `json:"ids"`
}

// RequestReminder adds ids to the run's pending summary request.
func RequestReminder(runDir string, ids []string) error {
	if runDir == "" || len(ids) == 0 {
		return nil
	}
	p := filepath.Join(runDir, RemindFile)
	var r remindReq
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &r)
	}
	seen := map[string]bool{}
	for _, id := range r.IDs {
		seen[id] = true
	}
	for _, id := range ids {
		if !seen[id] && len(r.IDs) < 1000 {
			seen[id] = true
			r.IDs = append(r.IDs, id)
		}
	}
	b, _ := json.Marshal(r)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// takeReminders reads and removes the pending summary request.
func takeReminders(runDir string) []string {
	if runDir == "" {
		return nil
	}
	p := filepath.Join(runDir, RemindFile)
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	_ = os.Remove(p)
	var r remindReq
	if json.Unmarshal(b, &r) != nil {
		return nil
	}
	return r.IDs
}
