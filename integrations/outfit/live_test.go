package outfit

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRealOutfitIsolated drives a real Outfit binary (path in
// PYRE_TEST_REAL_OUTFIT) against a fully isolated profile: temporary HOME,
// OUTFIT_HOME, CODEX_HOME, CLAUDE_CONFIG_DIR, and Outfit's file vault. It
// installs an offline synthetic connector whose managed secret is SYNTHETIC
// and proves the adapter's view never contains it. It never touches the
// user's Outfit state, keychain, or harness skill directories.
func TestRealOutfitIsolated(t *testing.T) {
	bin := os.Getenv("PYRE_TEST_REAL_OUTFIT")
	if bin == "" {
		t.Skip("set PYRE_TEST_REAL_OUTFIT to an outfit binary to run")
	}
	printf, err := exec.LookPath("printf")
	if err != nil {
		t.Skip("printf not found")
	}
	_ = printf
	root := t.TempDir()
	home := filepath.Join(root, "home")
	for _, d := range []string{home, filepath.Join(root, "codex"), filepath.Join(root, "claude"), filepath.Join(root, "pkg")} {
		os.MkdirAll(d, 0o700)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("OUTFIT_HOME", filepath.Join(root, "outfit"))
	t.Setenv("OUTFIT_VAULT_BACKEND", "file")
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	const token = "SYNTHETIC-real-outfit-token-c0ffee42"
	pkg := `apiVersion: outfit.jhh3.dev/connector/v1alpha2
key: pyre-synth
display: Pyre synthetic
version: 1.0.0
description: Offline synthetic connector for Pyre adapter verification.
category: Test
credentialMethods:
  - id: synthetic-token
    label: Synthetic token
    description: A synthetic token held by Outfit.
    recommended: true
    custody: outfit
    fields:
      - key: TOKEN
        label: Synthetic token
        secret: true
        environment: [PYRE_SYNTH_TOKEN]
    bindings:
      - {target: process-env, name: PYRE_SYNTH_TOKEN, value: '{{secret.TOKEN}}'}
modes:
  - id: cli
    label: Local printf
    description: Offline echo through printf.
    recommendation: {rank: 1, reason: Only mode.}
    executor: {kind: cli, binary: printf}
    credentialMethods: [synthetic-token]
    defaultCredentialMethod: synthetic-token
    verify: hello
    operations:
      - name: hello
        description: Print a fixed greeting.
        class: read
        cli: {args: ['hello-from-outfit']}
skill:
  summary: Synthetic connector used by Pyre tests.
  guidance: [Synthetic only]
`
	pkgDir := filepath.Join(root, "pkg")
	if err := os.WriteFile(filepath.Join(pkgDir, "connector.yaml"), []byte(pkg), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("outfit %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	run("setup")
	run("connector", "install", pkgDir, "--trust")
	run("connector", "add", "pyre-synth", "--name", "synth-work", "--set", "TOKEN="+token)

	a := New(Options{Binary: bin})
	ctx := context.Background()
	st := a.Detect(ctx)
	if !st.Present || st.Health != "ok" {
		t.Fatalf("detect = %+v", st)
	}
	var seen []string
	list, err := a.Connections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var synth *Connection
	for i := range list {
		if list[i].Name == "synth-work" {
			synth = &list[i]
		}
	}
	if synth == nil || synth.Connector != "pyre-synth" || synth.Status != "active" {
		t.Fatalf("connections = %+v", list)
	}
	raw, _ := json.Marshal(list)
	seen = append(seen, string(raw))
	a.mu.Lock()
	hasInventory := !a.noInventory
	a.mu.Unlock()
	t.Logf("outfit %s inventory=%v custody=%q env=%v verified=%v", st.Version, hasInventory, synth.Custody, synth.Environment, synth.LastVerifiedAt != nil)
	if hasInventory && (synth.Custody != "outfit" || strings.Join(synth.Environment, ",") != "PYRE_SYNTH_TOKEN" || synth.LastVerifiedAt == nil) {
		t.Fatalf("inventory metadata incomplete: %+v", synth)
	}
	c, err := a.Check(ctx, "synth-work")
	if err != nil || c.State != "verified" {
		t.Fatalf("check = %+v %v", c, err)
	}
	res, err := a.Invoke(ctx, InvokeRequest{Connection: "synth-work", Operation: "hello"})
	if err != nil || !res.OK || !strings.Contains(string(res.Envelope), "hello-from-outfit") {
		t.Fatalf("invoke = %+v %v", res, err)
	}
	seen = append(seen, string(res.Envelope))
	inv, err := a.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(inv)
	seen = append(seen, string(raw))
	for _, s := range seen {
		if strings.Contains(s, token) {
			t.Fatalf("adapter output disclosed the synthetic token: %s", s)
		}
	}
	// Outfit's own state files must not hold the plaintext either (file vault is encrypted).
	filepath.Walk(filepath.Join(root, "outfit"), func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), token) {
				t.Errorf("plaintext token found in outfit state file %s", p)
			}
		}
		return nil
	})
}
