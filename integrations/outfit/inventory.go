package outfit

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Inventory is Outfit-managed tooling on this node, for the environment
// inventory (WP14). It contains paths, versions, and digests only.
type Inventory struct {
	Source      string                   `json:"source"` // "inventory" | "text"
	Version     string                   `json:"version,omitempty"`
	Connections []Connection             `json:"connections"`
	Tools       []contracts.ToolVersion  `json:"tools"`
	Skills      []contracts.ExtensionRef `json:"skills"`
}

// Inventory collects managed CLIs and skills. With the upstream inventory
// command it is one call; older Outfit falls back to parsing the human output
// of `tool status` and `skill list --host both` (best effort).
func (a *Adapter) Inventory(ctx context.Context) (Inventory, error) {
	doc, ok, err := a.inventory(ctx)
	if err != nil {
		return Inventory{}, err
	}
	if ok {
		inv := Inventory{Source: "inventory", Version: doc.Version, Connections: validConnections(doc.Connections), Tools: []contracts.ToolVersion{}, Skills: []contracts.ExtensionRef{}}
		for _, t := range doc.Tools {
			inv.Tools = append(inv.Tools, toolVersion(t.ID, t.Version, t.Path, t.SHA256, t.Current, t.InstalledAt))
		}
		for _, s := range doc.Skills {
			inv.Skills = append(inv.Skills, skillRef(s.ID, s.Host, s.Connection, s.Source, s.Ref, s.Digest))
		}
		return inv, nil
	}
	inv := Inventory{Source: "text", Tools: []contracts.ToolVersion{}, Skills: []contracts.ExtensionRef{}}
	if inv.Connections, err = a.Connections(ctx); err != nil {
		return Inventory{}, err
	}
	res, err := a.run(ctx, a.opt.Timeout, nil, nil, "tool", "status")
	if err != nil {
		return Inventory{}, err
	}
	if res.exit != 0 {
		return Inventory{}, unavailable("outfit tool status failed (exit %d): %s", res.exit, diag(res.stderr))
	}
	inv.Tools = parseToolStatus(string(res.stdout))
	res, err = a.run(ctx, a.opt.Timeout, nil, nil, "skill", "list", "--host", "both")
	if err != nil {
		return Inventory{}, err
	}
	if res.exit != 0 {
		return Inventory{}, unavailable("outfit skill list failed (exit %d): %s", res.exit, diag(res.stderr))
	}
	inv.Skills = parseSkillList(string(res.stdout))
	return inv, nil
}

func toolVersion(id, version, path, sha string, current bool, installed time.Time) contracts.ToolVersion {
	tv := contracts.ToolVersion{Tool: id, Version: version, Path: path, State: "previous", InstalledAt: installed.UTC(), Health: "unchecked"}
	if current {
		tv.State = "active"
	}
	if sha != "" {
		if strings.HasPrefix(sha, "sha256:") {
			tv.Digest = contracts.Digest(sha)
		} else {
			tv.Digest = contracts.Digest("sha256:" + sha)
		}
	}
	return tv
}

func skillRef(id, host, connection, source, ref, digest string) contracts.ExtensionRef {
	e := contracts.ExtensionRef{Name: id, Version: ref, Layer: contracts.LayerAdoptedUser, Managed: false, Source: "outfit:" + connection}
	switch host {
	case "claude":
		e.Harness = contracts.HarnessClaude
	case "codex":
		e.Harness = contracts.HarnessCodex
	}
	if source != "" {
		e.Source += " " + source
	}
	if digest != "" {
		if strings.HasPrefix(digest, "sha256:") {
			e.Digest = contracts.Digest(digest)
		} else {
			e.Digest = contracts.Digest("sha256:" + digest)
		}
	}
	return e
}

// parseToolStatus parses "ID VERSION PLATFORM KIND [current] PATH" lines.
func parseToolStatus(out string) []contracts.ToolVersion {
	tools := []contracts.ToolVersion{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		current, pathAt := false, 4
		if f[4] == "current" {
			current, pathAt = true, 5
		}
		if pathAt >= len(f) {
			continue
		}
		tools = append(tools, toolVersion(f[0], f[1], strings.Join(f[pathAt:], " "), "", current, time.Time{}))
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Tool+tools[i].Version < tools[j].Tool+tools[j].Version })
	return tools
}

// parseSkillList parses "HOST CONNECTION ID KIND [REF] PATH" lines.
func parseSkillList(out string) []contracts.ExtensionRef {
	skills := []contracts.ExtensionRef{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		ref := ""
		if !strings.HasPrefix(f[4], "/") && len(f) >= 6 {
			ref = f[4]
		}
		skills = append(skills, skillRef(f[2], f[0], f[1], f[3], ref, ""))
	}
	return skills
}
