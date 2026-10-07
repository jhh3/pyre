package configuration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/extensions"
)

// Drift compares live state against the applied generations. It never
// writes to config homes. Kinds:
//
//	modified            a Pyre-managed file/key/overlay differs from what was applied,
//	                    or an adopted user setting changed since the generation (preserved)
//	missing             a managed item was removed
//	unmanaged_addition  a user item added outside Pyre (preserved, reported)
//	conflict            ownership is ambiguous (marker removed, same-name user registration)
//	version_mismatch    a runtime version differs from the generation's pin
func (m *Manager) Drift(ctx context.Context) ([]contracts.DriftItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	items := []contracts.DriftItem{}
	scopes := m.currentScopes()
	keys := make([]string, 0, len(scopes))
	for k := range scopes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var globalRec *genRecord
	for _, k := range keys {
		rec, err := m.loadRecord(scopes[k])
		if err != nil {
			items = append(items, contracts.DriftItem{Path: "generation:" + scopes[k], Kind: "missing", Detail: "current generation record unreadable: " + err.Error()})
			continue
		}
		if rec.Scope == "global" {
			globalRec = rec
		}
		items = append(items, m.overlayDrift(rec)...)
		items = append(items, m.homeDrift(rec)...)
		items = append(items, m.versionDrift(ctx, rec)...)
	}
	if globalRec != nil {
		more, err := m.adoptionDrift(ctx, globalRec)
		if err != nil {
			return nil, err
		}
		items = append(items, more...)
	}
	if len(items) > 0 {
		parts := make([]string, 0, len(items)*3)
		for _, it := range items {
			parts = append(parts, it.Path, it.Key, it.Kind, it.Actual)
		}
		d := extensions.DigestStrings(parts...)
		if d != m.lastDrift {
			m.lastDrift = d
			m.report(ctx, contracts.ReportEvent, m.event(contracts.EvEnvironmentDrift, contracts.ObjectRef{Kind: contracts.KindNode, ID: m.opt.NodeID}, "", map[string]any{"items": items}))
		}
	} else {
		m.lastDrift = ""
	}
	return items, nil
}

func (m *Manager) overlayDrift(rec *genRecord) []contracts.DriftItem {
	var out []contracts.DriftItem
	for _, rel := range sortedDigestKeys(rec.Overlay.Files) {
		want := rec.Overlay.Files[rel]
		p := filepath.Join(m.genDir(rec.Generation.GenerationID), filepath.FromSlash(rel))
		got, ok, _ := liveDigest(p)
		switch {
		case !ok:
			out = append(out, contracts.DriftItem{Path: m.display(p), Kind: "missing", Expected: string(want), Detail: "Pyre overlay file of the current generation is missing"})
		case got != want:
			out = append(out, contracts.DriftItem{Path: m.display(p), Kind: "modified", Expected: string(want), Actual: string(got), Detail: "Pyre overlay file was modified"})
		}
	}
	return out
}

func (m *Manager) homeDrift(rec *genRecord) []contracts.DriftItem {
	var out []contracts.DriftItem
	for _, t := range rec.Home {
		switch t.Kind {
		case "dir":
			live, ok, _ := liveDigest(t.Path)
			if !ok {
				out = append(out, contracts.DriftItem{Path: t.Display, Key: "skill:" + t.Name, Kind: "missing", Expected: string(t.Digest)})
				continue
			}
			if _, owned := extensions.ReadMarker(t.Path); !owned {
				out = append(out, contracts.DriftItem{Path: t.Display, Key: "skill:" + t.Name, Kind: "conflict", Detail: "Pyre ownership marker removed; the directory is treated as user-owned"})
				continue
			}
			if live != t.Digest {
				out = append(out, contracts.DriftItem{Path: t.Display, Key: "skill:" + t.Name, Kind: "modified", Expected: string(t.Digest), Actual: string(live), Detail: "managed skill edited locally"})
			}
		case "json_keys":
			raw, err := os.ReadFile(t.Path)
			if err != nil {
				out = append(out, contracts.DriftItem{Path: t.Display, Kind: "missing", Detail: err.Error()})
				continue
			}
			doc, err := parseOrdered(raw)
			if err != nil {
				out = append(out, contracts.DriftItem{Path: t.Display, Kind: "conflict", Detail: "file is no longer valid JSON: " + err.Error()})
				continue
			}
			for _, k := range sortedMapKeys(t.Keys) {
				segs, _ := SplitKey(k)
				lv, has := doc.getPath(segs)
				full := "claude." + k
				switch {
				case !has:
					out = append(out, contracts.DriftItem{Path: t.Display, Key: k, Kind: "missing", Expected: redactValue(full, t.Keys[k])})
				case !equalValues(plainValue(lv), t.Keys[k]):
					out = append(out, contracts.DriftItem{Path: t.Display, Key: k, Kind: "modified", Expected: redactValue(full, t.Keys[k]), Actual: redactValue(full, plainValue(lv))})
				}
			}
		}
	}
	return out
}

func (m *Manager) versionDrift(ctx context.Context, rec *genRecord) []contracts.DriftItem {
	if m.opt.Versions == nil {
		return nil
	}
	var out []contracts.DriftItem
	now := m.opt.Versions.ActiveVersions(ctx)
	for _, tool := range sortedStrKeys(rec.Generation.RuntimeVersions) {
		want := rec.Generation.RuntimeVersions[tool]
		if got, ok := now[tool]; ok && got != want {
			out = append(out, contracts.DriftItem{Path: "tool:" + tool, Key: tool, Kind: "version_mismatch", Expected: want, Actual: got,
				Detail: "new runs use " + got + "; generation " + rec.Generation.GenerationID + " was resolved with " + want})
		}
	}
	return out
}

// adoptionDrift reports user changes since the global generation's baseline.
func (m *Manager) adoptionDrift(ctx context.Context, rec *genRecord) ([]contracts.DriftItem, error) {
	var out []contracts.DriftItem
	inv, err := extensions.Scan(ctx, m.opt.Homes, extensions.ScanOptions{Managed: m.opt.Managed})
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, id := range rec.Baseline.Identities {
		known[id] = true
	}
	managedMCP := map[string]contracts.ExtensionRef{}
	for _, e := range rec.Generation.MCPServers {
		if e.Managed {
			managedMCP[extensions.MCPIdentity(e.Harness, e.Name)] = e
		}
	}
	for _, it := range inv.Items {
		if it.Kind == extensions.KindMCPServer {
			if e, ok := managedMCP[it.Identity]; ok && it.MCP != nil && it.MCP.DefDigest != e.Digest {
				out = append(out, contracts.DriftItem{Path: it.Location, Key: "mcp:" + it.Name, Kind: "conflict",
					Detail: "user registered an MCP server with the same name as a managed one but a different definition; the user entry is preserved"})
				continue
			}
		}
		if known[it.Identity] || it.Managed {
			continue
		}
		path, key := it.Location, it.Name
		if it.Kind == extensions.KindHook && it.Hook != nil {
			path, _, _ = strings.Cut(it.Location, "#")
			key = "hooks." + it.Hook.Event
		}
		out = append(out, contracts.DriftItem{Path: path, Key: key, Kind: "unmanaged_addition",
			Detail: fmt.Sprintf("user-added %s outside Pyre-managed items is preserved", strings.ReplaceAll(string(it.Kind), "_", " "))})
	}
	adopted, _ := adoptedValues(m.opt.Homes, nil)
	owned := m.ownedKeys()
	for k := range owned {
		delete(adopted, k)
	}
	for _, k := range sortedMapKeys(adopted) {
		v := redactValue(k, normalize(adopted[k]))
		old, had := rec.Baseline.Settings[k]
		h, _, _ := harnessOf(k)
		file := m.settingsFileFor(h)
		switch {
		case !had:
			out = append(out, contracts.DriftItem{Path: file, Key: k, Kind: "unmanaged_addition", Actual: v, Detail: "user setting added outside Pyre is preserved"})
		case old != v:
			out = append(out, contracts.DriftItem{Path: file, Key: k, Kind: "modified", Expected: old, Actual: v, Detail: "adopted user setting changed since generation " + rec.Generation.GenerationID + " (user-owned; preserved)"})
		}
	}
	for _, k := range sortedStrKeys(rec.Baseline.Settings) {
		if _, ok := adopted[k]; !ok {
			h, _, _ := harnessOf(k)
			out = append(out, contracts.DriftItem{Path: m.settingsFileFor(h), Key: k, Kind: "missing", Expected: rec.Baseline.Settings[k], Detail: "adopted user setting removed since the generation (user-owned)"})
		}
	}
	return out, nil
}

// ownedKeys returns the setting keys Pyre owns inside user files according
// to the current global generation's ownership record.
func (m *Manager) ownedKeys() map[string]bool {
	out := map[string]bool{}
	if rec := m.currentRecordFor("global"); rec != nil {
		for _, t := range rec.Home {
			if t.Kind == "json_keys" {
				for k := range t.Keys {
					out["claude."+k] = true
				}
			}
		}
	}
	return out
}

func (m *Manager) settingsFileFor(h contracts.Harness) string {
	if h == contracts.HarnessCodex {
		return m.display(filepath.Join(m.opt.Homes.CodexHome, "config.toml"))
	}
	return m.display(filepath.Join(m.opt.Homes.ClaudeHome, "settings.json"))
}
