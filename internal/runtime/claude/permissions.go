package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// PermissionsEnv carries the run's permission mode from the daemon
// (ResolvedLaunch.Env; "bypass" => --dangerously-skip-permissions).
const PermissionsEnv = "PYRE_PERMISSIONS"

// settingSkipBypassPrompt is Claude Code's (2.1.28x) record, in user
// settings, that the owner accepted the one-time bypass-permissions warning.
const settingSkipBypassPrompt = "skipDangerousModePermissionPrompt"

// legacyBypassAccepted is the older ~/.claude.json key for the same.
const legacyBypassAccepted = "bypassPermissionsModeAccepted"

func (a *Adapter) ownerHome() string {
	if a.o.OwnerHome != "" {
		return a.o.OwnerHome
	}
	h, _ := os.UserHomeDir()
	return h
}

// OwnerAcceptedBypass reports whether the owner accepted Claude's
// bypass-permissions warning under home (values are booleans; nothing
// secret is read).
func OwnerAcceptedBypass(home string) bool {
	if home == "" {
		return false
	}
	return jsonBool(filepath.Join(home, ".claude", "settings.json"), settingSkipBypassPrompt) ||
		jsonBool(filepath.Join(home, ".claude.json"), legacyBypassAccepted)
}

func jsonBool(path, key string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	var v bool
	return json.Unmarshal(m[key], &v) == nil && v
}

// setSettingsKey sets one top-level key in a run settings file.
func setSettingsKey(path, key string, val any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	m[key] = val
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, out)
}
