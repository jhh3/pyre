package contracts

import "testing"

func TestIsLoginEnv(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_API_KEY", "anthropic_api_key", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
		"CODEX_HOME", "OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_GENAI_USE_VERTEXAI"} {
		if !IsLoginEnv(k) {
			t.Errorf("%s not a login variable", k)
		}
	}
	for _, k := range []string{"AGENT_BRIDGE_TOKEN", "NODE_OPTIONS", "OPENAI_BASE", "CLAUDE_CODE_MAX_OUTPUT_TOKENS"} {
		if IsLoginEnv(k) {
			t.Errorf("%s is not a login variable", k)
		}
	}
}
