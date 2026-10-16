package cloud

import (
	"regexp"
	"sort"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// Developer toolchains a cloud node's boot script installs (gcp.toolchains in
// cloud.json, --toolchain on create/bake). An entry is a toolchain id, an id
// with a version (node@24.21.0) or a preset name. The CLI resolves the list
// (presets expanded, versions validated, one entry per tool, dependencies
// added, fixed install order) and hands it to the VM in pyre-config; the
// scripts install each one idempotently (skipped when the requested version
// is present).

// toolchainVersion says which versions a toolchain id accepts after "@".
type toolchainVersion int

const (
	tvNone    toolchainVersion = iota // no version
	tvExact                           // optional exact version (x.y.z)
	tvNode                            // lts, a major (24) or exact x.y.z
	tvChannel                         // rust: stable|beta|nightly[-date]|x.y[.z]
)

// toolchainOrder is the known ids in install order: compilers and runtimes
// before what builds on them (pnpm needs node; rust's MSVC host needs the
// Build Tools).
var toolchainOrder = []string{"git", "python", "vs-buildtools", "node", "pnpm", "bun", "rust", "cmake", "llvm", "gh", "parsec"}

// windowsOnly toolchains: dropped from presets on Linux, refused when listed
// explicitly for a Linux node.
var windowsOnly = map[string]bool{"parsec": true}

var toolchainVersions = map[string]toolchainVersion{
	"git": tvNone, "python": tvExact, "vs-buildtools": tvNone, "node": tvNode, "pnpm": tvExact,
	"bun": tvExact, "rust": tvChannel, "cmake": tvExact, "llvm": tvExact, "gh": tvNone,
	"parsec": tvNone, // Parsec host (Windows): the owner signs in once over RDP
}

var (
	exactVersionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	majorRe        = regexp.MustCompile(`^[0-9]+$`)
	rustChannelRe  = regexp.MustCompile(`^(stable|beta|nightly(-[0-9]{4}-[0-9]{2}-[0-9]{2})?|[0-9]+\.[0-9]+(\.[0-9]+)?)$`)
)

// Toolchain presets. "default" is what a node gets when nothing is
// configured (the toolchain the boot scripts always installed).
const (
	PresetDefault  = "default"
	PresetElectron = "electron"
	PresetMedia    = "media" // electron + parsec (GPU media-test nodes)
)

// ToolchainPresets lists the preset names.
func ToolchainPresets() []string { return []string{PresetDefault, PresetElectron, PresetMedia} }

func presetToolchains(name, osName string) ([]string, bool) {
	win := osName == OSWindows
	switch name {
	case PresetDefault:
		if win {
			return []string{"git", "node@lts", "python", "vs-buildtools"}, true
		}
		return []string{"git", "python"}, true // the base packages
	case PresetElectron, PresetMedia:
		out := []string{"node@lts", "pnpm", "python", "git", "gh"}
		if win {
			out = append(out, "vs-buildtools")
			if name == PresetMedia {
				out = append(out, "parsec")
			}
		}
		return out, true
	}
	return nil, false
}

// parseToolchain validates one id[@version] and returns its canonical form.
func parseToolchain(entry string) (name, canonical string, err error) {
	e := strings.ToLower(strings.TrimSpace(entry))
	name, ver, hasVer := strings.Cut(e, "@")
	kind, ok := toolchainVersions[name]
	if !ok {
		return "", "", contracts.Errorf(contracts.ErrInvalid, "unknown toolchain %q: use one of %s, or a preset (%s)",
			entry, strings.Join(toolchainOrder, ", "), strings.Join(ToolchainPresets(), ", "))
	}
	if hasVer && ver == "" {
		return "", "", contracts.Errorf(contracts.ErrInvalid, "toolchain %q: empty version", entry)
	}
	if name == "node" || name == "bun" || name == "pnpm" || name == "python" || name == "cmake" || name == "llvm" {
		ver = strings.TrimPrefix(ver, "v")
	}
	switch kind {
	case tvNone:
		if hasVer {
			return "", "", contracts.Errorf(contracts.ErrInvalid, "toolchain %q takes no version", entry)
		}
	case tvExact:
		if hasVer && !exactVersionRe.MatchString(ver) {
			return "", "", contracts.Errorf(contracts.ErrInvalid, "toolchain %q: the version must be exact (x.y.z)", entry)
		}
	case tvNode:
		if !hasVer {
			ver = "lts"
		}
		if ver != "lts" && !majorRe.MatchString(ver) && !exactVersionRe.MatchString(ver) {
			return "", "", contracts.Errorf(contracts.ErrInvalid, "toolchain %q: node takes lts, a major version (24) or an exact version (24.21.0)", entry)
		}
		return name, name + "@" + ver, nil
	case tvChannel:
		if hasVer && !rustChannelRe.MatchString(ver) {
			return "", "", contracts.Errorf(contracts.ErrInvalid, "toolchain %q: rust takes stable, beta, nightly[-YYYY-MM-DD] or a version (1.85.0)", entry)
		}
	}
	if ver == "" {
		return name, name, nil
	}
	return name, name + "@" + ver, nil
}

// ResolveToolchains is the list a node of osName installs: configured (or
// the default preset when nothing is configured) followed by extra (the
// --toolchain flags). Presets expand in place; a later entry for the same
// tool replaces an earlier one (so --toolchain node@24.21.0 overrides the
// configured node@lts). pnpm adds node@lts when no node is listed. The
// result is in install order.
func ResolveToolchains(osName string, configured, extra []string) ([]string, error) {
	base := configured
	if len(base) == 0 {
		base = []string{PresetDefault}
	}
	chosen := map[string]string{}
	add := func(entry string) error {
		name, canon, err := parseToolchain(entry)
		if err != nil {
			return err
		}
		if windowsOnly[name] && osName != OSWindows {
			return contracts.Errorf(contracts.ErrInvalid, "toolchain %s is Windows only", name)
		}
		chosen[name] = canon
		return nil
	}
	for _, list := range [][]string{base, extra} {
		for _, raw := range list {
			e := strings.TrimSpace(raw)
			if e == "" {
				continue
			}
			if p, ok := presetToolchains(strings.ToLower(e), osName); ok {
				for _, t := range p {
					if err := add(t); err != nil {
						return nil, err
					}
				}
				continue
			}
			if err := add(e); err != nil {
				return nil, err
			}
		}
	}
	if _, ok := chosen["pnpm"]; ok {
		if _, ok := chosen["node"]; !ok {
			chosen["node"] = "node@lts"
		}
	}
	if _, ok := chosen["rust"]; ok && osName == OSWindows {
		chosen["vs-buildtools"] = "vs-buildtools" // the MSVC linker and Windows SDK
	}
	rank := map[string]int{}
	for i, n := range toolchainOrder {
		rank[n] = i
	}
	names := make([]string, 0, len(chosen))
	for n := range chosen {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return rank[names[i]] < rank[names[j]] })
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = chosen[n]
	}
	return out, nil
}

// withHarnessToolchains adds what the harnesses need that the toolchain list
// lacks: gh is a toolchain install. (Node for Claude Code/Codex is the boot
// scripts' own fallback: node 22 on Linux, LTS on Windows, only when no node
// is installed.)
func withHarnessToolchains(tcs, harnesses []string) []string {
	for _, h := range harnesses {
		if h != "gh" {
			continue
		}
		for _, t := range tcs {
			if t == "gh" {
				return tcs
			}
		}
		return append(append([]string(nil), tcs...), "gh")
	}
	return tcs
}
