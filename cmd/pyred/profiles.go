package main

import (
	"os"
	"path/filepath"
)

// EnvProfileRoot overrides where isolated account profiles live.
const EnvProfileRoot = "PYRE_PROFILE_ROOT"

// profileRoot picks the account profile root (decision I08):
//
//  1. $PYRE_PROFILE_ROOT;
//  2. <state>/profiles when it already exists (installs from before I08 keep
//     their profiles and logins);
//  3. ~/.local/state/pyre/profiles on every OS.
func profileRoot(state string, getenv func(string) string, home string) string {
	if v := getenv(EnvProfileRoot); v != "" {
		return v
	}
	legacy := filepath.Join(state, "profiles")
	if st, err := os.Stat(legacy); err == nil && st.IsDir() {
		return legacy
	}
	if home == "" {
		return legacy
	}
	return filepath.Join(home, ".local", "state", "pyre", "profiles")
}
