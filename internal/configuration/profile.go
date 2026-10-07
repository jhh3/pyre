package configuration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/jhh3/pyre/internal/contracts"
)

// Profile is a desired, versioned, secret-free managed environment layer.
// The global profile supplies LayerManagedGlobal, a project profile
// LayerManagedProject. Secrets never appear here: connections are referenced
// by ID and resolved by the Outfit integration at launch.
type Profile struct {
	Name string `json:"name"`
	// Settings maps setting keys ("claude.<path>", "codex.<path>") to JSON
	// values. They are materialized as overlays (Claude --settings, Codex -c)
	// unless listed in Persist.
	Settings map[string]any `json:"settings,omitempty"`
	// Persist lists claude.* keys that must live in the config home's
	// settings.json (global scope only) rather than a per-run overlay. Pyre
	// owns exactly those keys; every other key in the file stays user-owned.
	Persist     []string          `json:"persist,omitempty"`
	Skills      []SkillSpec       `json:"skills,omitempty"`
	Plugins     []PluginSpec      `json:"plugins,omitempty"`
	MCPServers  []MCPServerSpec   `json:"mcp_servers,omitempty"`
	Hooks       []HookSpec        `json:"hooks,omitempty"`
	Connections []string          `json:"connections,omitempty"` // connection IDs (never secrets)
	Runtimes    map[string]string `json:"runtimes,omitempty"`    // desired tool versions (informational pin)
}

// Skill delivery modes.
const (
	DeliveryHome    = "home"    // install into <harness home>/skills/<name> with an ownership marker
	DeliveryOverlay = "overlay" // Claude only: per-generation plugin loaded with --plugin-dir (pinned per run)
)

// SkillSpec declares a managed skill package.
type SkillSpec struct {
	Name      string            `json:"name"`
	Version   string            `json:"version,omitempty"`
	Harness   contracts.Harness `json:"harness,omitempty"` // empty = both
	SourceDir string            `json:"source_dir"`        // local package directory (e.g. Outfit store)
	Digest    contracts.Digest  `json:"digest,omitempty"`  // verified against SourceDir when set
	Delivery  string            `json:"delivery,omitempty"`
}

// PluginSpec declares a managed plugin.
type PluginSpec struct {
	Name    string            `json:"name"` // "<plugin>@<marketplace>" or a local plugin name
	Harness contracts.Harness `json:"harness"`
	Version string            `json:"version,omitempty"`
	Enabled *bool             `json:"enabled,omitempty"` // default true
	// Dir loads a plugin directory per run (Claude --plugin-dir). When empty
	// and the updates manager has an active "plugin:<name>" tool, its pinned
	// directory is used; otherwise the plugin must already be installed and
	// is enabled through settings.
	Dir string `json:"dir,omitempty"`
}

// MCPServerSpec declares a managed MCP server. Env holds only non-secret
// values; credentials come from connection grants at launch.
type MCPServerSpec struct {
	Name      string            `json:"name"`
	Harness   contracts.Harness `json:"harness,omitempty"` // empty = both
	Transport string            `json:"transport,omitempty"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	URL       string            `json:"url,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// HookSpec declares a managed hook.
type HookSpec struct {
	Harness contracts.Harness `json:"harness"`
	Event   string            `json:"event"`
	Matcher string            `json:"matcher,omitempty"`
	Command string            `json:"command"`
	Timeout int               `json:"timeout,omitempty"`
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@:+-]{0,127}$`)

// Validate checks a profile for structural errors and secret material.
func (p *Profile) Validate() error {
	for k, v := range p.Settings {
		if _, _, err := harnessOf(k); err != nil {
			return contracts.Errorf(contracts.ErrInvalid, "profile %q: %v", p.Name, err)
		}
		if SensitiveKey(k) || SensitiveValue(normalize(v)) {
			return contracts.Errorf(contracts.ErrInvalid, "profile %q: setting %s looks like a credential; secrets belong in an Outfit connection, not a profile", p.Name, k)
		}
	}
	for _, k := range p.Persist {
		h, _, err := harnessOf(k)
		if err != nil {
			return contracts.Errorf(contracts.ErrInvalid, "profile %q persist: %v", p.Name, err)
		}
		if h != contracts.HarnessClaude {
			return contracts.Errorf(contracts.ErrInvalid, "profile %q persist %s: only claude.* keys can be persisted; Pyre never rewrites ~/.codex/config.toml (use an overlay)", p.Name, k)
		}
		if _, ok := p.Settings[k]; !ok {
			return contracts.Errorf(contracts.ErrInvalid, "profile %q persist %s: key has no value in settings", p.Name, k)
		}
	}
	for _, s := range p.Skills {
		if !nameRe.MatchString(s.Name) {
			return contracts.Errorf(contracts.ErrInvalid, "profile %q: invalid skill name %q", p.Name, s.Name)
		}
		if s.SourceDir == "" {
			return contracts.Errorf(contracts.ErrInvalid, "profile %q: skill %s has no source_dir", p.Name, s.Name)
		}
		if s.Delivery != "" && s.Delivery != DeliveryHome && s.Delivery != DeliveryOverlay {
			return contracts.Errorf(contracts.ErrInvalid, "profile %q: skill %s: unknown delivery %q", p.Name, s.Name, s.Delivery)
		}
	}
	for _, pl := range p.Plugins {
		if !nameRe.MatchString(pl.Name) {
			return contracts.Errorf(contracts.ErrInvalid, "profile %q: invalid plugin name %q", p.Name, pl.Name)
		}
		if pl.Harness != contracts.HarnessClaude && pl.Harness != contracts.HarnessCodex {
			return contracts.Errorf(contracts.ErrInvalid, "profile %q: plugin %s needs harness claude or codex", p.Name, pl.Name)
		}
	}
	for _, m := range p.MCPServers {
		if !nameRe.MatchString(m.Name) {
			return contracts.Errorf(contracts.ErrInvalid, "profile %q: invalid MCP server name %q", p.Name, m.Name)
		}
		if m.Command == "" && m.URL == "" {
			return contracts.Errorf(contracts.ErrInvalid, "profile %q: MCP server %s needs command or url", p.Name, m.Name)
		}
		for k, v := range m.Env {
			if SensitiveKey(k) || SensitiveValue(v) {
				return contracts.Errorf(contracts.ErrInvalid, "profile %q: MCP server %s env %s looks like a credential; use a connection grant", p.Name, m.Name, k)
			}
		}
		for _, a := range m.Args {
			if SensitiveValue(a) {
				return contracts.Errorf(contracts.ErrInvalid, "profile %q: MCP server %s has a credential-like argument; use a connection grant", p.Name, m.Name)
			}
		}
	}
	for _, h := range p.Hooks {
		if h.Harness != contracts.HarnessClaude && h.Harness != contracts.HarnessCodex {
			return contracts.Errorf(contracts.ErrInvalid, "profile %q: hook %s needs harness claude or codex", p.Name, h.Event)
		}
		if h.Event == "" || h.Command == "" {
			return contracts.Errorf(contracts.ErrInvalid, "profile %q: hook needs event and command", p.Name)
		}
	}
	return nil
}

// ProfileStore persists profiles under <state>/env/profiles.
type ProfileStore struct{ dir string }

func (s ProfileStore) path(projectID, name string) string {
	if name == "" {
		name = "default"
	}
	if projectID == "" {
		return filepath.Join(s.dir, "global", name+".json")
	}
	return filepath.Join(s.dir, "project", projectID, name+".json")
}

// Load returns the profile, or nil if none is defined (an empty layer).
func (s ProfileStore) Load(projectID, name string) (*Profile, error) {
	if projectID != "" && !contracts.ValidID(projectID, contracts.PrefixProject) {
		return nil, contracts.Errorf(contracts.ErrInvalid, "invalid project id %q", projectID)
	}
	if name != "" && !nameRe.MatchString(name) {
		return nil, contracts.Errorf(contracts.ErrInvalid, "invalid profile name %q", name)
	}
	b, err := os.ReadFile(s.path(projectID, name))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Profile
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("profile %s: %w", s.path(projectID, name), err)
	}
	if p.Name == "" {
		p.Name = name
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Save validates and stores a profile atomically.
func (s ProfileStore) Save(projectID string, p Profile) error {
	if p.Name == "" {
		p.Name = "default"
	}
	if projectID != "" && !contracts.ValidID(projectID, contracts.PrefixProject) {
		return contracts.Errorf(contracts.ErrInvalid, "invalid project id %q", projectID)
	}
	if !nameRe.MatchString(p.Name) {
		return contracts.Errorf(contracts.ErrInvalid, "invalid profile name %q", p.Name)
	}
	if err := p.Validate(); err != nil {
		return err
	}
	sort.Strings(p.Persist)
	return writeFileAtomic(s.path(projectID, p.Name), prettyJSON(p), 0o600)
}
