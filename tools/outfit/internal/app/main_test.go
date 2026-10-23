package app

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain points every home a command may fall back to (skill roots under
// HOME, CODEX_HOME, CLAUDE_CONFIG_DIR; Outfit's default state) at a
// temporary directory, so no test in this package can reach the user's
// real skills, Outfit state or config.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "outfit-app-test-home-")
	if err != nil {
		panic(err)
	}
	for key, value := range map[string]string{
		"HOME":              home,
		"USERPROFILE":       home,
		"XDG_CONFIG_HOME":   filepath.Join(home, ".config"),
		"XDG_DATA_HOME":     filepath.Join(home, ".local", "share"),
		"CODEX_HOME":        filepath.Join(home, ".codex"),
		"CLAUDE_CONFIG_DIR": filepath.Join(home, ".claude"),
		"OUTFIT_HOME":       filepath.Join(home, "outfit"),
	} {
		_ = os.Setenv(key, value)
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
