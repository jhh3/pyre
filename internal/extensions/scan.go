package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/jhh3/pyre/internal/contracts"
)

// ScanOptions tunes Scan.
type ScanOptions struct {
	// Managed is the Outfit-managed skill source (optional, WP12).
	Managed ManagedSource
	// MaxEntries bounds directory listings per source (default 500).
	MaxEntries int
}

// Scan inventories the given homes. It never writes. Unreadable or malformed
// sources become warnings rather than errors so one broken file does not hide
// the rest of the inventory.
func Scan(ctx context.Context, h Homes, opt ScanOptions) (Inventory, error) {
	if opt.MaxEntries <= 0 {
		opt.MaxEntries = 500
	}
	s := &scanner{h: h, opt: opt}
	if h.ClaudeHome != "" {
		s.root(contracts.HarnessClaude, h.ClaudeHome)
		s.claudeHome(h.ClaudeHome, "user")
	}
	if h.ClaudeJSON != "" {
		s.claudeJSON(h.ClaudeJSON)
	}
	if h.CodexHome != "" {
		s.root(contracts.HarnessCodex, h.CodexHome)
		s.codexHome(h.CodexHome)
	}
	for _, p := range h.ProjectDirs {
		s.project(p)
	}
	if opt.Managed != nil {
		ms, err := opt.Managed.ManagedSkills(ctx)
		if err != nil {
			s.warn("outfit managed skills unavailable: %v", err)
		} else {
			s.applyManaged(ms)
		}
	}
	s.findDuplicates()
	sort.SliceStable(s.inv.Items, func(i, j int) bool {
		a, b := s.inv.Items[i], s.inv.Items[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Harness != b.Harness {
			return a.Harness < b.Harness
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Location < b.Location
	})
	return s.inv, nil
}

type scanner struct {
	h   Homes
	opt ScanOptions
	inv Inventory
}

func (s *scanner) warn(f string, a ...any) {
	s.inv.Warnings = append(s.inv.Warnings, fmt.Sprintf(f, a...))
}

func (s *scanner) root(h contracts.Harness, p string) {
	_, err := os.Stat(p)
	s.inv.Roots = append(s.inv.Roots, contracts.ConfigRoot{Harness: h, Path: s.h.Display(p), Exists: err == nil})
}

func (s *scanner) add(it Item) {
	if it.Layer == "" {
		it.Layer = contracts.LayerAdoptedUser
	}
	if it.Location == "" && it.Path != "" {
		it.Location = s.h.Display(it.Path)
	}
	if it.Identity == "" {
		it.Identity = string(it.Kind) + ":" + string(it.Harness) + ":" + it.Name
	}
	s.inv.Items = append(s.inv.Items, it)
}

func (s *scanner) readDir(dir string) []os.DirEntry {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			s.warn("read %s: %v", s.h.Display(dir), err)
		}
		return nil
	}
	if len(ents) > s.opt.MaxEntries {
		s.warn("%s: %d entries, inventory truncated to %d", s.h.Display(dir), len(ents), s.opt.MaxEntries)
		ents = ents[:s.opt.MaxEntries]
	}
	return ents
}

// skills scans <dir>/<name>/SKILL.md (one extra nesting level is followed
// for grouping directories such as skills/synced/<name>).
func (s *scanner) skills(h contracts.Harness, dir, scope string, layer contracts.ConfigLayer, plugin string) {
	for _, e := range s.readDir(dir) {
		if !isDirEntry(dir, e) {
			continue
		}
		name := e.Name()
		p := filepath.Join(dir, name)
		if strings.HasPrefix(name, ".") {
			if h == contracts.HarnessCodex && name == ".system" && plugin == "" {
				s.skills(h, p, "system", contracts.LayerProviderDefault, "")
			}
			continue
		}
		if _, err := os.Stat(filepath.Join(p, "SKILL.md")); err != nil {
			// grouping directory: look one level down
			for _, e2 := range s.readDir(p) {
				p2 := filepath.Join(p, e2.Name())
				if isDirEntry(p, e2) && !strings.HasPrefix(e2.Name(), ".") {
					if _, err := os.Stat(filepath.Join(p2, "SKILL.md")); err == nil {
						s.skill(h, e2.Name(), p2, scope, layer, plugin, "nested under "+name)
					}
				}
			}
			continue
		}
		s.skill(h, name, p, scope, layer, plugin, "")
	}
}

func (s *scanner) skill(h contracts.Harness, name, p, scope string, layer contracts.ConfigLayer, plugin, detail string) {
	d, err := DigestDir(p)
	if err != nil {
		s.warn("digest %s: %v", s.h.Display(p), err)
	}
	it := Item{Kind: KindSkill, Name: name, Harness: h, Digest: d, Layer: layer, Scope: scope, Path: p, Plugin: plugin, Detail: detail}
	if plugin != "" {
		it.Identity = "skill:" + string(h) + ":" + plugin + ":" + name
	}
	if m, ok := ReadMarker(p); ok {
		it.Managed = true
		it.Layer = m.Layer
		it.Version = m.Version
		if m.Digest != d {
			it.Detail = "managed skill modified locally (digest differs from marker)"
		}
	}
	s.add(it)
}

func isDirEntry(parent string, e os.DirEntry) bool {
	if e.IsDir() {
		return true
	}
	if e.Type()&os.ModeSymlink != 0 {
		st, err := os.Stat(filepath.Join(parent, e.Name()))
		return err == nil && st.IsDir()
	}
	return false
}

// markdownEntries scans <dir>/*.md and <dir>/<ns>/*.md.
func (s *scanner) markdownEntries(h contracts.Harness, kind Kind, dir, scope string) {
	for _, e := range s.readDir(dir) {
		p := filepath.Join(dir, e.Name())
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if isDirEntry(dir, e) {
			for _, e2 := range s.readDir(p) {
				if strings.HasSuffix(e2.Name(), ".md") {
					p2 := filepath.Join(p, e2.Name())
					d, _ := DigestFile(p2)
					s.add(Item{Kind: kind, Harness: h, Name: e.Name() + ":" + strings.TrimSuffix(e2.Name(), ".md"), Digest: d, Scope: scope, Path: p2})
				}
			}
			continue
		}
		if strings.HasSuffix(e.Name(), ".md") {
			d, _ := DigestFile(p)
			s.add(Item{Kind: kind, Harness: h, Name: strings.TrimSuffix(e.Name(), ".md"), Digest: d, Scope: scope, Path: p})
		}
	}
}

func (s *scanner) readJSON(p string, v any) bool {
	b, err := os.ReadFile(p)
	if err != nil {
		if !os.IsNotExist(err) {
			s.warn("read %s: %v", s.h.Display(p), err)
		}
		return false
	}
	if err := json.Unmarshal(b, v); err != nil {
		s.warn("parse %s: %v", s.h.Display(p), err)
		return false
	}
	return true
}

// hookFile is the shared Claude settings / Codex hooks.json hooks shape.
type hookFile struct {
	Hooks map[string][]hookGroup `json:"hooks"`
}

type hookGroup struct {
	Matcher string      `json:"matcher"`
	Hooks   []hookEntry `json:"hooks"`
}

type hookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// HookGroupsFromAny converts a decoded hooks map (JSON or TOML) to groups.
func hookGroupsFromAny(v any) map[string][]hookGroup {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	out := map[string][]hookGroup{}
	for ev, raw := range m {
		var gs []hookGroup
		if json.Unmarshal(raw, &gs) == nil {
			out[ev] = gs
		}
	}
	return out
}

func (s *scanner) hooks(h contracts.Harness, groups map[string][]hookGroup, p, scope string) {
	evs := make([]string, 0, len(groups))
	for ev := range groups {
		evs = append(evs, ev)
	}
	sort.Strings(evs)
	for _, ev := range evs {
		for gi, g := range groups[ev] {
			for hi, e := range g.Hooks {
				if e.Type == "" {
					e.Type = "command"
				}
				def := &HookDef{Event: ev, Matcher: g.Matcher, Type: e.Type, Command: e.Command, CommandDigest: DigestStrings(strings.TrimSpace(e.Command)), Timeout: e.Timeout}
				name := ev
				if g.Matcher != "" {
					name += "[" + g.Matcher + "]"
				}
				s.add(Item{
					Kind: KindHook, Harness: h, Name: name, Scope: scope, Path: p,
					Location: fmt.Sprintf("%s#hooks.%s[%d].hooks[%d]", s.h.Display(p), ev, gi, hi),
					Identity: HookIdentity(h, ev, g.Matcher, e.Command), Hook: def,
					Digest: def.CommandDigest,
				})
			}
		}
	}
}

type claudeSettings struct {
	Hooks          map[string][]hookGroup `json:"hooks"`
	EnabledPlugins map[string]bool        `json:"enabledPlugins"`
}

func (s *scanner) claudeHome(home, scope string) {
	s.skills(contracts.HarnessClaude, filepath.Join(home, "skills"), scope, contracts.LayerAdoptedUser, "")
	s.markdownEntries(contracts.HarnessClaude, KindCommand, filepath.Join(home, "commands"), scope)
	s.markdownEntries(contracts.HarnessClaude, KindAgent, filepath.Join(home, "agents"), scope)
	enabled := map[string]bool{}
	for _, f := range []string{"settings.json", "settings.local.json"} {
		p := filepath.Join(home, f)
		var cs claudeSettings
		if s.readJSON(p, &cs) {
			s.hooks(contracts.HarnessClaude, cs.Hooks, p, scope)
			for k, v := range cs.EnabledPlugins {
				enabled[k] = v
			}
		}
	}
	if scope != "user" {
		return
	}
	var ip struct {
		Plugins map[string][]struct {
			Scope       string `json:"scope"`
			InstallPath string `json:"installPath"`
			Version     string `json:"version"`
		} `json:"plugins"`
	}
	ipPath := filepath.Join(home, "plugins", "installed_plugins.json")
	seen := map[string]bool{}
	if s.readJSON(ipPath, &ip) {
		names := make([]string, 0, len(ip.Plugins))
		for n := range ip.Plugins {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			for _, inst := range ip.Plugins[n] {
				en, ok := enabled[n]
				var enp *bool
				if ok {
					enp = &en
				} else {
					f := false
					enp = &f
				}
				it := Item{Kind: KindPlugin, Harness: contracts.HarnessClaude, Name: n, Version: inst.Version, Scope: orDefault(inst.Scope, "user"), Path: inst.InstallPath, Enabled: enp}
				if inst.InstallPath == "" {
					it.Location = s.h.Display(ipPath) + "#" + n
				}
				s.add(it)
				seen[n] = true
				if inst.InstallPath != "" {
					s.skills(contracts.HarnessClaude, filepath.Join(inst.InstallPath, "skills"), "plugin", contracts.LayerAdoptedUser, n)
				}
			}
		}
	}
	for n, en := range enabled {
		if !seen[n] {
			e := en
			s.add(Item{Kind: KindPlugin, Harness: contracts.HarnessClaude, Name: n, Scope: "user", Enabled: &e,
				Location: s.h.Display(filepath.Join(home, "settings.json")) + "#enabledPlugins", Detail: "enabled but not installed"})
		}
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// rawMCP is the JSON/TOML shape of an MCP server entry across harnesses.
type rawMCP struct {
	Type        string            `json:"type" toml:"type"`
	Command     string            `json:"command" toml:"command"`
	Args        []string          `json:"args" toml:"args"`
	URL         string            `json:"url" toml:"url"`
	Env         map[string]string `json:"env" toml:"env"`
	Headers     map[string]string `json:"headers" toml:"headers"`
	HTTPHeaders map[string]string `json:"http_headers" toml:"http_headers"`
	Enabled     *bool             `json:"enabled" toml:"enabled"`
}

func (r rawMCP) def(name string) *MCPDef {
	d := &MCPDef{Name: name, Transport: r.Type, Command: r.Command, Args: r.Args, URL: r.URL, Env: r.Env, Headers: r.Headers}
	if len(d.Headers) == 0 {
		d.Headers = r.HTTPHeaders
	}
	d.Normalize()
	return d
}

// MCPDefFromMap builds a normalized MCPDef from a decoded entry.
func MCPDefFromMap(name string, v any) *MCPDef {
	b, _ := json.Marshal(v)
	var r rawMCP
	_ = json.Unmarshal(b, &r)
	return r.def(name)
}

func (s *scanner) mcpServers(h contracts.Harness, servers map[string]rawMCP, p, keyPath, scope string) {
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		r := servers[n]
		s.add(Item{Kind: KindMCPServer, Harness: h, Name: n, Scope: scope, Path: p, Enabled: r.Enabled,
			Location: s.h.Display(p) + "#" + keyPath + "." + n, Identity: MCPIdentity(h, n), MCP: r.def(n), Digest: r.def(n).DefDigest})
	}
}

func (s *scanner) claudeJSON(p string) {
	var cj struct {
		MCPServers map[string]rawMCP `json:"mcpServers"`
		Projects   map[string]struct {
			MCPServers map[string]rawMCP `json:"mcpServers"`
		} `json:"projects"`
	}
	if !s.readJSON(p, &cj) {
		return
	}
	s.mcpServers(contracts.HarnessClaude, cj.MCPServers, p, "mcpServers", "user")
	for _, proj := range s.h.ProjectDirs {
		if e, ok := cj.Projects[proj]; ok && len(e.MCPServers) > 0 {
			s.mcpServers(contracts.HarnessClaude, e.MCPServers, p, "projects["+s.h.Display(proj)+"].mcpServers", "local")
		}
	}
}

func (s *scanner) codexHome(home string) {
	s.skills(contracts.HarnessCodex, filepath.Join(home, "skills"), "user", contracts.LayerAdoptedUser, "")
	s.markdownEntries(contracts.HarnessCodex, KindPrompt, filepath.Join(home, "prompts"), "user")
	hp := filepath.Join(home, "hooks.json")
	var hf hookFile
	if s.readJSON(hp, &hf) {
		s.hooks(contracts.HarnessCodex, hf.Hooks, hp, "user")
	}
	cp := filepath.Join(home, "config.toml")
	cfg, err := ReadTOML(cp)
	if err != nil {
		if !os.IsNotExist(err) {
			s.warn("parse %s: %v", s.h.Display(cp), err)
		}
	}
	if cfg != nil {
		if ms, ok := cfg["mcp_servers"].(map[string]any); ok {
			servers := map[string]rawMCP{}
			for n, v := range ms {
				b, _ := json.Marshal(v)
				var r rawMCP
				_ = json.Unmarshal(b, &r)
				servers[n] = r
			}
			s.mcpServers(contracts.HarnessCodex, servers, cp, "mcp_servers", "user")
		}
		if hk, ok := cfg["hooks"].(map[string]any); ok {
			events := map[string]any{}
			for k, v := range hk {
				if k != "state" {
					events[k] = v
				}
			}
			s.hooks(contracts.HarnessCodex, hookGroupsFromAny(events), cp, "user")
		}
		if pl, ok := cfg["plugins"].(map[string]any); ok {
			names := make([]string, 0, len(pl))
			for n := range pl {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				var enp *bool
				if t, ok := pl[n].(map[string]any); ok {
					if b, ok := t["enabled"].(bool); ok {
						enp = &b
					}
				}
				it := Item{Kind: KindPlugin, Harness: contracts.HarnessCodex, Name: n, Scope: "user", Enabled: enp,
					Location: s.h.Display(cp) + "#plugins." + n}
				if name, market, ok := strings.Cut(n, "@"); ok {
					if v, dir := latestVersionDir(filepath.Join(home, "plugins", "cache", market, name)); v != "" {
						it.Version, it.Path = v, dir
						it.Location = s.h.Display(dir)
						s.skills(contracts.HarnessCodex, filepath.Join(dir, "skills"), "plugin", contracts.LayerAdoptedUser, n)
					}
				}
				s.add(it)
			}
		}
	}
}

// latestVersionDir returns the lexically greatest non-hidden child of dir.
func latestVersionDir(dir string) (string, string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", ""
	}
	best := ""
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && e.Name() > best {
			best = e.Name()
		}
	}
	if best == "" {
		return "", ""
	}
	return best, filepath.Join(dir, best)
}

func (s *scanner) project(dir string) {
	cd := filepath.Join(dir, ".claude")
	s.claudeHome(cd, "project")
	var mj struct {
		MCPServers map[string]rawMCP `json:"mcpServers"`
	}
	mp := filepath.Join(dir, ".mcp.json")
	if s.readJSON(mp, &mj) {
		s.mcpServers(contracts.HarnessClaude, mj.MCPServers, mp, "mcpServers", "project")
	}
	xd := filepath.Join(dir, ".codex")
	s.skills(contracts.HarnessCodex, filepath.Join(xd, "skills"), "project", contracts.LayerAdoptedUser, "")
	hp := filepath.Join(xd, "hooks.json")
	var hf hookFile
	if s.readJSON(hp, &hf) {
		s.hooks(contracts.HarnessCodex, hf.Hooks, hp, "project")
	}
}

func (s *scanner) applyManaged(ms []ManagedSkill) {
	for _, m := range ms {
		found := false
		for i := range s.inv.Items {
			it := &s.inv.Items[i]
			if it.Kind == KindSkill && it.Name == m.Name && it.Plugin == "" && (m.Harness == "" || m.Harness == it.Harness) {
				if m.Digest == "" || m.Digest == it.Digest {
					it.Managed = true
					it.Layer = orLayer(m.Layer, contracts.LayerManagedGlobal)
					it.Version = m.Version
					it.Detail = "managed by outfit"
					found = true
				} else {
					it.Detail = "same name as an outfit-managed skill but different content (user-owned, preserved)"
				}
			}
		}
		if !found {
			s.add(Item{Kind: KindSkill, Name: m.Name, Harness: m.Harness, Version: m.Version, Digest: m.Digest,
				Layer: orLayer(m.Layer, contracts.LayerManagedGlobal), Managed: true, Scope: "outfit", Path: m.Dir,
				Detail: "managed by outfit; not installed in a harness home"})
		}
	}
}

func orLayer(l, d contracts.ConfigLayer) contracts.ConfigLayer {
	if l == "" {
		return d
	}
	return l
}

func (s *scanner) findDuplicates() {
	count := map[string][]Item{}
	for _, it := range s.inv.Items {
		if it.Kind == KindHook || it.Kind == KindMCPServer {
			count[it.Identity] = append(count[it.Identity], it)
		}
	}
	ids := make([]string, 0, len(count))
	for id := range count {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		its := count[id]
		if len(its) < 2 {
			continue
		}
		if its[0].Kind == KindMCPServer {
			// Same MCP name in several scopes: the harness picks one by its own
			// precedence. Report only when definitions differ.
			same := true
			for _, o := range its[1:] {
				if o.MCP.DefDigest != its[0].MCP.DefDigest {
					same = false
				}
			}
			if same {
				continue
			}
		}
		locs := make([]string, len(its))
		for i, it := range its {
			locs[i] = it.Location
		}
		s.inv.Duplicates = append(s.inv.Duplicates, contracts.DriftItem{
			Path: its[0].Location, Key: its[0].Name, Kind: "conflict",
			Detail: fmt.Sprintf("%s registered %d times: %s", its[0].Kind, len(its), strings.Join(locs, ", ")),
		})
	}
}

// ReadTOML decodes a TOML file into a generic map.
func ReadTOML(p string) (map[string]any, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if _, err := toml.Decode(string(b), &m); err != nil {
		return nil, err
	}
	return m, nil
}
