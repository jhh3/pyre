package configuration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/extensions"
)

// scopeSpec is a parsed resolution scope.
type scopeSpec struct {
	Kind      string // "global" | "project" | "session"
	Scope     string // contract form: "global" | "project:<id>" | "session:<id>"
	ProjectID string
	SessionID string
	Profile   string
	Overrides map[string]string
	// Launch-only inputs.
	ConfigHome   string            // isolated CLAUDE_CONFIG_DIR / CODEX_HOME bound to the run
	HomeHarness  contracts.Harness // harness ConfigHome belongs to
	AdapterHooks []HookSpec
	AdapterMCP   []MCPServerSpec
	// Desired (plans and previews) reads a pending desired profile written by
	// env.profile.set (<state>/env/desired) before the active profile store;
	// launches read the active store only.
	Desired bool
	// Launch is set for Resolve (a new run); the launcher reports its own
	// unsupported overlays there.
	Launch bool
}

// baseScope is the applied scope a session/launch inherits from.
func (s scopeSpec) baseScope() string {
	if s.ProjectID != "" {
		return "project:" + s.ProjectID
	}
	return "global"
}

func parseScope(p contracts.EnvPlanParams) (scopeSpec, error) {
	s := scopeSpec{Profile: p.Profile, Overrides: p.Overrides, ProjectID: p.ProjectID, Desired: true}
	switch {
	case p.Scope == "" || p.Scope == "global":
		s.Kind, s.Scope = "global", "global"
		if p.ProjectID != "" {
			return s, contracts.Errorf(contracts.ErrInvalid, "scope global does not take project_id; use scope project:%s", p.ProjectID)
		}
	case strings.HasPrefix(p.Scope, "project:"):
		id := strings.TrimPrefix(p.Scope, "project:")
		if !contracts.ValidID(id, contracts.PrefixProject) {
			return s, contracts.Errorf(contracts.ErrInvalid, "invalid project id in scope %q", p.Scope)
		}
		if p.ProjectID != "" && p.ProjectID != id {
			return s, contracts.Errorf(contracts.ErrInvalid, "scope %q disagrees with project_id %q", p.Scope, p.ProjectID)
		}
		s.Kind, s.Scope, s.ProjectID = "project", p.Scope, id
	case strings.HasPrefix(p.Scope, "session:"):
		id := strings.TrimPrefix(p.Scope, "session:")
		if !contracts.ValidID(id, contracts.PrefixSession) {
			return s, contracts.Errorf(contracts.ErrInvalid, "invalid session id in scope %q", p.Scope)
		}
		if p.ProjectID != "" && !contracts.ValidID(p.ProjectID, contracts.PrefixProject) {
			return s, contracts.Errorf(contracts.ErrInvalid, "invalid project_id %q", p.ProjectID)
		}
		s.Kind, s.Scope, s.SessionID = "session", p.Scope, id
	default:
		return s, contracts.Errorf(contracts.ErrInvalid, "scope %q: want global, project:<id> or session:<id>", p.Scope)
	}
	if s.Kind != "session" && len(p.Overrides) > 0 {
		return s, contracts.Errorf(contracts.ErrInvalid, "overrides are session-layer values; use scope session:<id> (global/project values belong in a profile)")
	}
	return s, nil
}

// rvalue is one merged setting.
type rvalue struct {
	value    any
	layer    contracts.ConfigLayer
	overrode []contracts.ConfigLayer
}

func layerRank(l contracts.ConfigLayer) int {
	for i, x := range contracts.LayerOrder {
		if x == l {
			return i
		}
	}
	return -1
}

func managedLayer(l contracts.ConfigLayer) bool {
	return layerRank(l) >= layerRank(contracts.LayerManagedGlobal)
}

// desiredTarget is a managed config-home target (global scope only).
type desiredTarget struct {
	Kind    string // "dir" | "json_keys"
	Path    string
	Display string
	Name    string
	Files   map[string][]byte // dir content without marker
	Keys    map[string]any    // json_keys: in-file key -> value
	Layer   contracts.ConfigLayer
	Version string
}

func (t desiredTarget) digest() contracts.Digest {
	if t.Kind == "dir" {
		return extensions.DigestFileMap(t.Files)
	}
	return extensions.DigestStrings(canon(t.Keys))
}

// resolution is the full output of one merge.
type resolution struct {
	spec         scopeSpec
	gen          contracts.EnvironmentGeneration // without ID/CreatedAt/PreviousID
	values       map[string]*rvalue
	overlay      overlayRecord
	overlayFiles map[string][]byte // generation-relative path -> content
	targets      []desiredTarget
	conflicts    []contracts.DriftItem
	notes        []contracts.PlanChange
	baseline     baseline
	inv          extensions.Inventory
	// global/project are the managed profiles this resolution used.
	global, project *Profile
}

const genPrefix = "@gen/"

// resolve merges all layers for a scope. It reads live files but writes
// nothing.
func (m *Manager) resolve(ctx context.Context, sc scopeSpec) (*resolution, error) {
	homes := m.opt.Homes
	var projectDirs []string
	if sc.ProjectID != "" && m.opt.ProjectDirs != nil {
		projectDirs = m.opt.ProjectDirs(ctx, sc.ProjectID)
	}
	homes.ProjectDirs = projectDirs
	userClaudeJSON := homes.ClaudeJSON
	isolatedClaude := false
	if sc.ConfigHome != "" {
		switch sc.HomeHarness {
		case contracts.HarnessClaude:
			if filepath.Clean(sc.ConfigHome) != filepath.Clean(homes.ClaudeHome) {
				homes.ClaudeHome = sc.ConfigHome
				homes.ClaudeJSON = filepath.Join(sc.ConfigHome, ".claude.json")
				isolatedClaude = true
			}
		case contracts.HarnessCodex:
			homes.CodexHome = sc.ConfigHome
		}
	}
	inv, err := extensions.Scan(ctx, homes, extensions.ScanOptions{Managed: m.opt.Managed})
	if err != nil {
		return nil, err
	}
	r := &resolution{spec: sc, values: map[string]*rvalue{}, overlayFiles: map[string][]byte{}, inv: inv}
	r.overlay.Files = map[string]contracts.Digest{}

	global, err := m.profileFor(sc, "")
	if err != nil {
		return nil, err
	}
	var project *Profile
	if sc.ProjectID != "" {
		if project, err = m.profileFor(sc, sc.ProjectID); err != nil {
			return nil, err
		}
	}
	r.global, r.project = global, project

	// ---- values, lowest to highest precedence ----
	put := func(l contracts.ConfigLayer, k string, v any) {
		v = normalize(v)
		if cur, ok := r.values[k]; ok {
			if cur.layer == l {
				cur.value = v // same layer, later source wins (e.g. settings.local.json)
				return
			}
			cur.overrode = append(cur.overrode, cur.layer)
			cur.layer, cur.value = l, v
			return
		}
		r.values[k] = &rvalue{value: v, layer: l}
	}
	for k, v := range m.opt.ProviderDefaults {
		put(contracts.LayerProviderDefault, k, v)
	}
	adopted, warns := adoptedValues(homes, projectDirs)
	for _, w := range warns {
		r.inv.Warnings = append(r.inv.Warnings, w)
	}
	for k := range m.ownedKeys() {
		delete(adopted, k) // Pyre-owned keys inside user files are managed, not adopted
	}
	r.baseline.Settings = map[string]string{}
	for _, k := range sortedMapKeys(adopted) {
		put(contracts.LayerAdoptedUser, k, adopted[k])
		r.baseline.Settings[k] = redactValue(k, normalize(adopted[k]))
	}
	if global != nil {
		for _, k := range sortedMapKeys(global.Settings) {
			put(contracts.LayerManagedGlobal, k, global.Settings[k])
		}
	}
	if project != nil {
		for _, k := range sortedMapKeys(project.Settings) {
			put(contracts.LayerManagedProject, k, project.Settings[k])
		}
	}
	if sc.Kind == "session" {
		keys := make([]string, 0, len(sc.Overrides))
		for k := range sc.Overrides {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, _, err := harnessOf(k); err != nil {
				return nil, contracts.Errorf(contracts.ErrInvalid, "override: %v", err)
			}
			v := ParseValue(sc.Overrides[k])
			if SensitiveKey(k) || SensitiveValue(v) {
				return nil, contracts.Errorf(contracts.ErrInvalid, "override %s looks like a credential; pass secrets through a connection grant, not configuration", k)
			}
			put(contracts.LayerSession, k, v)
		}
	}

	// ---- overlays from managed/session values ----
	claudeSettings := map[string]any{}
	var codexArgs []string
	for _, k := range sortedRKeys(r.values) {
		rv := r.values[k]
		if !managedLayer(rv.layer) {
			continue
		}
		h, segs, _ := harnessOf(k)
		switch h {
		case contracts.HarnessClaude:
			if err := setPath(claudeSettings, segs, rv.value); err != nil {
				r.conflict("claude --settings overlay", k, fmt.Sprintf("cannot set %s: %v", k, err))
			}
		case contracts.HarnessCodex:
			path, ok := codexOverridePath(segs)
			if !ok {
				r.conflict("codex -c overlay", k, "codex -c splits keys on '.', so this key cannot be overlaid; not applied (unsupported, user config untouched)")
				continue
			}
			tv, err := tomlValue(rv.value)
			if err != nil {
				r.conflict("codex -c overlay", k, err.Error())
				continue
			}
			codexArgs = append(codexArgs, "-c", path+"="+tv)
		}
	}

	// ---- extensions ----
	var profiles []*Profile
	var layers []contracts.ConfigLayer
	if global != nil {
		profiles, layers = append(profiles, global), append(layers, contracts.LayerManagedGlobal)
	}
	if project != nil {
		profiles, layers = append(profiles, project), append(layers, contracts.LayerManagedProject)
	}

	// Skills.
	bundle := map[string][]byte{}
	managedSkill := map[string]bool{}
	for i, p := range profiles {
		for _, s := range p.Skills {
			files, err := readTree(s.SourceDir)
			if err != nil {
				r.conflict(s.SourceDir, "skill:"+s.Name, "skill source unreadable: "+err.Error())
				continue
			}
			if _, ok := files["SKILL.md"]; !ok {
				r.conflict(s.SourceDir, "skill:"+s.Name, "skill source has no SKILL.md")
				continue
			}
			delete(files, extensions.MarkerFile)
			d := extensions.DigestFileMap(files)
			if s.Digest != "" && s.Digest != d {
				r.conflict(s.SourceDir, "skill:"+s.Name, fmt.Sprintf("skill source digest %s does not match declared %s; refusing to install unverified content", d, s.Digest))
				continue
			}
			for _, h := range harnessesFor(s.Harness) {
				managedSkill[string(h)+":"+s.Name] = true
				ref := contracts.ExtensionRef{Name: s.Name, Harness: h, Version: s.Version, Digest: d, Layer: layers[i], Managed: true}
				delivery := s.Delivery
				if delivery == "" {
					delivery = DeliveryHome
					if layers[i] != contracts.LayerManagedGlobal && h == contracts.HarnessClaude {
						delivery = DeliveryOverlay
					}
				}
				switch {
				case h == contracts.HarnessClaude && delivery == DeliveryOverlay:
					for rel, b := range files {
						bundle["skills/"+s.Name+"/"+rel] = b
					}
					ref.Source = "skill@generation:claude/plugins/pyre-managed/skills/" + s.Name
				case layers[i] != contracts.LayerManagedGlobal:
					r.conflict(string(h)+":skills", "skill:"+s.Name, fmt.Sprintf("%s has no per-run skill overlay; a %s skill can only be delivered globally (delivery=home in the global profile)", h, layers[i]))
					continue
				case delivery == DeliveryOverlay:
					r.conflict(string(h)+":skills", "skill:"+s.Name, "overlay delivery is only supported for claude (--plugin-dir)")
					continue
				default:
					home := homes.ClaudeHome
					if h == contracts.HarnessCodex {
						home = homes.CodexHome
					}
					p := filepath.Join(home, "skills", s.Name)
					r.targets = append(r.targets, desiredTarget{Kind: "dir", Path: p, Display: m.display(p), Name: s.Name, Files: files, Layer: layers[i], Version: s.Version})
					ref.Source = "skill@" + m.display(p)
				}
				r.gen.Skills = append(r.gen.Skills, ref)
			}
		}
	}
	for _, it := range inv.OfKind(extensions.KindSkill, "") {
		if it.Managed && it.Scope != "outfit" && managedSkill[string(it.Harness)+":"+it.Name] {
			continue // listed above as managed
		}
		r.gen.Skills = append(r.gen.Skills, it.Ref())
	}
	if len(bundle) > 0 {
		bundle[".claude-plugin/plugin.json"] = prettyJSON(map[string]any{
			"name": "pyre-managed", "version": "0.0.0-" + string(extensions.DigestFileMap(bundle))[7:19],
			"description": "Pyre-managed skills pinned to one environment generation",
		})
		for rel, b := range bundle {
			r.overlayFiles["claude/plugins/pyre-managed/"+rel] = b
		}
		r.overlay.ClaudePluginDirs = append(r.overlay.ClaudePluginDirs, genPrefix+"claude/plugins/pyre-managed")
	}

	// Plugins.
	installed := map[string]extensions.Item{}
	for _, it := range inv.OfKind(extensions.KindPlugin, "") {
		installed[string(it.Harness)+":"+it.Name] = it
	}
	managedPlugin := map[string]bool{}
	for i, p := range profiles {
		for _, pl := range p.Plugins {
			en := pl.Enabled == nil || *pl.Enabled
			ref := contracts.ExtensionRef{Name: pl.Name, Harness: pl.Harness, Version: pl.Version, Layer: layers[i], Managed: true}
			managedPlugin[string(pl.Harness)+":"+pl.Name] = true
			dir := pl.Dir
			if dir == "" && m.opt.Versions != nil {
				if pin, ok := m.opt.Versions.Pinned(ctx, "plugin:"+pl.Name); ok {
					dir, ref.Version, ref.Digest = pin.Path, pin.Version, pin.Digest
				}
			}
			switch pl.Harness {
			case contracts.HarnessClaude:
				if dir != "" {
					if en {
						r.overlay.ClaudePluginDirs = append(r.overlay.ClaudePluginDirs, dir)
					}
					ref.Source = "plugin@" + m.display(dir)
				} else {
					if _, ok := installed["claude:"+pl.Name]; !ok && en {
						r.conflicts = append(r.conflicts, contracts.DriftItem{Path: "claude:plugins", Key: pl.Name, Kind: "missing",
							Detail: "plugin is not installed on this node; install it (claude plugin install) or give a dir / stage plugin:" + pl.Name})
					}
					if err := setPath(claudeSettings, []string{"enabledPlugins", pl.Name}, en); err != nil {
						r.conflict("claude --settings overlay", "enabledPlugins."+pl.Name, err.Error())
					}
					ref.Source = "plugin@settings-overlay"
				}
			case contracts.HarnessCodex:
				path, ok := codexOverridePath([]string{"plugins", pl.Name, "enabled"})
				if !ok {
					r.conflict("codex -c overlay", "plugins."+pl.Name, "plugin name cannot be addressed with -c")
					continue
				}
				if _, ok := installed["codex:"+pl.Name]; !ok && en {
					r.conflicts = append(r.conflicts, contracts.DriftItem{Path: "codex:plugins", Key: pl.Name, Kind: "missing", Detail: "plugin is not installed for codex on this node"})
				}
				codexArgs = append(codexArgs, "-c", path+"="+fmt.Sprint(en))
				ref.Source = "plugin@codex-overlay"
			}
			r.gen.Plugins = append(r.gen.Plugins, ref)
		}
	}
	for _, it := range inv.OfKind(extensions.KindPlugin, "") {
		if !managedPlugin[string(it.Harness)+":"+it.Name] {
			r.gen.Plugins = append(r.gen.Plugins, it.Ref())
		}
	}

	// MCP servers (profiles + Pyre runtime servers from the adapter).
	type mcpDecl struct {
		spec    MCPServerSpec
		layer   contracts.ConfigLayer
		runtime bool // the Pyre runtime's own server (always wins)
	}
	var decls []mcpDecl
	for i, p := range profiles {
		for _, s := range p.MCPServers {
			decls = append(decls, mcpDecl{spec: s, layer: layers[i]})
		}
	}
	for _, s := range sc.AdapterMCP {
		decls = append(decls, mcpDecl{spec: s, layer: contracts.LayerManagedGlobal, runtime: true})
	}
	// One winner per harness and name: a higher managed layer (project over
	// global) replaces the lower definition; two declarations in the same
	// layer, or a managed server named like a Pyre runtime server, are
	// conflicts (the later or runtime declaration is used).
	type mcpPick struct {
		d mcpDecl
		h contracts.Harness
	}
	rank := func(d mcpDecl) int {
		if d.runtime {
			return 1 << 20
		}
		return layerRank(d.layer)
	}
	var picks []mcpPick
	pickAt := map[string]int{}
	for _, d := range decls {
		for _, h := range harnessesFor(d.spec.Harness) {
			id := extensions.MCPIdentity(h, d.spec.Name)
			i, dup := pickAt[id]
			if !dup {
				pickAt[id] = len(picks)
				picks = append(picks, mcpPick{d, h})
				continue
			}
			prev := picks[i].d
			switch {
			case rank(d) == rank(prev) || d.runtime || prev.runtime:
				r.conflict(string(h)+":mcp", d.spec.Name, "declared more than once in the same managed layer (or named like a Pyre runtime server); the later declaration wins")
				if rank(d) >= rank(prev) {
					picks[i] = mcpPick{d, h}
				}
			case rank(d) > rank(prev):
				r.notes = append(r.notes, contracts.PlanChange{Path: string(h) + ":mcp:" + d.spec.Name, Action: "register",
					Diff: "~ " + string(d.layer) + " definition overrides " + string(prev.layer)})
				picks[i] = mcpPick{d, h}
			default:
				r.notes = append(r.notes, contracts.PlanChange{Path: string(h) + ":mcp:" + d.spec.Name, Action: "register",
					Diff: "~ " + string(prev.layer) + " definition overrides " + string(d.layer)})
			}
		}
	}
	adoptedMCP := map[string]extensions.Item{}
	for _, it := range inv.OfKind(extensions.KindMCPServer, "") {
		if _, ok := adoptedMCP[it.Identity]; !ok {
			adoptedMCP[it.Identity] = it
		}
	}
	claudeMCP := map[string]any{}
	reused := map[string]bool{}
	seenManaged := map[string]bool{}
	for _, pk := range picks {
		d := pk.d
		def := extensions.MCPDef{Name: d.spec.Name, Transport: d.spec.Transport, Command: d.spec.Command, Args: d.spec.Args, URL: d.spec.URL, Env: d.spec.Env}
		def.Normalize()
		for _, h := range []contracts.Harness{pk.h} {
			id := extensions.MCPIdentity(h, d.spec.Name)
			seenManaged[id] = true
			ref := contracts.ExtensionRef{Name: d.spec.Name, Harness: h, Digest: def.DefDigest, Layer: d.layer, Managed: true}
			if ex, ok := adoptedMCP[id]; ok {
				if ex.MCP != nil && ex.MCP.DefDigest == def.DefDigest {
					reused[id] = true
					r.notes = append(r.notes, contracts.PlanChange{Path: string(h) + ":mcp:" + d.spec.Name, Action: "register",
						Diff: "= identical server already registered in " + ex.Location + "; reused, not registered twice"})
					continue
				}
				r.conflicts = append(r.conflicts, contracts.DriftItem{Path: ex.Location, Key: "mcp:" + d.spec.Name, Kind: "conflict",
					Detail: "an MCP server with this name is already registered with a different definition; rename the managed server or remove the user entry (user config is not overwritten)"})
				continue
			}
			switch h {
			case contracts.HarnessClaude:
				claudeMCP[d.spec.Name] = mcpJSON(def)
				ref.Source = "mcp_server@generation:claude/mcp.json"
			case contracts.HarnessCodex:
				args, err := codexMCPArgs(def)
				if err != nil {
					r.conflict("codex -c overlay", "mcp:"+d.spec.Name, err.Error())
					continue
				}
				codexArgs = append(codexArgs, args...)
				ref.Source = "mcp_server@codex-overlay"
			}
			r.notes = append(r.notes, contracts.PlanChange{Path: string(h) + ":mcp:" + d.spec.Name, Action: "register"})
			r.gen.MCPServers = append(r.gen.MCPServers, ref)
		}
	}
	if isolatedClaude && userClaudeJSON != "" {
		// The isolated profile has its own .claude.json; compose the user's
		// user-scope servers per run instead of editing harness-owned files.
		userInv, _ := extensions.Scan(ctx, extensions.Homes{Home: homes.Home, ClaudeJSON: userClaudeJSON}, extensions.ScanOptions{})
		for _, it := range userInv.OfKind(extensions.KindMCPServer, contracts.HarnessClaude) {
			if it.Scope != "user" || it.MCP == nil {
				continue
			}
			if _, dup := adoptedMCP[it.Identity]; dup || seenManaged[it.Identity] {
				continue
			}
			if _, dup := claudeMCP[it.Name]; dup {
				continue
			}
			if it.MCP.HasSecretMaterial() {
				r.conflicts = append(r.conflicts, contracts.DriftItem{Path: it.Location, Key: "mcp:" + it.Name, Kind: "missing",
					Detail: "user MCP server carries env/header values; not copied into the isolated profile overlay (provide it through an Outfit connection)"})
				continue
			}
			claudeMCP[it.Name] = mcpJSON(*it.MCP)
			ref := it.Ref()
			ref.Source = "mcp_server@" + it.Location + " (composed into isolated profile)"
			r.gen.MCPServers = append(r.gen.MCPServers, ref)
		}
	}
	for _, it := range inv.OfKind(extensions.KindMCPServer, "") {
		r.gen.MCPServers = append(r.gen.MCPServers, it.Ref())
	}

	// Hooks (profiles + Pyre runtime hooks), deduplicated by identity.
	adoptedHooks := map[string]extensions.Item{}
	for _, it := range inv.OfKind(extensions.KindHook, "") {
		adoptedHooks[it.Identity] = it
	}
	var hooks []HookSpec
	for _, p := range profiles {
		hooks = append(hooks, p.Hooks...)
	}
	hooks = append(hooks, sc.AdapterHooks...)
	claudeHooks := map[string][]any{}
	codexHooks := map[string][]HookSpec{}
	seenHook := map[string]bool{}
	for _, h := range hooks {
		id := extensions.HookIdentity(h.Harness, h.Event, h.Matcher, h.Command)
		if seenHook[id] {
			continue
		}
		seenHook[id] = true
		if ex, ok := adoptedHooks[id]; ok {
			r.notes = append(r.notes, contracts.PlanChange{Path: string(h.Harness) + ":hook:" + h.Event, Action: "register",
				Diff: "= identical hook already registered in " + ex.Location + "; reused, not registered twice"})
			continue
		}
		switch h.Harness {
		case contracts.HarnessClaude:
			claudeHooks[h.Event] = appendHookGroup(claudeHooks[h.Event], h)
		case contracts.HarnessCodex:
			codexHooks[h.Event] = append(codexHooks[h.Event], h)
		}
		r.notes = append(r.notes, contracts.PlanChange{Path: string(h.Harness) + ":hook:" + h.Event, Action: "register"})
	}
	if len(claudeHooks) > 0 {
		hm := map[string]any{}
		for ev, groups := range claudeHooks {
			hm[ev] = groups
		}
		claudeSettings["hooks"] = hm
	}
	if len(codexHooks) > 0 {
		args, err := codexHookArgs(homes.CodexHome, codexHooks)
		if err != nil {
			r.conflict("codex -c overlay", "hooks", err.Error())
		} else {
			codexArgs = append(codexArgs, args...)
		}
	}

	// Persisted keys (global scope): managed keys inside the user's
	// settings.json. They are also kept in the overlay so Pyre runs get them
	// even before an apply.
	if sc.Kind == "global" && global != nil && len(global.Persist) > 0 {
		keys := map[string]any{}
		for _, k := range global.Persist {
			_, segs, _ := harnessOf(k)
			rv := r.values[k]
			if rv == nil {
				continue
			}
			keys[JoinKey(segs...)] = rv.value
		}
		p := filepath.Join(m.opt.Homes.ClaudeHome, "settings.json")
		r.targets = append(r.targets, desiredTarget{Kind: "json_keys", Path: p, Display: m.display(p) + "#pyre-managed", Keys: keys, Layer: contracts.LayerManagedGlobal})
	}

	// ---- overlay files ----
	if len(claudeSettings) > 0 && m.opt.ClaudeSettingsUnsupported != "" && !sc.Launch {
		r.conflict("claude --settings overlay", strings.Join(sortedMapKeys(claudeSettings), ","), m.opt.ClaudeSettingsUnsupported)
	}
	if len(codexArgs) > 0 && m.opt.CodexOverlayUnsupported != "" && !sc.Launch {
		r.conflict("codex -c overlay", strings.Join(codexArgs, " "), m.opt.CodexOverlayUnsupported)
	}
	if len(claudeSettings) > 0 {
		b := prettyJSON(claudeSettings)
		r.overlayFiles["claude/settings.json"] = b
		r.overlay.ClaudeSettings = genPrefix + "claude/settings.json"
	}
	if len(claudeMCP) > 0 {
		r.overlayFiles["claude/mcp.json"] = prettyJSON(map[string]any{"mcpServers": claudeMCP})
		r.overlay.ClaudeMCP = genPrefix + "claude/mcp.json"
	}
	r.overlay.CodexArgs = codexArgs
	for rel, b := range r.overlayFiles {
		r.overlay.Files[rel] = extensions.DigestBytes(b)
	}

	// ---- connections ----
	var requested []string
	seenConn := map[string]bool{}
	for _, p := range profiles {
		for _, c := range p.Connections {
			if !seenConn[c] {
				seenConn[c] = true
				requested = append(requested, c)
			}
		}
	}
	granted := requested
	if m.opt.Grants != nil && len(requested) > 0 {
		g, err := m.opt.Grants.GrantedConnections(ctx, GrantScope{NodeID: m.opt.NodeID, ProjectID: sc.ProjectID, SessionID: sc.SessionID}, requested)
		if err != nil {
			return nil, fmt.Errorf("connection grants: %w", err)
		}
		granted = g
		gs := map[string]bool{}
		for _, c := range g {
			gs[c] = true
		}
		for _, c := range requested {
			if !gs[c] {
				r.conflicts = append(r.conflicts, contracts.DriftItem{Path: "connections", Key: c, Kind: "missing", Detail: "connection requested by the profile is not granted for this scope"})
			}
		}
	}

	// ---- generation ----
	g := &r.gen
	g.NodeID = m.opt.NodeID
	g.Profile = orDefault(sc.Profile, "default")
	if sc.ProjectID != "" {
		pid := sc.ProjectID
		g.ProjectID = &pid
	}
	g.RuntimeVersions = map[string]string{}
	if m.opt.Versions != nil {
		for k, v := range m.opt.Versions.ActiveVersions(ctx) {
			g.RuntimeVersions[k] = v
		}
	}
	g.ConfigDigests = map[string]contracts.Digest{}
	for rel, d := range r.overlay.Files {
		g.ConfigDigests["generation:"+rel] = d
	}
	for _, t := range r.targets {
		g.ConfigDigests[t.Display] = t.digest()
	}
	g.ConnectionGrants = append([]string{}, granted...)
	for _, k := range sortedRKeys(r.values) {
		rv := r.values[k]
		g.Values = append(g.Values, contracts.ResolvedValue{Key: k, Value: redactValue(k, rv.value), Layer: rv.layer, Overrode: rv.overrode})
	}
	sortRefs(g.Skills)
	sortRefs(g.Plugins)
	sortRefs(g.MCPServers)
	for _, it := range inv.Items {
		r.baseline.Identities = append(r.baseline.Identities, it.Identity)
	}
	sort.Strings(r.baseline.Identities)
	g.Digest = m.digestGeneration(r)
	return r, nil
}

// profileFor returns the managed profile of the global scope (projectID "")
// or a project. Plans and previews prefer a pending desired profile
// (env.profile.set); launches read the active store, which only a reviewed
// env.apply or env.rollback changes (or the owner editing it by hand).
func (m *Manager) profileFor(sc scopeSpec, projectID string) (*Profile, error) {
	if sc.Desired {
		p, err := m.desired.Load(projectID, sc.Profile)
		if err != nil || p != nil {
			return p, err
		}
	}
	return m.profiles.Load(projectID, sc.Profile)
}

// snapshotFor records the scope's own profile for a planned generation.
func (r *resolution) snapshotFor(sc scopeSpec) *profileSnapshot {
	var p *Profile
	switch sc.Kind {
	case "global":
		p = r.global
	case "project":
		p = r.project
	default:
		return nil
	}
	s := &profileSnapshot{Name: orDefault(sc.Profile, "default"), Present: p != nil}
	if p != nil {
		cp := *p
		s.Profile = &cp
	}
	return s
}

func (r *resolution) conflict(path, key, detail string) {
	r.conflicts = append(r.conflicts, contracts.DriftItem{Path: path, Key: key, Kind: "conflict", Detail: detail})
}

// digestGeneration digests everything that defines the generation's effect.
// Secret values contribute only through their own digest.
func (m *Manager) digestGeneration(r *resolution) contracts.Digest {
	g := r.gen
	parts := []string{"profile", g.Profile, "node", g.NodeID}
	if g.ProjectID != nil {
		parts = append(parts, "project", *g.ProjectID)
	}
	for _, k := range sortedRKeys(r.values) {
		rv := r.values[k]
		parts = append(parts, "v", k, string(rv.layer), string(extensions.DigestStrings(canon(rv.value))))
	}
	for _, set := range [][]contracts.ExtensionRef{g.Skills, g.Plugins, g.MCPServers} {
		parts = append(parts, "refs")
		for _, e := range set {
			parts = append(parts, e.Name, string(e.Harness), e.Version, string(e.Digest), string(e.Layer), fmt.Sprint(e.Managed), e.Source)
		}
	}
	parts = append(parts, "conns")
	parts = append(parts, g.ConnectionGrants...)
	parts = append(parts, "versions")
	for _, k := range sortedStrKeys(g.RuntimeVersions) {
		parts = append(parts, k, g.RuntimeVersions[k])
	}
	parts = append(parts, "digests")
	for _, k := range sortedDigestKeys(g.ConfigDigests) {
		parts = append(parts, k, string(g.ConfigDigests[k]))
	}
	parts = append(parts, "codex")
	parts = append(parts, r.overlay.CodexArgs...)
	parts = append(parts, "plugin-dirs")
	parts = append(parts, r.overlay.ClaudePluginDirs...)
	return extensions.DigestStrings(parts...)
}

func harnessesFor(h contracts.Harness) []contracts.Harness {
	if h == "" {
		return []contracts.Harness{contracts.HarnessClaude, contracts.HarnessCodex}
	}
	return []contracts.Harness{h}
}

func appendHookGroup(groups []any, h HookSpec) []any {
	entry := map[string]any{"type": "command", "command": h.Command}
	if h.Timeout > 0 {
		entry["timeout"] = int64(h.Timeout)
	}
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		matcher, _ := gm["matcher"].(string)
		if matcher == h.Matcher {
			existing, _ := gm["hooks"].([]any)
			gm["hooks"] = append(existing, entry)
			return groups
		}
	}
	gm := map[string]any{"hooks": []any{entry}}
	if h.Matcher != "" {
		gm["matcher"] = h.Matcher
	}
	return append(groups, gm)
}

func mcpJSON(d extensions.MCPDef) map[string]any {
	out := map[string]any{}
	if d.Transport != "" && d.Transport != "stdio" {
		out["type"] = d.Transport
	}
	if d.Command != "" {
		out["command"] = d.Command
	}
	if len(d.Args) > 0 {
		a := make([]any, len(d.Args))
		for i, s := range d.Args {
			a[i] = s
		}
		out["args"] = a
	}
	if d.URL != "" {
		out["url"] = d.URL
	}
	if len(d.Env) > 0 {
		e := map[string]any{}
		for k, v := range d.Env {
			e[k] = v
		}
		out["env"] = e
	}
	if len(d.Headers) > 0 {
		e := map[string]any{}
		for k, v := range d.Headers {
			e[k] = v
		}
		out["headers"] = e
	}
	return out
}

func codexMCPArgs(d extensions.MCPDef) ([]string, error) {
	base, ok := codexOverridePath([]string{"mcp_servers", d.Name})
	if !ok {
		return nil, fmt.Errorf("MCP server name %q cannot be addressed with -c", d.Name)
	}
	var out []string
	add := func(k string, v any) error {
		tv, err := tomlValue(v)
		if err != nil {
			return err
		}
		out = append(out, "-c", base+"."+k+"="+tv)
		return nil
	}
	if d.Command != "" {
		if err := add("command", d.Command); err != nil {
			return nil, err
		}
		args := d.Args
		if args == nil {
			args = []string{}
		}
		if err := add("args", args); err != nil {
			return nil, err
		}
	}
	if d.URL != "" {
		if err := add("url", d.URL); err != nil {
			return nil, err
		}
	}
	if len(d.Env) > 0 {
		if err := add("env", d.Env); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// codexHookArgs renders managed Codex hooks as -c hooks.<Event>=[...]. A -c
// array replaces the same key from config.toml, so hooks the user declared
// inline in config.toml for that event are carried over first (hooks.json
// is a separate source and is never affected).
func codexHookArgs(codexHome string, hooks map[string][]HookSpec) ([]string, error) {
	var userHooks map[string]any
	if codexHome != "" {
		if cfg, err := extensions.ReadTOML(filepath.Join(codexHome, "config.toml")); err == nil {
			userHooks, _ = cfg["hooks"].(map[string]any)
		}
	}
	events := make([]string, 0, len(hooks))
	for ev := range hooks {
		events = append(events, ev)
	}
	sort.Strings(events)
	var out []string
	for _, ev := range events {
		path, ok := codexOverridePath([]string{"hooks", ev})
		if !ok {
			return nil, fmt.Errorf("hook event %q cannot be addressed with -c", ev)
		}
		var groups []any
		if uh, ok := userHooks[ev]; ok {
			if arr, ok := normalize(uh).([]any); ok {
				groups = append(groups, arr...)
			}
		}
		for _, h := range hooks[ev] {
			groups = appendHookGroup(groups, h)
		}
		tv, err := tomlValue(groups)
		if err != nil {
			return nil, err
		}
		out = append(out, "-c", path+"="+tv)
	}
	return out, nil
}

// adoptedValues reads the user's own settings as LayerAdoptedUser values in
// native precedence order (user < user local < project < project local).
func adoptedValues(h extensions.Homes, projectDirs []string) (map[string]any, []string) {
	out := map[string]any{}
	var warns []string
	readJSON := func(p string) {
		b, err := os.ReadFile(p)
		if err != nil {
			return
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		var v map[string]any
		if err := dec.Decode(&v); err != nil {
			warns = append(warns, fmt.Sprintf("adopt %s: %v", h.Display(p), err))
			return
		}
		flatten([]string{"claude"}, normalize(v), skipper(contracts.HarnessClaude), out)
	}
	readTOML := func(p string) {
		v, err := extensions.ReadTOML(p)
		if err != nil {
			if !os.IsNotExist(err) {
				warns = append(warns, fmt.Sprintf("adopt %s: %v", h.Display(p), err))
			}
			return
		}
		flatten([]string{"codex"}, normalize(v), skipper(contracts.HarnessCodex), out)
	}
	if h.ClaudeHome != "" {
		readJSON(filepath.Join(h.ClaudeHome, "settings.json"))
		readJSON(filepath.Join(h.ClaudeHome, "settings.local.json"))
	}
	for _, d := range projectDirs {
		readJSON(filepath.Join(d, ".claude", "settings.json"))
		readJSON(filepath.Join(d, ".claude", "settings.local.json"))
	}
	if h.CodexHome != "" {
		readTOML(filepath.Join(h.CodexHome, "config.toml"))
	}
	for _, d := range projectDirs {
		readTOML(filepath.Join(d, ".codex", "config.toml"))
	}
	return out, warns
}

func skipper(h contracts.Harness) func([]string) bool {
	skips := adoptSkip[h]
	return func(path []string) bool {
		for _, s := range skips {
			if path[0] == s {
				return true
			}
		}
		return false
	}
}

func sortedRKeys(m map[string]*rvalue) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func sortedStrKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func sortedDigestKeys(m map[string]contracts.Digest) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func sortRefs(r []contracts.ExtensionRef) {
	sort.SliceStable(r, func(i, j int) bool {
		if r[i].Name != r[j].Name {
			return r[i].Name < r[j].Name
		}
		if r[i].Harness != r[j].Harness {
			return r[i].Harness < r[j].Harness
		}
		return r[i].Source < r[j].Source
	})
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
