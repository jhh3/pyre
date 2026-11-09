// Package apikeylogin tells whether a machine's own Claude or Codex login
// bills an API key instead of a subscription. The owner's rule: Codex and
// Claude work runs on subscription accounts, never an API key. pyre run's
// fallback and a session launch on the node's own login (ambient) check
// here first and refuse an API-key login.
//
// Only local files are read; key values are never returned, kept or
// logged. Login environment variables (ANTHROPIC_API_KEY, OPENAI_API_KEY,
// ...) are not judged here: every caller removes them from the child's
// environment (contracts.ClaudeLoginEnv, contracts.CodexLoginEnv).
package apikeylogin

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// Codex reports why the Codex login in home (CODEX_HOME; ~/.codex when
// empty) is an API-key login, "" when it is not (a ChatGPT login, none or
// unreadable). auth.json in API-key mode ("auth_mode": "apikey", or an
// OPENAI_API_KEY with no ChatGPT tokens) bills the API.
func Codex(home string) string {
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(h, ".codex")
	}
	p := filepath.Join(home, "auth.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var a struct {
		AuthMode string  `json:"auth_mode"`
		APIKey   *string `json:"OPENAI_API_KEY"`
		Tokens   *struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(b, &a) != nil {
		return ""
	}
	hasKey := a.APIKey != nil && *a.APIKey != ""
	noTokens := a.Tokens == nil || a.Tokens.AccessToken == "" && a.Tokens.RefreshToken == ""
	if a.AuthMode == "apikey" || hasKey && noTokens {
		return p + " is an API-key login (auth_mode " + quoteMode(a.AuthMode) + "), which bills the OpenAI API, not a ChatGPT subscription"
	}
	return ""
}

func quoteMode(m string) string {
	if m == "" {
		return "unset"
	}
	return `"` + m + `"`
}

// Claude reports why the Claude login is an API-key login, "" when it is
// not. configDir is CLAUDE_CONFIG_DIR ("" for the default: ~/.claude with
// ~/.claude.json).
func Claude(configDir string) string {
	if configDir != "" {
		return ClaudeFiles(configDir, filepath.Join(configDir, ".claude.json"))
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return ClaudeFiles(filepath.Join(h, ".claude"), filepath.Join(h, ".claude.json"))
}

// ClaudeFiles is Claude for a settings directory (holding settings.json)
// and a state file (.claude.json) given apart. An apiKeyHelper in the
// settings (or the machine's managed settings) or a primaryApiKey (a
// Console login) in the state file bills the API.
func ClaudeFiles(settingsDir, stateFile string) string {
	var files []string
	if settingsDir != "" {
		files = append(files, filepath.Join(settingsDir, "settings.json"), filepath.Join(settingsDir, "settings.local.json"))
	}
	for _, p := range append(files, managedSettings()...) {
		var s struct {
			APIKeyHelper string `json:"apiKeyHelper"`
		}
		if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &s) == nil && s.APIKeyHelper != "" {
			return p + " sets apiKeyHelper, which bills the Anthropic API, not a Claude subscription"
		}
	}
	var st struct {
		PrimaryAPIKey string `json:"primaryApiKey"`
	}
	if stateFile != "" {
		if b, err := os.ReadFile(stateFile); err == nil && json.Unmarshal(b, &st) == nil && st.PrimaryAPIKey != "" {
			return stateFile + " holds an API key (a Console login), which bills the Anthropic API, not a Claude subscription"
		}
	}
	return ""
}

// managedSettings are Claude Code's machine-wide managed settings files.
func managedSettings() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"/Library/Application Support/ClaudeCode/managed-settings.json"}
	case "windows":
		return []string{`C:\Program Files\ClaudeCode\managed-settings.json`}
	}
	return []string{"/etc/claude-code/managed-settings.json"}
}

// Check is Claude or Codex for harness name h ("claude", "codex") with
// that harness's config home; "" for any other harness.
func Check(h, home string) string {
	switch h {
	case "claude":
		return Claude(home)
	case "codex":
		return Codex(home)
	}
	return ""
}

// NoLogin reports why harness h ("claude", "codex") has no own login at all
// in home (its config home; the default when empty), "" when it has one or
// it cannot tell. A run on no login fails at the provider (Codex: "401
// Unauthorized: Missing bearer"), so pyre run's fallback refuses it.
// Codex: no auth.json. Claude: no .credentials.json on Linux and Windows,
// unless CLAUDE_CODE_OAUTH_TOKEN carries a login (getenv); macOS keeps the
// login in the Keychain, which is not read here, so it is never judged.
func NoLogin(h, home string, getenv func(string) string) string {
	if home == "" {
		u, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(u, "."+h)
	}
	var p string
	switch h {
	case "codex":
		p = filepath.Join(home, "auth.json")
	case "claude":
		if runtime.GOOS == "darwin" || getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
			return ""
		}
		p = filepath.Join(home, ".credentials.json")
	default:
		return ""
	}
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		return "this machine has no own " + h + " login (" + p + " does not exist)"
	}
	return ""
}
