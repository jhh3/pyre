package contracts

import (
	"slices"
	"strings"
)

// Login and provider environment variables: the one list behind the
// variables a managed launch removes (internal/accounts), the keys a
// session's environment file may only clear (internal/envfile) and the
// provider switches that disable Claude's plugin monitors.
var (
	// ClaudeProviderSwitches move Claude off the bound subscription to a
	// cloud provider's credential chain.
	ClaudeProviderSwitches = []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"}
	// ClaudeMonitorBlockers are the variables under which Claude Code runs
	// no plugin monitors (Pyre's doorbell; docs: Monitor tool availability).
	ClaudeMonitorBlockers = append([]string{"DISABLE_TELEMETRY", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"}, ClaudeProviderSwitches...)
	// ClaudeLoginEnv are Claude's login, API key and credential-store
	// variables plus the provider switches.
	ClaudeLoginEnv = append([]string{"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_SECURESTORAGE_CONFIG_DIR",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}, ClaudeProviderSwitches...)
	// CodexLoginEnv are Codex's API key and token variables.
	CodexLoginEnv = []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"}
	// GoogleLoginEnv are the Gemini/Google credential and provider
	// variables agy (Google Antigravity) and Gemini tooling read.
	GoogleLoginEnv = []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS",
		"GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION"}
	// loginEnvPrefixes cover variants and future variables of the same
	// families (CLAUDE_CODE_OAUTH_REFRESH_TOKEN, CODEX_HOME, ...).
	loginEnvPrefixes = []string{"ANTHROPIC_", "CLAUDE_CODE_OAUTH_", "CLAUDE_CODE_USE_", "CODEX_", "GEMINI_", "GOOGLE_GENAI_"}
)

// IsLoginEnv reports whether k (any case: Windows names are
// case-insensitive) is a login, credential, config-home or provider
// variable the run's account binding decides.
func IsLoginEnv(k string) bool {
	k = strings.ToUpper(k)
	if slices.Contains(ClaudeLoginEnv, k) || slices.Contains(CodexLoginEnv, k) || slices.Contains(GoogleLoginEnv, k) {
		return true
	}
	for _, p := range loginEnvPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}
