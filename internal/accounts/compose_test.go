package accounts

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// syntheticOwnerPlugins builds a fake owner plugin store like the real ones
// on server/laptop: registries, a marketplace, a versioned plugin cache, and
// Claude-written state (synced/, data/, store/).
func syntheticOwnerPlugins(t *testing.T, plugins string) {
	t.Helper()
	files := map[string]string{
		"installed_plugins.json":                           `{"plugins":{"synth@mkt":[{"version":"1.0.0"}]}}`,
		"known_marketplaces.json":                          `{"mkt":{"lastUpdated":"2026-01-01T00:00:00Z"}}`,
		"marketplaces/mkt/.claude-plugin/marketplace.json": `{"name":"mkt"}`,
		"cache/mkt/synth/1.0.0/.claude-plugin/plugin.json": `{"name":"synth"}`,
		"cache/mkt/synth/1.0.0/skills/s/SKILL.md":          "synthetic plugin skill\n",
		"synced/owner-org_owner-acct/.marketplaces.json":   `{}`,
		"synced/.bucket-owner-org_owner-acct":              "",
		"data/synth-mkt/state.json":                        `{"owner":true}`,
		"store/synth.json":                                 `{}`,
	}
	for rel, body := range files {
		p := filepath.Join(plugins, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func writeIn(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestProfileWritesNeverReachOwner: everything a Claude or Codex process
// writes in its profile's settings/plugin/skill state stays in the profile;
// the fake owner home is byte-identical (contents, modes, mtimes, links)
// afterwards, while the owner's skills, plugin code and marketplaces stay
// readable through the profile, and a launch refresh picks up owner changes.
func TestProfileWritesNeverReachOwner(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	syntheticOwnerPlugins(t, filepath.Join(e.userClaude, "plugins"))
	writeIn(t, e.userClaude, "commands/c.md", "synthetic command\n")
	writeIn(t, e.userClaude, "skills/synced/owner-org_owner-acct/.marketplaces.json", "{}")
	writeIn(t, e.userCodex, "skills/.system/bundled/SKILL.md", "owner codex bundled\n")
	writeIn(t, e.userCodex, "skills/mine/SKILL.md", "owner codex skill\n")
	writeIn(t, e.userCodex, "hooks.json", `{"hooks":{}}`)
	writeIn(t, e.userCodex, "plugins/cache/mkt/p/1/plugin.json", `{}`)
	home := filepath.Dir(e.userClaude)
	before := fileState(t, home)

	ca := contracts.Account{AccountID: "acct_synth-a", Provider: contracts.ProviderAnthropic}
	cp, err := e.svc.CreateProfile(ctx, ca, contracts.HarnessClaude)
	if err != nil {
		t.Fatal(err)
	}
	xa := contracts.Account{AccountID: "acct_synth-x", Provider: contracts.ProviderOpenAI}
	xp, err := e.svc.CreateProfile(ctx, xa, contracts.HarnessCodex)
	if err != nil {
		t.Fatal(err)
	}
	if len(cp.Conflicts)+len(xp.Conflicts) != 0 {
		t.Fatalf("conflicts %v %v", cp.Conflicts, xp.Conflicts)
	}

	// Read-only sharing works through the profile.
	for _, rel := range []string{"skills/demo/SKILL.md", "commands/c.md", "plugins/installed_plugins.json",
		"plugins/marketplaces/mkt/.claude-plugin/marketplace.json", "plugins/cache/mkt/synth/1.0.0/skills/s/SKILL.md"} {
		if !bytes.Equal(mustRead(t, filepath.Join(cp.Dir, rel)), mustRead(t, filepath.Join(e.userClaude, rel))) {
			t.Fatalf("%s not readable through the profile", rel)
		}
	}
	if !bytes.Equal(mustRead(t, filepath.Join(xp.Dir, "skills/mine/SKILL.md")), []byte("owner codex skill\n")) {
		t.Fatal("codex skill not shared")
	}
	// Harness-written state is never shared.
	for _, rel := range []string{"plugins/synced", "plugins/data", "plugins/store", "skills/synced"} {
		if _, err := os.Lstat(filepath.Join(cp.Dir, rel)); !os.IsNotExist(err) {
			t.Fatalf("%s must stay profile-local (not linked): %v", rel, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(xp.Dir, "skills", ".system")); !os.IsNotExist(err) {
		t.Fatal("codex skills/.system must not be linked")
	}

	// What Claude does in a run: migrate settings, edit memory, rewrite the
	// plugin registries, create its per-org sync bucket, plugin data and
	// store, clone a marketplace, install a plugin/version, add a skill and
	// an agent.
	for rel, body := range map[string]string{
		"settings.json":                           `{"env":{"DISABLE_AUTOUPDATER":"1"}}`,
		"CLAUDE.md":                               "edited by the run\n",
		"plugins/installed_plugins.json":          `{"plugins":{}}`,
		"plugins/known_marketplaces.json":         `{"official":{}}`,
		"plugins/synced/run-org_run-acct/x":       "run",
		"plugins/synced/.bucket-run-org_run-acct": "",
		"plugins/data/synth-mkt/state.json":       `{"run":true}`,
		"plugins/store/run.json":                  `{}`,
		"plugins/marketplaces/official/x.json":    `{}`,
		"plugins/cache/mkt/other/2.0.0/p.json":    `{}`,
		"plugins/cache/mkt/synth/1.1.0/p.json":    `{}`,
		"skills/new-skill/SKILL.md":               "made by the run\n",
		"skills/synced/run-org_run-acct/x":        "run",
		"agents/new.md":                           "made by the run\n",
	} {
		writeIn(t, cp.Dir, rel, body)
	}
	// What Codex does: install bundled skills, sync plugin caches, rewrite
	// its config/hooks/agents files.
	for rel, body := range map[string]string{
		"skills/.system/bundled/SKILL.md":   "codex bundled v2\n",
		"plugins/cache/remote/p/9/x.json":   `{}`,
		"plugins/cache/mkt/p/2/plugin.json": `{}`,
		"config.toml":                       "model = \"changed\"\n",
		"hooks.json":                        `{"changed":true}`,
		"AGENTS.md":                         "changed\n",
	} {
		writeIn(t, xp.Dir, rel, body)
	}
	sameState(t, "owner home after harness writes", before, fileState(t, home))

	// The next launch re-copies the owner's files (run changes discarded),
	// is still owner-neutral, and picks up owner changes and removals.
	if _, err := e.svc.CreateProfile(ctx, ca, contracts.HarnessClaude); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(cp.Dir, "settings.json")), mustRead(t, filepath.Join(e.userClaude, "settings.json"))) {
		t.Fatal("settings.json not re-copied at the next launch")
	}
	if b := mustRead(t, filepath.Join(cp.Dir, "plugins/synced/run-org_run-acct/x")); string(b) != "run" {
		t.Fatal("profile-local state lost")
	}
	sameState(t, "owner home after a launch refresh", before, fileState(t, home))
	writeIn(t, e.userClaude, "settings.json", `{"theme":"light"}`)
	os.RemoveAll(filepath.Join(e.userClaude, "skills", "demo"))
	writeIn(t, e.userClaude, "skills/added/SKILL.md", "added by the owner\n")
	e.svc.refreshComposition(cp.Dir, contracts.HarnessClaude)
	if string(mustRead(t, filepath.Join(cp.Dir, "settings.json"))) != `{"theme":"light"}` {
		t.Fatal("owner settings change not picked up at launch")
	}
	if _, err := os.Lstat(filepath.Join(cp.Dir, "skills", "demo")); !os.IsNotExist(err) {
		t.Fatal("link to a removed owner skill not pruned")
	}
	if string(mustRead(t, filepath.Join(cp.Dir, "skills/added/SKILL.md"))) != "added by the owner\n" {
		t.Fatal("new owner skill not shared")
	}
	if string(mustRead(t, filepath.Join(cp.Dir, "skills/new-skill/SKILL.md"))) != "made by the run\n" {
		t.Fatal("run-created skill lost")
	}
}

// TestLegacyProfileLinksReplaced: a profile composed by an older Pyre (whole
// settings.json/plugins/skills symlinked to the owner) is converted on the
// next launch; only the links are removed, never the owner's targets.
func TestLegacyProfileLinksReplaced(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	syntheticOwnerPlugins(t, filepath.Join(e.userClaude, "plugins"))
	a := contracts.Account{AccountID: "acct_synth-a", Provider: contracts.ProviderAnthropic}
	info, err := e.svc.CreateProfile(ctx, a, contracts.HarnessClaude)
	if err != nil {
		t.Fatal(err)
	}
	uc := canonicalOrClean(e.userClaude)
	for _, n := range []string{"settings.json", "plugins", "skills"} {
		os.RemoveAll(filepath.Join(info.Dir, n))
		if err := os.Symlink(filepath.Join(uc, n), filepath.Join(info.Dir, n)); err != nil {
			t.Fatal(err)
		}
	}
	home := filepath.Dir(e.userClaude)
	before := fileState(t, home)
	e.svc.refreshComposition(info.Dir, contracts.HarnessClaude)
	for _, n := range []string{"settings.json", "plugins", "skills"} {
		fi, err := os.Lstat(filepath.Join(info.Dir, n))
		if err != nil || fi.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("%s still a legacy link: %v %v", n, fi, err)
		}
	}
	sameState(t, "owner home after converting a legacy profile", before, fileState(t, home))
	writeIn(t, info.Dir, "plugins/synced/run/x", "run")
	writeIn(t, info.Dir, "settings.json", "{}")
	sameState(t, "owner home after writes in the converted profile", before, fileState(t, home))
}
