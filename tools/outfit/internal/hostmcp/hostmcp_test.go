package hostmcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiscoverGroupsClaudeAndCodexOAuthWithoutExposingTokens(t *testing.T) {
	paths := fixturePaths(t)
	writeJSONFile(t, paths.CodexCredentials, map[string]any{
		"linear|one": map[string]any{"server_name": "linear", "server_url": "https://mcp.linear.app/mcp", "client_id": "codex-client", "access_token": "codex-access", "refresh_token": "codex-refresh", "expires_at": time.Now().Add(time.Hour).Unix(), "scopes": []string{"read", "write"}},
	})
	writeJSONFile(t, paths.ClaudeCredentials, map[string]any{"mcpOAuth": map[string]any{
		"plugin:team:linear|one": map[string]any{"serverName": "plugin:team:linear", "serverUrl": "https://mcp.linear.app/mcp", "clientId": "claude-client", "redirectUri": "http://localhost:3118/callback", "accessToken": "claude-access", "refreshToken": "claude-refresh", "expiresAt": time.Now().Add(2 * time.Hour).UnixMilli(), "scope": "read write"},
	}})

	candidates, err := Discover(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %#v", candidates)
	}
	candidate := candidates[0]
	if candidate.ConnectorKey != "linear" || strings.Join(candidate.Hosts, ",") != "claude,codex" || !candidate.Transferable {
		t.Fatalf("candidate = %#v", candidate)
	}
	if candidate.Fields["OAUTH_ACCESS_TOKEN"] != "claude-access" || candidate.Fields["OAUTH_REFRESH_TOKEN"] != "claude-refresh" {
		t.Fatalf("newest transferable grant was not selected: %#v", candidate.Fields)
	}
	if candidate.Fields["OAUTH_REDIRECT_URI"] != "http://localhost:3118/callback" {
		t.Fatalf("OAuth callback ownership was not transferred: %#v", candidate.Fields)
	}
	encoded, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "access") || strings.Contains(string(encoded), "refresh") {
		t.Fatalf("safe candidate JSON leaked a token: %s", encoded)
	}
}

func TestDiscoverKeepsSafeCandidatesButReportsUnreadableHostStore(t *testing.T) {
	paths := fixturePaths(t)
	writeJSONFile(t, paths.CodexCredentials, map[string]any{
		"linear|one": map[string]any{"server_name": "linear", "server_url": "https://mcp.linear.app/mcp", "client_id": "client", "access_token": "access", "refresh_token": "refresh", "expires_at": time.Now().Add(time.Hour).Unix()},
	})
	writeFile(t, paths.ClaudeCredentials, "not-json")
	candidates, err := Discover(paths)
	if len(candidates) != 1 || err == nil || !strings.Contains(err.Error(), "Claude") {
		t.Fatalf("candidates=%#v err=%v", candidates, err)
	}
	if _, findErr := Find(paths, candidates[0].ID); findErr == nil {
		t.Fatal("adoption proceeded while another host credential store was unreadable")
	}
}

func TestDiscoverTreatsRefreshableGrantWithoutRecordedScopesAsTransferable(t *testing.T) {
	paths := fixturePaths(t)
	writeJSONFile(t, paths.ClaudeCredentials, map[string]any{"mcpOAuth": map[string]any{
		"notion|one": map[string]any{"serverName": "notion", "serverUrl": "https://mcp.notion.com/mcp", "clientId": "client", "accessToken": "access", "refreshToken": "refresh", "expiresAt": time.Now().Add(time.Hour).UnixMilli(), "scope": ""},
	}})

	candidates, err := Discover(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ConnectorKey != "notion" || !candidates[0].Transferable {
		t.Fatalf("candidate = %#v", candidates)
	}
	if candidates[0].Fields["OAUTH_GRANTED_SCOPES"] != "" || len(candidates[0].Scopes) != 0 {
		t.Fatalf("unreported scopes were invented: %#v", candidates[0])
	}
}

func TestDiscoverDoesNotClaimHostOwnedSlackOAuthIsTransferable(t *testing.T) {
	paths := fixturePaths(t)
	writeJSONFile(t, paths.ClaudeCredentials, map[string]any{"mcpOAuth": map[string]any{
		"slack|one": map[string]any{"serverName": "slack", "serverUrl": "https://mcp.slack.com/mcp", "clientId": "host-client", "accessToken": "access", "refreshToken": "refresh", "expiresAt": time.Now().Add(time.Hour).UnixMilli()},
	}})
	candidates, err := Discover(paths)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates=%#v err=%v", candidates, err)
	}
	if candidates[0].Transferable || !strings.Contains(candidates[0].Reason, "agent-slack") {
		t.Fatalf("Slack host OAuth was advertised as portable: %#v", candidates[0])
	}
}

func TestDisconnectRemovesHostCredentialsAndDisablesExactEndpoint(t *testing.T) {
	paths := fixturePaths(t)
	writeJSONFile(t, paths.CodexCredentials, map[string]any{
		"linear|one": map[string]any{"server_name": "linear", "server_url": "https://mcp.linear.app/mcp", "client_id": "client", "access_token": "access", "refresh_token": "refresh", "expires_at": time.Now().Add(time.Hour).Unix()},
		"other|one":  map[string]any{"server_name": "other", "server_url": "https://mcp.other.test/mcp", "access_token": "keep"},
	})
	writeFile(t, paths.CodexConfig, "model = \"gpt\"\n\n[mcp_servers.linear]\nurl = \"https://mcp.linear.app/mcp\"\nhttp_headers = { X = \"keep-out\" }\n\n[mcp_servers.other]\nurl = \"https://mcp.other.test/mcp\"\n")
	writeJSONFile(t, paths.ClaudeCredentials, map[string]any{"mcpOAuth": map[string]any{
		"plugin:team:linear|one": map[string]any{"serverName": "plugin:team:linear", "serverUrl": "https://mcp.linear.app/mcp", "clientId": "client", "accessToken": "access", "refreshToken": "refresh", "expiresAt": time.Now().Add(time.Hour).UnixMilli()},
		"other|one":              map[string]any{"serverName": "other", "serverUrl": "https://mcp.other.test/mcp", "accessToken": "keep"},
	}})
	writeJSONFile(t, paths.ClaudeSettings, map[string]any{"theme": "dark", "deniedMcpServers": []any{map[string]any{"serverUrl": "https://already.test/mcp"}}})
	writeJSONFile(t, paths.ClaudeConfig, map[string]any{
		"mcpServers": map[string]any{"linear": map[string]any{"type": "http", "url": "https://mcp.linear.app/mcp"}, "keep": map[string]any{"type": "http", "url": "https://mcp.other.test/mcp"}},
		"projects":   map[string]any{"/repo": map[string]any{"mcpServers": map[string]any{"linear-project": map[string]any{"type": "http", "url": "https://mcp.linear.app/mcp"}}}},
	})

	candidates, err := Discover(paths)
	if err != nil {
		t.Fatal(err)
	}
	var linear Candidate
	for _, candidate := range candidates {
		if candidate.ConnectorKey == "linear" {
			linear = candidate
		}
	}
	receipt, err := Disconnect(paths, linear)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.BackupPath == "" {
		t.Fatal("missing backup path")
	}
	if entries, _ := os.ReadDir(receipt.BackupPath); len(entries) < 4 {
		t.Fatalf("backup entries = %d", len(entries))
	}
	codexCredentials := readObject(t, paths.CodexCredentials)
	if _, ok := codexCredentials["linear|one"]; ok || codexCredentials["other|one"] == nil {
		t.Fatalf("codex credentials = %#v", codexCredentials)
	}
	codexConfig := string(readFile(t, paths.CodexConfig))
	if !strings.Contains(codexConfig, "[mcp_servers.\"linear\"]") || !strings.Contains(codexConfig, "enabled = false") || !strings.Contains(codexConfig, "[mcp_servers.other]") || strings.Contains(codexConfig, "keep-out") {
		t.Fatalf("codex config = %s", codexConfig)
	}
	claudeCredentials := readObject(t, paths.ClaudeCredentials)["mcpOAuth"].(map[string]any)
	if _, ok := claudeCredentials["plugin:team:linear|one"]; ok || claudeCredentials["other|one"] == nil {
		t.Fatalf("claude credentials = %#v", claudeCredentials)
	}
	settings := readObject(t, paths.ClaudeSettings)
	encoded, _ := json.Marshal(settings["deniedMcpServers"])
	if !strings.Contains(string(encoded), "https://mcp.linear.app/mcp") || !strings.Contains(string(encoded), "https://already.test/mcp") {
		t.Fatalf("denied MCP servers = %s", encoded)
	}
	claudeConfig := readObject(t, paths.ClaudeConfig)
	servers := claudeConfig["mcpServers"].(map[string]any)
	if servers["linear"] != nil || servers["keep"] == nil {
		t.Fatalf("claude MCP servers = %#v", servers)
	}
	projectServers := claudeConfig["projects"].(map[string]any)["/repo"].(map[string]any)["mcpServers"].(map[string]any)
	if len(projectServers) != 0 {
		t.Fatalf("project MCP servers = %#v", projectServers)
	}
}

func TestPreserveRefreshedGrantUpdatesOnlySelectedOwner(t *testing.T) {
	paths := fixturePaths(t)
	writeJSONFile(t, paths.CodexCredentials, map[string]any{
		"linear|one": map[string]any{"server_name": "linear", "server_url": "https://mcp.linear.app/mcp", "client_id": "codex-client", "access_token": "codex-old", "refresh_token": "codex-old-refresh", "expires_at": time.Now().Add(time.Hour).Unix(), "scopes": []string{"read"}},
	})
	writeJSONFile(t, paths.ClaudeCredentials, map[string]any{"mcpOAuth": map[string]any{
		"linear|two": map[string]any{"serverName": "linear", "serverUrl": "https://mcp.linear.app/mcp", "clientId": "claude-client", "accessToken": "claude-old", "refreshToken": "claude-old-refresh", "expiresAt": time.Now().Add(2 * time.Hour).UnixMilli(), "scope": "read write"},
	}})
	candidates, err := Discover(paths)
	if err != nil {
		t.Fatal(err)
	}
	candidate := candidates[0]
	if candidate.SelectedSource.Host != "claude" {
		t.Fatalf("selected source = %#v", candidate.SelectedSource)
	}
	expiry := time.Now().Add(4 * time.Hour).UTC().Truncate(time.Second)
	fields := map[string]string{"OAUTH_CLIENT_ID": "claude-client", "OAUTH_ACCESS_TOKEN": "claude-new", "OAUTH_REFRESH_TOKEN": "claude-new-refresh", "OAUTH_TOKEN_EXPIRY": expiry.Format(time.RFC3339Nano), "OAUTH_GRANTED_SCOPES": "read,write"}
	if err := PreserveRefreshedGrant(paths, candidate, fields); err != nil {
		t.Fatal(err)
	}
	claude := readObject(t, paths.ClaudeCredentials)["mcpOAuth"].(map[string]any)["linear|two"].(map[string]any)
	if claude["accessToken"] != "claude-new" || claude["refreshToken"] != "claude-new-refresh" || claude["scope"] != "read write" {
		t.Fatalf("selected owner was not refreshed: %#v", claude)
	}
	codex := readObject(t, paths.CodexCredentials)["linear|one"].(map[string]any)
	if codex["access_token"] != "codex-old" || codex["refresh_token"] != "codex-old-refresh" {
		t.Fatalf("unselected owner was changed: %#v", codex)
	}
}

func fixturePaths(t *testing.T) Paths {
	t.Helper()
	root := t.TempDir()
	return Paths{CodexConfig: filepath.Join(root, "codex", "config.toml"), CodexCredentials: filepath.Join(root, "codex", ".credentials.json"), ClaudeConfig: filepath.Join(root, ".claude.json"), ClaudeSettings: filepath.Join(root, "claude", "settings.json"), ClaudeCredentials: filepath.Join(root, "claude", ".credentials.json"), BackupRoot: filepath.Join(root, "backups")}
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(encoded)+"\n")
}

func writeFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readObject(t *testing.T, path string) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(readFile(t, path), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
