package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Hook trust in account profiles.
//
// Codex 0.162+ runs a hook (hooks.json, config.toml [hooks], plugin hooks)
// only after the owner trusted it: config.toml records
// [hooks.state."<source>:<event>:<group>:<handler>"] trusted_hash =
// "sha256:…" (the hash covers the hook's content, not its path), and the
// TUI opens "Hooks need review" for every hook that is new or changed
// (iss_…: a reviewer session on devbox). A
// Pyre account profile copies the owner's hooks.json and mirrors their
// plugins (accounts/compose.go), but not that trust: a hooks.json hook's
// key names the file's path, which differs in the profile, and plugin
// hashes in the profile's config snapshot go stale. So in a profile the
// owner's hooks silently did not run and an attached TUI stopped at the
// review screen.
//
// Before a run's thread starts in a profile home (never the owner's own
// CODEX_HOME), mirrorHookTrust lists the hooks the app-server sees
// (hooks/list) and, for each one not trusted there, copies the owner's own
// record (every field: trusted_hash, Codex 0.166's trusted_content_hash,
// enabled, and any sibling key a later Codex adds) into the
// profile's config (config/value/write, upsert into hooks.state) only when
// the owner trusted exactly that content: the owner's trusted_hash equals
// the profile hook's currentHash under the owner's key for it (the same
// key, with the profile's path rewritten to the owner's CODEX_HOME). A new
// or changed hook, one the owner never trusted, or any hook while the
// owner's config cannot be read stays untrusted, for the owner to review.

// hookState is one [hooks.state.<key>] record, kept whole: Codex adds
// sibling trust fields (0.166: trusted_content_hash, the hash of the hook's
// script) that must be mirrored with trusted_hash or the profile still
// shows the hook as needing review (iss_…).
type hookState map[string]any

// trustedHash is the record's trusted_hash ("" when absent).
func (s hookState) trustedHash() string {
	h, _ := s["trusted_hash"].(string)
	return h
}

// trusts reports whether the record trusts a hook whose hooks/list
// currentHash is current. Codex 0.166+ reports a hook with a script as
// "<trusted_hash>/<trusted_content_hash>" (e.g. "sha256:1507…/sha256:dde9…"):
// both halves must match the owner's record. Comparing the whole string with
// trusted_hash alone never matched, so after a plugin update changed a
// script (acme 0.21.4, iss_…) the profile kept the
// old content hash and every run stopped at "Hooks need review" although the
// owner had trusted the new script in ~/.codex.
func (s hookState) trusts(current string) bool {
	cfg, content, hasContent := strings.Cut(current, "/")
	if cfg == "" || s.trustedHash() != cfg {
		return false
	}
	if !hasContent {
		return true
	}
	h, _ := s["trusted_content_hash"].(string)
	return h == content
}

type hooksListParams struct {
	Cwds []string `json:"cwds,omitempty"`
}

type hookMetadata struct {
	Key         string `json:"key"`
	CurrentHash string `json:"currentHash"`
	TrustStatus string `json:"trustStatus"`
	EventName   string `json:"eventName,omitempty"`
	PluginID    string `json:"pluginId,omitempty"`
	SourcePath  string `json:"sourcePath,omitempty"`
}

type hooksListEntry struct {
	Cwd   string         `json:"cwd"`
	Hooks []hookMetadata `json:"hooks"`
}

type hooksListResponse struct {
	Data []hooksListEntry `json:"data"`
}

// ownerHookStates reads hooks.state from the owner's config.toml in home.
func ownerHookStates(home string) map[string]hookState {
	if home == "" {
		return nil
	}
	var cfg struct {
		Hooks struct {
			State map[string]hookState `toml:"state"`
		} `toml:"hooks"`
	}
	if _, err := toml.DecodeFile(filepath.Join(home, "config.toml"), &cfg); err != nil {
		return nil
	}
	return cfg.Hooks.State
}

// ownerHookKey is the owner's key for a profile hook key: a file source
// under the profile home names the same file under the owner's home; any
// other key (a plugin id, session flags) is the same.
func ownerHookKey(key, profile, owner string) string {
	p := filepath.Clean(profile) + string(filepath.Separator)
	if rest, ok := strings.CutPrefix(key, p); ok {
		return filepath.Clean(owner) + string(filepath.Separator) + rest
	}
	return key
}

// hookTrustToMirror returns the hooks.state records to write in the profile:
// hooks it does not trust whose content the owner trusted.
func hookTrustToMirror(hooks []hookMetadata, owner map[string]hookState, profileHome, ownerHome string) map[string]hookState {
	out := map[string]hookState{}
	for _, h := range hooks {
		if h.Key == "" || h.CurrentHash == "" || (h.TrustStatus != "untrusted" && h.TrustStatus != "modified") {
			continue
		}
		st, ok := owner[ownerHookKey(h.Key, profileHome, ownerHome)]
		if !ok || !st.trusts(h.CurrentHash) {
			continue
		}
		rec := hookState{}
		for k, v := range st {
			rec[k] = v
		}
		out[h.Key] = rec
	}
	return out
}

// mirrorHookTrust applies the owner's hook trust to a profile home's
// app-server before a thread starts, and returns the hooks that stay
// untrusted (new or changed; the TUI opens "Hooks need review" for them,
// hookreview.go). Best effort and bounded: an app-server without
// hooks/list (Codex before 0.162) or any error changes nothing and
// returns nil. The owner's own CODEX_HOME is listed, never written.
func (a *Adapter) mirrorHookTrust(ctx context.Context, c *rpcClient, profileHome, cwd string) []string {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var r hooksListResponse
	if err := c.call(cctx, "hooks/list", hooksListParams{Cwds: []string{cwd}}, &r); err != nil {
		return nil
	}
	var hooks []hookMetadata
	for _, e := range r.Data {
		hooks = append(hooks, e.Hooks...)
	}
	var mirror map[string]hookState
	if ownerHome := a.ownerHomeFor(profileHome); ownerHome != "" {
		mirror = hookTrustToMirror(hooks, ownerHookStates(ownerHome), profileHome, ownerHome)
		if len(mirror) > 0 {
			if err := c.call(cctx, "config/value/write", configValueWriteParams{KeyPath: "hooks.state", MergeStrategy: "upsert", Value: mirror}, nil); err != nil {
				mirror = nil
			}
		}
	}
	return untrustedHooks(hooks, mirror)
}

// ownerHomeFor is the owner's CODEX_HOME to mirror trust from into
// profileHome ("" = none: not a profile run, tests, or no owner config).
func (a *Adapter) ownerHomeFor(profileHome string) string {
	if profileHome == "" || skipProfileTrust {
		return ""
	}
	ownerHome := a.o.OwnerCodexHome
	if ownerHome == "" {
		ownerHome = codexHome("")
	}
	if ownerHome == "" || filepath.Clean(ownerHome) == filepath.Clean(profileHome) {
		return "" // never the owner's own home
	}
	if _, err := os.Stat(filepath.Join(ownerHome, "config.toml")); err != nil {
		return ""
	}
	return ownerHome
}

// untrustedHooks names the hooks that stay untrusted after mirror: one
// line each, "<plugin or file> <event>" (e.g. "acme@acme-plugins
// sessionStart (changed)").
func untrustedHooks(hooks []hookMetadata, mirror map[string]hookState) []string {
	var out []string
	for _, h := range hooks {
		if h.TrustStatus != "untrusted" && h.TrustStatus != "modified" {
			continue
		}
		if _, ok := mirror[h.Key]; ok {
			continue
		}
		src := h.PluginID
		if src == "" {
			src = h.SourcePath
		}
		if src == "" {
			src = h.Key
		}
		d := src
		if h.EventName != "" {
			d += " " + h.EventName
		}
		if h.TrustStatus == "modified" {
			d += " (changed)"
		} else {
			d += " (new)"
		}
		out = append(out, d)
	}
	return out
}
