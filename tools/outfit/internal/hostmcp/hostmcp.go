package hostmcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Paths struct {
	CodexConfig       string
	CodexCredentials  string
	ClaudeConfig      string
	ClaudeSettings    string
	ClaudeCredentials string
	BackupRoot        string
}

type Source struct {
	Host, CredentialKey, ServerName string
}

type Candidate struct {
	ID, ConnectorKey, Display, Endpoint string
	Hosts, ServerNames, Scopes          []string
	ExpiresAt                           time.Time
	Transferable                        bool
	Reason                              string
	Fields                              map[string]string `json:"-"`
	Sources                             []Source          `json:"-"`
	SelectedSource                      Source            `json:"-"`
}

type Receipt struct {
	BackupPath string
	Hosts      []string
}

type rawGrant struct {
	source       Source
	endpoint     string
	clientID     string
	redirectURI  string
	accessToken  string
	refreshToken string
	expiresAt    time.Time
	scopes       []string
}

func DefaultPaths(home string) Paths {
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	codexRoot := os.Getenv("CODEX_HOME")
	if codexRoot == "" {
		codexRoot = filepath.Join(home, ".codex")
	}
	claudeRoot := os.Getenv("CLAUDE_CONFIG_DIR")
	if claudeRoot == "" {
		claudeRoot = filepath.Join(home, ".claude")
	}
	return Paths{
		CodexConfig: filepath.Join(codexRoot, "config.toml"), CodexCredentials: filepath.Join(codexRoot, ".credentials.json"),
		ClaudeConfig: filepath.Join(home, ".claude.json"), ClaudeSettings: filepath.Join(claudeRoot, "settings.json"), ClaudeCredentials: filepath.Join(claudeRoot, ".credentials.json"),
		BackupRoot: filepath.Join(home, ".config", "outfit", "host-backups"),
	}
}

func Discover(paths Paths) ([]Candidate, error) {
	var grants []rawGrant
	var problems []error
	if discovered, err := readCodexCredentials(paths.CodexCredentials); err != nil {
		problems = append(problems, err)
	} else {
		grants = append(grants, discovered...)
	}
	if discovered, err := readClaudeCredentials(paths.ClaudeCredentials); err != nil {
		problems = append(problems, err)
	} else {
		grants = append(grants, discovered...)
	}
	if len(grants) == 0 && len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	grouped := map[string][]rawGrant{}
	for _, grant := range grants {
		endpoint, err := canonicalEndpoint(grant.endpoint)
		if err != nil {
			continue
		}
		grant.endpoint = endpoint
		grouped[endpoint] = append(grouped[endpoint], grant)
	}
	var result []Candidate
	for endpoint, values := range grouped {
		best := values[0]
		for _, value := range values[1:] {
			if betterGrant(value, best) {
				best = value
			}
		}
		connector, display := connectorFor(endpoint, best.source.ServerName)
		candidate := Candidate{ID: candidateID(connector, endpoint), ConnectorKey: connector, Display: display, Endpoint: endpoint, ExpiresAt: best.expiresAt, Transferable: best.accessToken != "" && best.refreshToken != "" && best.clientID != "", Sources: make([]Source, 0, len(values)), SelectedSource: best.source}
		candidate.Fields = map[string]string{
			"OAUTH_CLIENT_ID": best.clientID, "OAUTH_ACCESS_TOKEN": best.accessToken, "OAUTH_REFRESH_TOKEN": best.refreshToken,
			"OAUTH_TOKEN_EXPIRY": best.expiresAt.UTC().Format(time.RFC3339Nano), "OAUTH_GRANTED_SCOPES": strings.Join(best.scopes, ","), "OAUTH_REDIRECT_URI": best.redirectURI,
		}
		if !candidate.Transferable {
			candidate.Reason = "the host grant is not refreshable; reconnect it in the host or configure outfit directly"
		}
		if endpoint == "https://mcp.slack.com/mcp" {
			candidate.Transferable = false
			candidate.Reason = "Slack's hosted MCP OAuth client secret belongs to the coding host and cannot be transferred; use Outfit's recommended agent-slack CLI mode instead"
		}
		hosts, names := map[string]bool{}, map[string]bool{}
		for _, value := range values {
			candidate.Sources = append(candidate.Sources, value.source)
			hosts[value.source.Host], names[value.source.ServerName] = true, true
		}
		candidate.Hosts, candidate.ServerNames, candidate.Scopes = sortedKeys(hosts), sortedKeys(names), append([]string(nil), best.scopes...)
		result = append(result, candidate)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Display != result[j].Display {
			return result[i].Display < result[j].Display
		}
		return result[i].Endpoint < result[j].Endpoint
	})
	return result, errors.Join(problems...)
}

// PreserveRefreshedGrant updates only the credential record that supplied the
// selected grant. OAuth providers may rotate refresh tokens during preflight;
// persisting that rotation keeps the source host usable if later MCP
// verification fails. It never adds, removes, enables, or disables an MCP.
func PreserveRefreshedGrant(paths Paths, candidate Candidate, fields map[string]string) error {
	source := candidate.SelectedSource
	if source.Host == "" || source.CredentialKey == "" {
		return errors.New("host MCP import has no selected credential owner")
	}
	path, claude := paths.CodexCredentials, false
	if source.Host == "claude" {
		path, claude = paths.ClaudeCredentials, true
	} else if source.Host != "codex" {
		return fmt.Errorf("unsupported host %s", source.Host)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return err
	}
	container := document
	if claude {
		var oauth map[string]json.RawMessage
		if err := json.Unmarshal(document["mcpOAuth"], &oauth); err != nil {
			return err
		}
		container = oauth
	}
	encoded, ok := container[source.CredentialKey]
	if !ok {
		return errors.New("selected host OAuth credential changed concurrently")
	}
	var record map[string]any
	if err := json.Unmarshal(encoded, &record); err != nil {
		return err
	}
	endpointKey := "server_url"
	if claude {
		endpointKey = "serverUrl"
	}
	currentEndpoint, _ := record[endpointKey].(string)
	canonical, _ := canonicalEndpoint(currentEndpoint)
	if canonical != candidate.Endpoint {
		return errors.New("selected host OAuth credential changed endpoint concurrently")
	}
	expiry, err := time.Parse(time.RFC3339Nano, fields["OAUTH_TOKEN_EXPIRY"])
	if err != nil {
		return fmt.Errorf("refreshed OAuth expiry: %w", err)
	}
	if claude {
		record["clientId"], record["accessToken"], record["refreshToken"] = fields["OAUTH_CLIENT_ID"], fields["OAUTH_ACCESS_TOKEN"], fields["OAUTH_REFRESH_TOKEN"]
		record["expiresAt"], record["scope"] = expiry.UnixMilli(), strings.Join(normalizeScopes(strings.Split(fields["OAUTH_GRANTED_SCOPES"], ",")), " ")
	} else {
		record["client_id"], record["access_token"], record["refresh_token"] = fields["OAUTH_CLIENT_ID"], fields["OAUTH_ACCESS_TOKEN"], fields["OAUTH_REFRESH_TOKEN"]
		record["expires_at"], record["scopes"] = expiry.Unix(), normalizeScopes(strings.Split(fields["OAUTH_GRANTED_SCOPES"], ","))
	}
	container[source.CredentialKey], err = json.Marshal(record)
	if err != nil {
		return err
	}
	if claude {
		document["mcpOAuth"], err = json.Marshal(container)
		if err != nil {
			return err
		}
	}
	updated, err := marshalJSON(document)
	if err != nil {
		return err
	}
	return atomicWrite(path, updated, 0o600)
}

func Find(paths Paths, id string) (Candidate, error) {
	candidates, err := Discover(paths)
	if err != nil {
		return Candidate{}, err
	}
	for _, candidate := range candidates {
		if candidate.ID == id {
			return candidate, nil
		}
	}
	return Candidate{}, fmt.Errorf("host MCP import %s is no longer available", id)
}

func Disconnect(paths Paths, candidate Candidate) (Receipt, error) {
	if len(candidate.Sources) == 0 {
		return Receipt{}, errors.New("host MCP import has no source registrations")
	}
	type mutation struct {
		path     string
		original []byte
		updated  []byte
		mode     os.FileMode
	}
	byPath := map[string]*mutation{}
	load := func(path string) (*mutation, error) {
		if existing := byPath[path]; existing != nil {
			return existing, nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				raw = []byte("{}\n")
			} else {
				return nil, err
			}
		}
		mode := os.FileMode(0o600)
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
		item := &mutation{path: path, original: append([]byte(nil), raw...), updated: append([]byte(nil), raw...), mode: mode}
		byPath[path] = item
		return item, nil
	}
	hosts := map[string]bool{}
	for _, source := range candidate.Sources {
		hosts[source.Host] = true
	}
	if hosts["codex"] {
		credentials, err := load(paths.CodexCredentials)
		if err != nil {
			return Receipt{}, err
		}
		credentials.mode = 0o600
		credentials.updated, err = removeCredentialRecords(credentials.updated, candidate.Endpoint, false)
		if err != nil {
			return Receipt{}, err
		}
		config, err := load(paths.CodexConfig)
		if err != nil {
			return Receipt{}, err
		}
		config.updated = disableCodexServers(config.updated, candidate.Endpoint, sourceNames(candidate.Sources, "codex"))
	}
	if hosts["claude"] {
		credentials, err := load(paths.ClaudeCredentials)
		if err != nil {
			return Receipt{}, err
		}
		credentials.mode = 0o600
		credentials.updated, err = removeCredentialRecords(credentials.updated, candidate.Endpoint, true)
		if err != nil {
			return Receipt{}, err
		}
		settings, err := load(paths.ClaudeSettings)
		if err != nil {
			return Receipt{}, err
		}
		settings.updated, err = denyClaudeEndpoint(settings.updated, candidate.Endpoint)
		if err != nil {
			return Receipt{}, err
		}
		config, err := load(paths.ClaudeConfig)
		if err != nil {
			return Receipt{}, err
		}
		config.updated, err = removeClaudeServers(config.updated, candidate.Endpoint, sourceNames(candidate.Sources, "claude"))
		if err != nil {
			return Receipt{}, err
		}
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z") + "-" + candidate.ID
	backup := filepath.Join(paths.BackupRoot, stamp)
	if err := os.MkdirAll(backup, 0o700); err != nil {
		return Receipt{}, err
	}
	mutations := make([]*mutation, 0, len(byPath))
	for _, item := range byPath {
		if bytes.Equal(item.original, item.updated) {
			continue
		}
		mutations = append(mutations, item)
	}
	sort.Slice(mutations, func(i, j int) bool { return mutations[i].path < mutations[j].path })
	for index, item := range mutations {
		name := fmt.Sprintf("%02d-%s", index, strings.TrimPrefix(filepath.Clean(item.path), string(filepath.Separator)))
		name = strings.ReplaceAll(name, string(filepath.Separator), "__")
		if err := os.WriteFile(filepath.Join(backup, name), item.original, 0o600); err != nil {
			return Receipt{}, err
		}
	}
	var written []*mutation
	for _, item := range mutations {
		if err := atomicWrite(item.path, item.updated, item.mode); err != nil {
			for index := len(written) - 1; index >= 0; index-- {
				_ = atomicWrite(written[index].path, written[index].original, written[index].mode)
			}
			return Receipt{}, fmt.Errorf("disconnect host MCP registration: %w", err)
		}
		written = append(written, item)
	}
	return Receipt{BackupPath: backup, Hosts: sortedKeys(hosts)}, nil
}

func readCodexCredentials(path string) ([]rawGrant, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var records map[string]struct {
		ServerName   string   `json:"server_name"`
		ServerURL    string   `json:"server_url"`
		ClientID     string   `json:"client_id"`
		AccessToken  string   `json:"access_token"`
		RefreshToken string   `json:"refresh_token"`
		RedirectURI  string   `json:"redirect_uri"`
		ExpiresAt    int64    `json:"expires_at"`
		Scopes       []string `json:"scopes"`
	}
	if err := json.Unmarshal(raw, &records); err != nil {
		return nil, fmt.Errorf("parse Codex MCP credentials: %w", err)
	}
	result := make([]rawGrant, 0, len(records))
	for key, item := range records {
		name := item.ServerName
		if name == "" {
			name, _, _ = strings.Cut(key, "|")
		}
		result = append(result, rawGrant{source: Source{Host: "codex", CredentialKey: key, ServerName: name}, endpoint: item.ServerURL, clientID: item.ClientID, redirectURI: item.RedirectURI, accessToken: item.AccessToken, refreshToken: item.RefreshToken, expiresAt: parseEpoch(item.ExpiresAt), scopes: normalizeScopes(item.Scopes)})
	}
	return result, nil
}

func readClaudeCredentials(path string) ([]rawGrant, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var document struct {
		MCPOAuth map[string]struct {
			ServerName, ServerURL, ClientID, RedirectURI, AccessToken, RefreshToken, Scope string
			ExpiresAt                                                                      int64
		} `json:"mcpOAuth"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("parse Claude MCP credentials: %w", err)
	}
	result := make([]rawGrant, 0, len(document.MCPOAuth))
	for key, item := range document.MCPOAuth {
		name := item.ServerName
		if name == "" {
			name, _, _ = strings.Cut(key, "|")
		}
		result = append(result, rawGrant{source: Source{Host: "claude", CredentialKey: key, ServerName: name}, endpoint: item.ServerURL, clientID: item.ClientID, redirectURI: item.RedirectURI, accessToken: item.AccessToken, refreshToken: item.RefreshToken, expiresAt: parseEpoch(item.ExpiresAt), scopes: normalizeScopes(strings.Fields(item.Scope))})
	}
	return result, nil
}

func connectorFor(endpoint, name string) (string, string) {
	known := map[string][2]string{
		"https://mcp.linear.app/mcp": {"linear", "Linear"}, "https://mcp.notion.com/mcp": {"notion", "Notion"},
		"https://mcp.sentry.dev/mcp": {"sentry", "Sentry"}, "https://mcp.slack.com/mcp": {"slack", "Slack"},
	}
	if item, ok := known[endpoint]; ok {
		return item[0], item[1]
	}
	key := sanitizeName(name)
	if key == "" {
		parsed, _ := url.Parse(endpoint)
		key = sanitizeName(strings.Split(parsed.Hostname(), ".")[0])
	}
	if key == "" {
		key = "hosted"
	}
	return "imported-" + key + "-mcp", titleName(key) + " MCP"
}

func candidateID(connector, endpoint string) string {
	digest := sha256.Sum256([]byte(endpoint))
	return connector + "-" + hex.EncodeToString(digest[:6])
}

func betterGrant(left, right rawGrant) bool {
	leftComplete := left.refreshToken != "" && left.clientID != ""
	rightComplete := right.refreshToken != "" && right.clientID != ""
	if leftComplete != rightComplete {
		return leftComplete
	}
	return left.expiresAt.After(right.expiresAt)
}

func canonicalEndpoint(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("MCP endpoint must be a fixed HTTPS URL")
	}
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return parsed.String(), nil
}

func parseEpoch(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	if value > 10_000_000_000 {
		return time.UnixMilli(value).UTC()
	}
	return time.Unix(value, 0).UTC()
}

func normalizeScopes(values []string) []string {
	seen := map[string]bool{}
	for _, value := range values {
		for _, scope := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' }) {
			if scope != "" {
				seen[scope] = true
			}
		}
	}
	return sortedKeys(seen)
}

func sortedKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func sourceNames(values []Source, host string) []string {
	result := map[string]bool{}
	for _, value := range values {
		if value.Host == host {
			result[value.ServerName] = true
		}
	}
	return sortedKeys(result)
}

func removeCredentialRecords(raw []byte, endpoint string, claude bool) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	container := document
	if claude {
		var oauth map[string]json.RawMessage
		if current := document["mcpOAuth"]; len(current) > 0 {
			if err := json.Unmarshal(current, &oauth); err != nil {
				return nil, err
			}
		}
		container = oauth
	}
	for key, encoded := range container {
		var record map[string]any
		if json.Unmarshal(encoded, &record) != nil {
			continue
		}
		value, _ := record["server_url"].(string)
		if claude {
			value, _ = record["serverUrl"].(string)
		}
		canonical, _ := canonicalEndpoint(value)
		if canonical == endpoint {
			delete(container, key)
		}
	}
	if claude {
		encoded, _ := json.Marshal(container)
		document["mcpOAuth"] = encoded
	}
	return marshalJSON(document)
}

func denyClaudeEndpoint(raw []byte, endpoint string) ([]byte, error) {
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	var denied []any
	if current, ok := document["deniedMcpServers"].([]any); ok {
		denied = current
	}
	for _, item := range denied {
		if object, ok := item.(map[string]any); ok && object["serverUrl"] == endpoint {
			return marshalJSON(document)
		}
	}
	document["deniedMcpServers"] = append(denied, map[string]any{"serverUrl": endpoint})
	return marshalJSON(document)
}

func removeClaudeServers(raw []byte, endpoint string, names []string) ([]byte, error) {
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	removeMatchingServerMap(document["mcpServers"], endpoint, names)
	if projects, ok := document["projects"].(map[string]any); ok {
		for _, value := range projects {
			if project, ok := value.(map[string]any); ok {
				removeMatchingServerMap(project["mcpServers"], endpoint, names)
			}
		}
	}
	return marshalJSON(document)
}

func removeMatchingServerMap(value any, endpoint string, names []string) {
	servers, ok := value.(map[string]any)
	if !ok {
		return
	}
	nameSet := map[string]bool{}
	for _, name := range names {
		nameSet[name] = true
	}
	for name, value := range servers {
		server, _ := value.(map[string]any)
		serverURL, _ := server["url"].(string)
		canonical, _ := canonicalEndpoint(serverURL)
		if canonical == endpoint || (serverURL == "" && nameSet[name]) {
			delete(servers, name)
		}
	}
}

func disableCodexServers(raw []byte, endpoint string, names []string) []byte {
	result := string(raw)
	for _, name := range names {
		result = removeTOMLSection(result, "mcp_servers", name)
	}
	if !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	result += "\n# Disabled by outfit after its OAuth grant was verified and transferred.\n"
	for _, name := range names {
		result += "[mcp_servers." + strconv.Quote(name) + "]\nurl = " + strconv.Quote(endpoint) + "\nenabled = false\n\n"
	}
	return []byte(result)
}

func removeTOMLSection(raw, root, name string) string {
	lines := strings.SplitAfter(raw, "\n")
	wanted := root + "." + name
	quoted := root + "." + strconv.Quote(name)
	var result strings.Builder
	skipping := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "["), "]"))
			skipping = section == wanted || section == quoted || strings.HasPrefix(section, wanted+".") || strings.HasPrefix(section, quoted+".")
		}
		if !skipping {
			result.WriteString(line)
		}
	}
	return strings.TrimRight(result.String(), "\n") + "\n"
}

func marshalJSON(value any) ([]byte, error) {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func atomicWrite(path string, raw []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".outfit-host-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(raw); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func sanitizeName(value string) string {
	value = strings.ToLower(value)
	var result strings.Builder
	lastDash := false
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			result.WriteRune(r)
			lastDash = false
		} else if result.Len() > 0 && !lastDash {
			result.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(result.String(), "-")
}

func titleName(value string) string {
	parts := strings.Split(strings.ReplaceAll(value, "_", "-"), "-")
	for index, part := range parts {
		if part != "" {
			parts[index] = strings.ToUpper(part[:1]) + part[1:]
		}
	}
	return strings.Join(parts, " ")
}
