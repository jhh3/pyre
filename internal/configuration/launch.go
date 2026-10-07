package configuration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// Scope selects what a launch resolves.
type Scope struct {
	NodeID    string
	ProjectID string // optional
	SessionID string // optional; with Overrides forms the session layer
	Profile   string // profile name; "" = "default"
	Harness   contracts.Harness
	// ConfigHome is the isolated harness home bound to the run by the account
	// binder (CLAUDE_CONFIG_DIR / CODEX_HOME), or "" for the default home.
	ConfigHome string
	// Overrides are explicit session-layer values ("claude.model" -> "opus").
	Overrides map[string]string
	// AdapterHooks / AdapterMCP are the Pyre runtime's own hooks and MCP
	// servers (e.g. the Claude adapter's PermissionRequest broker and the
	// pyre MCP server). They are merged into the same overlay as managed
	// entries so the harness receives exactly one --settings file, and they
	// are deduplicated against identical user registrations.
	AdapterHooks []HookSpec
	AdapterMCP   []MCPServerSpec
}

// LaunchOverlay is what the daemon adds to a harness launch.
type LaunchOverlay struct {
	// ExtraArgs are global harness flags; place them before any subcommand
	// (e.g. codex -c k=v app-server ...). Claude: --settings, --mcp-config,
	// --plugin-dir. Codex: -c key=value pairs.
	ExtraArgs []string
	// Env is non-secret (PYRE_ENV_GENERATION). Credentials are resolved
	// separately by the connections/accounts integrations.
	Env map[string]string
	// Files are Pyre-owned files the run depends on; they are immutable for
	// the generation and protected from pruning while a run references it.
	Files []string
}

// Resolver is the interface the daemon calls at launch.
type Resolver interface {
	Resolve(ctx context.Context, s Scope) (contracts.EnvironmentGeneration, LaunchOverlay, error)
}

var _ Resolver = (*Manager)(nil)

// Resolve resolves the environment for a new run and returns an immutable
// generation plus the overlay to apply. Identical resolutions reuse one
// generation (by digest); otherwise a new launch generation is recorded with
// PreviousID = the scope's current applied generation. Resolve never writes
// to harness config homes: managed home items (skills, persisted keys) are
// materialized only by a reviewed env.apply.
func (m *Manager) Resolve(ctx context.Context, s Scope) (contracts.EnvironmentGeneration, LaunchOverlay, error) {
	if s.Harness != contracts.HarnessClaude && s.Harness != contracts.HarnessCodex {
		return contracts.EnvironmentGeneration{}, LaunchOverlay{}, contracts.Errorf(contracts.ErrUnsupported, "no configuration overlay for harness %q", s.Harness)
	}
	if s.NodeID != "" && m.opt.NodeID != "" && s.NodeID != m.opt.NodeID {
		return contracts.EnvironmentGeneration{}, LaunchOverlay{}, contracts.Errorf(contracts.ErrInvalid, "scope node %s is not this node (%s)", s.NodeID, m.opt.NodeID)
	}
	sc := scopeSpec{Kind: "global", Scope: "global", ProjectID: s.ProjectID, SessionID: s.SessionID, Profile: s.Profile,
		ConfigHome: s.ConfigHome, HomeHarness: s.Harness, AdapterHooks: s.AdapterHooks, AdapterMCP: s.AdapterMCP, Launch: true}
	if s.ProjectID != "" {
		if !contracts.ValidID(s.ProjectID, contracts.PrefixProject) {
			return contracts.EnvironmentGeneration{}, LaunchOverlay{}, contracts.Errorf(contracts.ErrInvalid, "invalid project id %q", s.ProjectID)
		}
		sc.Kind, sc.Scope = "project", "project:"+s.ProjectID
	}
	if len(s.Overrides) > 0 {
		if !contracts.ValidID(s.SessionID, contracts.PrefixSession) {
			return contracts.EnvironmentGeneration{}, LaunchOverlay{}, contracts.Errorf(contracts.ErrInvalid, "session overrides need a session id")
		}
		sc.Kind, sc.Scope, sc.Overrides = "session", "session:"+s.SessionID, s.Overrides
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	res, err := m.resolve(ctx, sc)
	if err != nil {
		return contracts.EnvironmentGeneration{}, LaunchOverlay{}, err
	}
	rec := m.reusable(res)
	if rec == nil {
		gen := res.gen
		gen.GenerationID = contracts.NewID(contracts.PrefixEnvironment)
		gen.CreatedAt = m.opt.Clock.Now()
		if prev := m.currentRecordFor(sc.baseScope()); prev != nil {
			gen.PreviousID = prev.Generation.GenerationID
			markRestart(&gen, &prev.Generation, m.opt.HotReloadKeys)
		}
		rec = &genRecord{Generation: gen, Scope: sc.Scope, Source: "launch", Overlay: res.overlay, Baseline: res.baseline}
		if err := m.writeGeneration(rec, res.overlayFiles, nil); err != nil {
			return contracts.EnvironmentGeneration{}, LaunchOverlay{}, err
		}
		_ = m.indexGeneration(gen.Digest, gen.GenerationID)
	}
	return rec.Generation, m.overlayFor(rec, s.Harness), nil
}

// reusable returns an existing intact generation with the same digest.
func (m *Manager) reusable(res *resolution) *genRecord {
	id, ok := m.loadIndex()[res.gen.Digest]
	if !ok {
		return nil
	}
	rec, err := m.loadRecord(id)
	if err != nil || rec.Generation.Digest != res.gen.Digest {
		return nil
	}
	for rel, d := range rec.Overlay.Files {
		got, ok, _ := liveDigest(filepath.Join(m.genDir(id), filepath.FromSlash(rel)))
		if !ok || got != d {
			return nil // tampered or pruned: mint a fresh generation
		}
	}
	return rec
}

func (m *Manager) genPath(id, p string) string {
	if strings.HasPrefix(p, genPrefix) {
		return filepath.Join(m.genDir(id), filepath.FromSlash(strings.TrimPrefix(p, genPrefix)))
	}
	return p
}

// overlayFor renders the launch overlay of a generation for one harness.
func (m *Manager) overlayFor(rec *genRecord, h contracts.Harness) LaunchOverlay {
	id := rec.Generation.GenerationID
	lo := LaunchOverlay{Env: map[string]string{"PYRE_ENV_GENERATION": id}}
	switch h {
	case contracts.HarnessClaude:
		if rec.Overlay.ClaudeSettings != "" {
			p := m.genPath(id, rec.Overlay.ClaudeSettings)
			lo.ExtraArgs = append(lo.ExtraArgs, "--settings="+p)
			lo.Files = append(lo.Files, p)
		}
		if rec.Overlay.ClaudeMCP != "" {
			p := m.genPath(id, rec.Overlay.ClaudeMCP)
			lo.ExtraArgs = append(lo.ExtraArgs, "--mcp-config="+p) // "=" form: the flag is variadic
			lo.Files = append(lo.Files, p)
		}
		for _, d := range rec.Overlay.ClaudePluginDirs {
			p := m.genPath(id, d)
			if _, err := os.Stat(p); err != nil {
				continue // a pinned plugin dir vanished; drift reports it
			}
			lo.ExtraArgs = append(lo.ExtraArgs, "--plugin-dir="+p)
			lo.Files = append(lo.Files, p)
		}
	case contracts.HarnessCodex:
		lo.ExtraArgs = append(lo.ExtraArgs, rec.Overlay.CodexArgs...)
	}
	return lo
}

// LaunchConnections returns the connection IDs the active managed profiles
// (global, plus the project's when the scope has one) request for a launch,
// filtered by the current grants for the scope. It applies to every harness,
// including plain shells, which have no configuration overlay: a shell
// session can still receive a connection credential provisioned on this
// node. The profiles are read exactly as Resolve reads them (the active
// store, never a pending desired profile); nothing is written.
func (m *Manager) LaunchConnections(ctx context.Context, s Scope) ([]string, error) {
	if s.NodeID != "" && m.opt.NodeID != "" && s.NodeID != m.opt.NodeID {
		return nil, contracts.Errorf(contracts.ErrInvalid, "scope node %s is not this node (%s)", s.NodeID, m.opt.NodeID)
	}
	if s.ProjectID != "" && !contracts.ValidID(s.ProjectID, contracts.PrefixProject) {
		return nil, contracts.Errorf(contracts.ErrInvalid, "invalid project id %q", s.ProjectID)
	}
	sc := scopeSpec{Profile: s.Profile}
	scopes := []string{""}
	if s.ProjectID != "" {
		scopes = append(scopes, s.ProjectID)
	}
	var requested []string
	seen := map[string]bool{}
	m.mu.Lock()
	for _, pid := range scopes {
		p, err := m.profileFor(sc, pid)
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		if p == nil {
			continue
		}
		for _, c := range p.Connections {
			if !seen[c] {
				seen[c] = true
				requested = append(requested, c)
			}
		}
	}
	m.mu.Unlock()
	if len(requested) == 0 || m.opt.Grants == nil {
		return requested, nil
	}
	granted, err := m.opt.Grants.GrantedConnections(ctx, GrantScope{NodeID: m.opt.NodeID, ProjectID: s.ProjectID, SessionID: s.SessionID}, requested)
	if err != nil {
		return nil, fmt.Errorf("connection grants: %w", err)
	}
	return granted, nil
}
