package accounts

// Profile composition from the owner's config, under one rule: a Pyre run
// must never write the owner's files. Read-only sharing is fine; write-through
// is not.
//
// A symlink to an owner file or directory is write-through: anything the
// harness writes there (Claude 2.1.285 migrating settings into settings.json,
// per-org plugin sync buckets in plugins/synced, Codex installing bundled
// skills into skills/.system, plugin caches, plugin data) lands in the owner's
// home. So composition uses only:
//
//   - per-launch copies for files the harness may rewrite (settings.json,
//     CLAUDE.md, hooks.json, AGENTS.md, the plugin registries): refreshed
//     from the owner's file at every launch; changes made during a run stay in
//     the profile and are discarded at the next launch;
//   - mirrors for directories: a real profile directory whose entries are
//     links to the owner's entries (read-only sharing of skills, agents,
//     commands, plugin code). New entries the harness creates land in the
//     profile; harness-written state directories (plugins/synced, data,
//     store, Codex skills/.system, ...) are never linked and stay local.
//
// Composition is idempotent and re-run before every launch (see
// refreshComposition), so owner changes appear in the next run and links to
// vanished owner entries are pruned. An older profile's top-level symlinks
// (settings.json -> owner, plugins -> owner) are replaced; only the link is
// removed, never its target.

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// mirrorSpec describes how one profile directory mirrors an owner directory.
type mirrorSpec struct {
	copyFiles bool                   // regular files: per-launch copy (true) or link (false)
	linkDirs  bool                   // child dirs without a sub-spec: link (true) or keep local (false)
	dirs      map[string]*mirrorSpec // child dirs mirrored with their own spec
	each      *mirrorSpec            // spec for every other child dir (overrides linkDirs)
	skip      map[string]bool        // child names never linked or copied (harness-written state)
}

// flatMirror links every owner entry (skills, agents, commands, prompts).
func flatMirror(skip ...string) *mirrorSpec {
	m := &mirrorSpec{linkDirs: true, skip: map[string]bool{}}
	for _, s := range skip {
		m.skip[s] = true
	}
	return m
}

// depthMirror keeps n levels of real directories and links below that
// (plugin caches: <marketplace>/<plugin>/<version>), so a newly installed
// plugin or version lands in the profile, not in the owner's cache.
func depthMirror(n int) *mirrorSpec {
	if n <= 1 {
		return flatMirror()
	}
	return &mirrorSpec{each: depthMirror(n - 1)}
}

type composition struct {
	copies  []string               // top-level per-launch copies
	mirrors map[string]*mirrorSpec // top-level mirrored directories
}

var compositions = map[contracts.Harness]composition{
	contracts.HarnessClaude: {
		copies: []string{"settings.json", "CLAUDE.md"},
		mirrors: map[string]*mirrorSpec{
			// Claude keeps per-org sync buckets in skills/synced (as in
			// plugins/synced): harness-written state, never linked (live on
			// laptop: a bucket per test account's org appeared in the
			// owner's skills/synced through the old whole-dir link).
			"skills": flatMirror("synced"), "agents": flatMirror("synced"), "commands": flatMirror("synced"),
			"output-styles": flatMirror("synced"),
			// Registries (installed_plugins.json, known_marketplaces.json, caches)
			// are copies; plugin code is shared read-only; synced/, data/,
			// store/ and any other directory Claude writes stay local.
			"plugins": {copyFiles: true, dirs: map[string]*mirrorSpec{
				"marketplaces": flatMirror(), "repos": flatMirror(), "cache": depthMirror(3),
			}},
		},
	},
	contracts.HarnessCodex: {
		copies: []string{"hooks.json", "AGENTS.md"},
		mirrors: map[string]*mirrorSpec{
			// Codex installs its bundled skills into skills/.system itself.
			"skills":  flatMirror(".system"),
			"prompts": flatMirror(),
			"plugins": {copyFiles: true, dirs: map[string]*mirrorSpec{"cache": depthMirror(3)}},
		},
	},
}

// composeFromOwner applies the harness composition of userDir into dir.
func (s *Service) composeFromOwner(info *ProfileInfo, dir, userDir string, h contracts.Harness) {
	c := compositions[h]
	names := append([]string{}, c.copies...)
	for n := range c.mirrors {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if credentialNames[name] {
			continue
		}
		p := filepath.Join(dir, name)
		if userDir == "" {
			info.Skipped = append(info.Skipped, name)
			continue
		}
		src := filepath.Join(userDir, name)
		st, err := os.Stat(src)
		if err != nil {
			// Owner has none: drop a legacy link to it; leave profile-local state.
			s.removeLegacyLink(p, src)
			info.Skipped = append(info.Skipped, name)
			continue
		}
		if spec, ok := c.mirrors[name]; ok && st.IsDir() {
			if s.mirror(info, name, p, src, spec) {
				info.Linked = append(info.Linked, Link{Name: name + "/", Target: src})
			}
			continue
		}
		if !st.Mode().IsRegular() {
			info.Skipped = append(info.Skipped, name)
			continue
		}
		if s.copyIn(info, name, p, src) {
			info.Copied = append(info.Copied, name)
		}
	}
	if h == contracts.HarnessClaude {
		dropUnreadableMarketplaces(info, dir)
	}
}

// removeLegacyLink removes p only if it is a symlink to target (an older
// composition); the target itself is never touched.
func (s *Service) removeLegacyLink(p, target string) bool {
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		return false
	}
	if cur, _ := os.Readlink(p); cur != target {
		return false
	}
	return os.Remove(p) == nil
}

// copyIn refreshes the profile copy p of the owner file src (0600; written
// only when the bytes differ). A legacy symlink to src is replaced.
func (s *Service) copyIn(info *ProfileInfo, rel, p, src string) bool {
	fi, err := os.Lstat(p)
	switch {
	case err == nil && fi.Mode()&fs.ModeSymlink != 0:
		if !s.removeLegacyLink(p, src) {
			info.Conflicts = append(info.Conflicts, rel+": a symlink not made by Pyre; left untouched")
			return false
		}
	case err == nil && !fi.Mode().IsRegular():
		info.Conflicts = append(info.Conflicts, rel+": exists and is not a file; left untouched")
		return false
	}
	b, err := os.ReadFile(src)
	if err != nil {
		info.Conflicts = append(info.Conflicts, rel+": "+err.Error())
		return false
	}
	if cur, err := os.ReadFile(p); err == nil && bytes.Equal(cur, b) {
		return true
	}
	if err := writeFileAtomic(p, b, 0o600); err != nil {
		info.Conflicts = append(info.Conflicts, rel+": "+err.Error())
		return false
	}
	return true
}

// mirror makes p a real directory mirroring src per spec.
func (s *Service) mirror(info *ProfileInfo, rel, p, src string, spec *mirrorSpec) bool {
	fi, err := os.Lstat(p)
	switch {
	case err == nil && fi.Mode()&fs.ModeSymlink != 0:
		if !s.removeLegacyLink(p, src) {
			info.Conflicts = append(info.Conflicts, rel+": a symlink not made by Pyre; left untouched")
			return false
		}
	case err == nil && !fi.IsDir():
		info.Conflicts = append(info.Conflicts, rel+": exists and is not a directory; left untouched")
		return false
	}
	if err := os.MkdirAll(p, 0o700); err != nil {
		info.Conflicts = append(info.Conflicts, rel+": "+err.Error())
		return false
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		info.Conflicts = append(info.Conflicts, rel+": "+err.Error())
		return false
	}
	want := map[string]bool{} // names linked this pass
	for _, e := range ents {
		name := e.Name()
		if spec.skip[name] || credentialNames[name] {
			continue
		}
		cs, cp := filepath.Join(src, name), filepath.Join(p, name)
		st, err := os.Stat(cs) // follows the owner's own symlinks
		if err != nil {
			continue
		}
		crel := rel + "/" + name
		if st.IsDir() {
			sub := spec.dirs[name]
			if sub == nil {
				sub = spec.each
			}
			switch {
			case sub != nil:
				s.mirror(info, crel, cp, cs, sub)
			case spec.linkDirs:
				if s.link(info, cp, cs) {
					want[name] = true
				}
			}
			continue
		}
		if !st.Mode().IsRegular() {
			continue
		}
		if spec.copyFiles {
			s.copyIn(info, crel, cp, cs)
		} else if s.link(info, cp, cs) {
			want[name] = true
		}
	}
	// Prune links to owner entries that vanished (or are no longer linked).
	if cur, err := os.ReadDir(p); err == nil {
		for _, e := range cur {
			cp := filepath.Join(p, e.Name())
			if want[e.Name()] || e.Type()&fs.ModeSymlink == 0 {
				continue
			}
			if t, _ := os.Readlink(cp); t == filepath.Join(src, e.Name()) {
				_ = os.Remove(cp)
			}
		}
	}
	return true
}

// refreshComposition re-applies the profile composition before a launch so
// copies are current and the owner is never written. Problems are logged;
// the launch proceeds with the profile as it is.
func (s *Service) refreshComposition(dir string, h contracts.Harness) {
	if dir == "" {
		return
	}
	userDir := ""
	switch h {
	case contracts.HarnessClaude:
		userDir = canonicalOrClean(s.cfg.UserClaudeDir)
	case contracts.HarnessCodex:
		userDir = canonicalOrClean(s.cfg.UserCodexDir)
	default:
		return
	}
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return
	}
	var info ProfileInfo
	s.composeFromOwner(&info, dir, userDir, h)
	if len(info.Conflicts) > 0 {
		s.log.Warn("accounts: profile composition conflicts (left untouched)", "profile", dir, "conflicts", strings.Join(info.Conflicts, "; "))
	}
	for _, u := range info.Unavailable {
		s.log.Warn("accounts: profile composition: owner configuration left out", "profile", dir, "reason", u)
	}
}
