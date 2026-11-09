package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRealCodexHookTrustMirror checks the hooks.state write format against
// the installed Codex (0.162+). Skipped unless PYRE_REAL_HOOKTRUST=1. It
// runs an app-server on a scratch CODEX_HOME (no login, no turn) with a
// synthetic hooks.json, plays the owner on another scratch home that
// trusted that hook's content, mirrors, and reads hooks/list again. Nothing
// outside the scratch directories is touched.
func TestRealCodexHookTrustMirror(t *testing.T) {
	if os.Getenv("PYRE_REAL_HOOKTRUST") != "1" {
		t.Skip("set PYRE_REAL_HOOKTRUST=1 to run against the installed codex")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex not installed")
	}
	dir, err := os.MkdirTemp("", "pht")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	profile, owner, cwd := filepath.Join(dir, "profile"), filepath.Join(dir, "owner"), filepath.Join(dir, "work")
	for _, d := range []string{profile, owner, cwd} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	hooksJSON := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"true"}]}]}}`
	for _, h := range []string{profile, owner} {
		if err := os.WriteFile(filepath.Join(h, "hooks.json"), []byte(hooksJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(profile, "config.toml"), []byte("[features]\nhooks = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(Options{NodeID: "node_real", TmuxSocket: filepath.Join(dir, "tmux", "t.sock"), StateRoot: filepath.Join(dir, "st"),
		CodexBin: codex, DisableTUI: true, OwnerCodexHome: owner})
	if err != nil {
		t.Fatal(err)
	}
	defer a.srv.KillServer(context.Background())
	hs, err := a.homeServer(profile)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_ = hs.addRef("probe-hooktrust")
	defer hs.release(ctx, "probe-hooktrust")
	c, err := hs.ensure(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	list := func() []hookMetadata {
		var r hooksListResponse
		if err := c.call(ctx, "hooks/list", hooksListParams{Cwds: []string{cwd}}, &r); err != nil {
			t.Fatalf("hooks/list: %v", err)
		}
		var out []hookMetadata
		for _, e := range r.Data {
			out = append(out, e.Hooks...)
		}
		return out
	}
	before := list()
	t.Logf("before: %+v", before)
	if len(before) != 1 || before[0].TrustStatus != "untrusted" {
		t.Fatalf("want one untrusted hook: %+v", before)
	}
	key := ownerHookKey(before[0].Key, profile, owner)
	cfgHash, contentHash, _ := strings.Cut(before[0].CurrentHash, "/")
	ownerCfg := "[hooks.state.\"" + key + "\"]\ntrusted_hash = \"" + cfgHash + "\"\n"
	if contentHash != "" {
		ownerCfg += "trusted_content_hash = \"" + contentHash + "\"\n"
	}
	if err := os.WriteFile(filepath.Join(owner, "config.toml"), []byte(ownerCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	a.mirrorHookTrust(ctx, c, profile, cwd)
	after := list()
	t.Logf("after: %+v", after)
	if len(after) != 1 || after[0].TrustStatus != "trusted" {
		b, _ := os.ReadFile(filepath.Join(profile, "config.toml"))
		t.Fatalf("not trusted after the mirror: %+v\nprofile config.toml:\n%s", after, b)
	}
	b, _ := os.ReadFile(filepath.Join(profile, "config.toml"))
	t.Logf("profile config.toml after:\n%s", b)
}
