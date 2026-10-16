package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jhh3/pyre/internal/cloud"
)

// cliFakeGcloud is a minimal in-memory gcloud for the CLI wiring tests
// (internal/cloud has the thorough ones). It never runs the real binary.
type cliFakeGcloud struct {
	mu         sync.Mutex
	calls      []string
	vms        map[string]map[string]any
	bootConfig string // the last pyre-config passed by file
}

func (f *cliFakeGcloud) Run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := strings.Join(args, " ")
	f.calls = append(f.calls, j)
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--metadata-from-file="); ok {
			for _, kv := range strings.Split(v, ",") {
				if k, p, _ := strings.Cut(kv, "="); k == cloud.MetaConfig {
					b, _ := os.ReadFile(p)
					f.bootConfig = string(b)
				}
			}
		}
	}
	nf := &cloud.GcloudError{Msg: "was not found"}
	switch {
	case strings.HasPrefix(j, "config get-value project"):
		return []byte("synthetic-project\n"), nil
	case strings.HasPrefix(j, "config get-value compute/zone"):
		return []byte("zz-test1-a\n"), nil
	case strings.HasPrefix(j, "compute firewall-rules describe"):
		return []byte("{}"), nil
	case strings.HasPrefix(j, "compute images describe-from-family"):
		return nil, nf
	case strings.HasPrefix(j, "compute instances describe"):
		if vm := f.vms[args[3]]; vm != nil {
			return json.Marshal(vm)
		}
		return nil, nf
	case strings.HasPrefix(j, "compute instances create"):
		f.vms[args[3]] = map[string]any{"name": args[3], "zone": "zones/zz-test1-a", "machineType": "x/e2-standard-8", "status": "RUNNING",
			"labels": map[string]string{"pyre-managed": "true", "pyre-node": strings.TrimPrefix(args[3], "pyre-"), "pyre-os": "linux"}}
		return []byte("[]"), nil
	case strings.HasPrefix(j, "compute instances list"):
		var all []any
		for _, vm := range f.vms {
			all = append(all, vm)
		}
		return json.Marshal(all)
	case strings.HasPrefix(j, "compute instances delete"):
		delete(f.vms, args[3])
		return nil, nil
	case strings.HasPrefix(j, "compute disks list"):
		return []byte("[]"), nil
	case strings.HasPrefix(j, "compute disks add-labels"), strings.HasPrefix(j, "compute instances remove-metadata"),
		strings.HasPrefix(j, "compute instances add-metadata"):
		return nil, nil
	}
	return nil, errors.New("unexpected gcloud " + j)
}

type cliFakeKeys struct{}

func (cliFakeKeys) Mint(context.Context, string) (cloud.AuthKey, error) {
	return cloud.AuthKey{Key: "tskey-auth-SYNTHETIC-cli"}, nil
}
func (cliFakeKeys) Revoke(context.Context, string) error { return nil }

func TestCloudNodeCommands(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	home := e["HOME"]
	_ = os.MkdirAll(filepath.Join(home, ".config", "pyre"), 0o700)
	_ = os.WriteFile(filepath.Join(home, ".config", "pyre", "release-allowed-signers"), []byte("pyre-release namespaces=\"pyre-release\" ssh-ed25519 AAAASYNTHETIC pyre-release\n"), 0o600)
	cfgPath := filepath.Join(home, "cloud.json")
	_ = os.WriteFile(cfgPath, []byte(`{"hub_url":"https://hub.synthetic.ts.net:7443","release":{"url":"https://releases.invalid/pyre/0.0.0/","version":"0.0.0"}}`), 0o600)
	e = e.with(cloud.EnvConfig, cfgPath)
	g := &cliFakeGcloud{vms: map[string]map[string]any{}}
	app := func() *App { return &App{CloudRunner: g, CloudKeys: cliFakeKeys{}} }

	r := run(t, e, app(), "--json", "node", "create", "--cloud", "gcp", "--name", "synth-cloud", "--no-wait")
	expectCode(t, r, 0)
	var cr cloud.CreateResult
	if err := json.Unmarshal([]byte(r.stdout), &cr); err != nil || cr.Name != "synth-cloud" || cr.NodeID == "" || cr.Connected {
		t.Fatalf("create: %v %s", err, r)
	}
	for _, c := range g.calls {
		if strings.Contains(c, "tskey-auth-SYNTHETIC") || strings.Contains(c, "mock-enroll-") {
			t.Fatalf("secret in gcloud argv: %s", c)
		}
	}
	// The TUI snapshot sits beside the client config.
	snap := cloud.CacheReader(filepath.Join(filepath.Dir(e[EnvConfig]), cloud.CacheFileName))("synth-cloud")
	if !strings.Contains(snap, "gcp") {
		t.Fatalf("cloud snapshot: %q", snap)
	}

	r = run(t, e, app(), "--json", "nodes", "--cloud")
	expectCode(t, r, 0)
	var ns []cloud.CloudNode
	if err := json.Unmarshal([]byte(r.stdout), &ns); err != nil || len(ns) != 1 || ns[0].Connection != "never_connected" && ns[0].Connection == "" {
		t.Fatalf("ls --cloud: %v %s", err, r)
	}
	r = run(t, e, app(), "node", "ls", "--cloud")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "synth-cloud") || !strings.Contains(r.stdout, "EST $/H") {
		t.Fatalf("human ls --cloud: %s", r)
	}

	r = run(t, e, app(), "--json", "node", "create", "--cloud", "gcp", "--name", "synth-cloud", "--no-wait")
	expectCode(t, r, ExitConflict)

	r = run(t, e, app(), "--json", "node", "create", "--cloud", "aws")
	expectCode(t, r, ExitUsage)

	r = run(t, e, app(), "--json", "node", "delete", "synth-cloud")
	expectCode(t, r, 0)
	var dr cloud.DeleteResult
	if err := json.Unmarshal([]byte(r.stdout), &dr); err != nil || dr.Instance != "pyre-synth-cloud" || dr.RevokedNode == "" {
		t.Fatalf("delete: %v %s", err, r)
	}
	r = run(t, e, app(), "--json", "node", "delete", "synth-cloud")
	expectCode(t, r, ExitNotFound)
}

func TestCloudCreateToolchainFlags(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	home := e["HOME"]
	_ = os.MkdirAll(filepath.Join(home, ".config", "pyre"), 0o700)
	_ = os.WriteFile(filepath.Join(home, ".config", "pyre", "release-allowed-signers"), []byte("pyre-release namespaces=\"pyre-release\" ssh-ed25519 AAAASYNTHETIC pyre-release\n"), 0o600)
	cfgPath := filepath.Join(home, "cloud.json")
	_ = os.WriteFile(cfgPath, []byte(`{"hub_url":"https://hub.synthetic.ts.net:7443","release":{"url":"https://releases.invalid/pyre/0.0.0/","version":"0.0.0"},
	  "gcp":{"toolchains":["electron"]}}`), 0o600)
	e = e.with(cloud.EnvConfig, cfgPath)
	g := &cliFakeGcloud{vms: map[string]map[string]any{}}
	app := func() *App { return &App{CloudRunner: g, CloudKeys: cliFakeKeys{}} }

	// Repeatable and comma-separated; a flag's version replaces the configured one.
	r := run(t, e, app(), "--json", "node", "create", "--cloud", "gcp", "--name", "synth-win", "--os", "windows", "--no-wait",
		"--toolchain", "node@24.21.0", "--toolchain", "bun@1.3.14,rust", "--toolchain", "cmake", "--toolchain", "llvm")
	expectCode(t, r, 0)
	var bc cloud.BootConfig
	if err := json.Unmarshal([]byte(g.bootConfig), &bc); err != nil {
		t.Fatalf("boot config: %v %q", err, g.bootConfig)
	}
	want := "git python vs-buildtools node@24.21.0 pnpm bun@1.3.14 rust cmake llvm gh"
	if got := strings.Join(bc.Toolchains, " "); got != want {
		t.Fatalf("toolchains: %s, want %s", got, want)
	}
	r = run(t, e, app(), "--json", "node", "create", "--cloud", "gcp", "--name", "synth-bad", "--no-wait", "--toolchain", "node@latest")
	expectCode(t, r, ExitUsage)
	if !strings.Contains(r.stderr, "node@latest") {
		t.Fatalf("bad toolchain error: %s", r)
	}
}
