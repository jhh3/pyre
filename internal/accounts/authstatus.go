package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/winenv"
)

// claudeAuthStatus is the subset of `claude auth status --json` Pyre uses
// (observed on Claude Code 2.1.289; tokens are never part of it).
type claudeAuthStatus struct {
	LoggedIn        bool   `json:"loggedIn"`
	AuthMethod      string `json:"authMethod"` // e.g. "claude.ai", "api_key", "none"
	APIProvider     string `json:"apiProvider"`
	ConfigDirectory string `json:"configDirectory"`
	APIKeySource    string `json:"apiKeySource"`
}

// probeClaudeAuth runs the harness's own status command for one profile
// with a minimal environment (no API keys or tokens are inherited, so an
// ambient ANTHROPIC_API_KEY cannot mask the profile's login). It exits 1
// when logged out but still prints JSON, so stdout is parsed regardless.
// ok=false means the probe is unavailable (old Claude, missing binary):
// callers fall back to file/keychain signals.
func (s *Service) probeClaudeAuth(ctx context.Context, profileDir string) (claudeAuthStatus, bool) {
	if s.cfg.ClaudeBin == "" {
		return claudeAuthStatus{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := execCommand(ctx, s.cfg.ClaudeBin, "auth", "status", "--json")
	s.refreshComposition(profileDir, contracts.HarnessClaude) // a Claude process in the profile: copies, never owner links
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "USER=" + os.Getenv("USER"), "CLAUDE_CONFIG_DIR=" + profileDir, "DISABLE_AUTOUPDATER=1"}
	cmd.Env = append(cmd.Env, winenv.SystemEnv()...)
	var out bytes.Buffer
	cmd.Stdout = &out
	_ = cmd.Run()
	var st claudeAuthStatus
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &st); err != nil || st.ConfigDirectory == "" {
		return claudeAuthStatus{}, false
	}
	return st, true
}

// checkClaudeAuth returns "" when the status agrees with the profile, else
// the reason the grant must not be used.
func checkClaudeAuth(st claudeAuthStatus, profileDir string) string {
	if !sameDir(st.ConfigDirectory, profileDir) {
		return fmt.Sprintf("claude auth status reports config directory %s, not the profile %s (binding not in effect)", st.ConfigDirectory, profileDir)
	}
	if st.AuthMethod == "api_key" || st.APIKeySource != "" {
		return "claude auth status reports an API-key login in the profile; not a subscription (API billing is never used)"
	}
	if !st.LoggedIn {
		return "claude auth status: not logged in; run the profile login command"
	}
	return ""
}

func sameDir(a, b string) bool {
	ca, err1 := filepath.EvalSymlinks(a)
	cb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ca == cb
}
