package updates

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/configuration"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/extensions"
)

// fakeScript returns a synthetic tool: `--version` prints version and exits
// with code; `version --json` prints a pyred protocol descriptor.
func fakeScript(version string, code int, protoMin, protoMax int) string {
	return fmt.Sprintf(`#!/bin/sh
case "$1" in
  --version) echo "%s (Synthetic Tool)"; exit %d ;;
  version) echo '{"version":"%s","protocol":%d,"min_protocol":%d}'; exit %d ;;
  smoke) exit 0 ;;
esac
exit 0
`, version, code, version, protoMax, protoMin, code)
}

func writeExe(t *testing.T, p, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func digestOf(t *testing.T, p string) contracts.Digest {
	t.Helper()
	d, err := sha256File(p)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

type runs struct {
	mu sync.Mutex
	r  []configuration.RunRef
}

func (f *runs) ActiveRuns(ctx context.Context) ([]configuration.RunRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]configuration.RunRef(nil), f.r...), nil
}

type reporter struct {
	mu  sync.Mutex
	evs []contracts.Event
}

func (r *reporter) Report(ctx context.Context, kind contracts.ReportKind, payload any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ev, ok := payload.(contracts.Event); ok {
		r.evs = append(r.evs, ev)
	}
	return nil
}

func (r *reporter) outcomes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.evs {
		var u UpdateResult
		_ = json.Unmarshal(e.Payload, &u)
		out = append(out, string(e.Kind)+":"+u.Version+":"+u.Outcome)
	}
	return out
}

type tclock struct{ n atomic.Int64 }

func (c *tclock) Now() time.Time {
	return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC).Add(time.Duration(c.n.Add(1)) * time.Second)
}

type fixture struct {
	t    *testing.T
	root string
	m    *Manager
	runs *runs
	rep  *reporter
}

func newFixture(t *testing.T, mod func(*Options)) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{t: t, root: root, runs: &runs{}, rep: &reporter{}}
	opt := Options{StateDir: filepath.Join(root, "state"), NodeID: "node_synth-mac", Clock: &tclock{}, Runs: f.runs, Reporter: f.rep, TestMode: true, HealthTimeout: 10 * time.Second,
		// Never read the developer's pinned release key.
		ReleaseSigners: filepath.Join(root, "no-release-allowed-signers")}
	if mod != nil {
		mod(&opt)
	}
	m, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	f.m = m
	return f
}

func (f *fixture) artifact(name, version string, code int) (string, contracts.Digest) {
	p := writeExe(f.t, filepath.Join(f.root, "artifacts", name+"-"+version), fakeScript(version, code, 1, 1))
	return p, digestOf(f.t, p)
}

func (f *fixture) stageActivate(tool, version string) contracts.ToolVersion {
	f.t.Helper()
	ctx := context.Background()
	p, d := f.artifact(tool, version, 0)
	if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: tool, Version: version, Source: p, Digest: d}); err != nil {
		f.t.Fatal(err)
	}
	tv, err := f.m.Activate(ctx, contracts.UpdateParams{Tool: tool, Version: version})
	if err != nil {
		f.t.Fatal(err)
	}
	return tv
}

func byVersion(list []contracts.ToolVersion, tool, v string) contracts.ToolVersion {
	for _, tv := range list {
		if tv.Tool == tool && tv.Version == v {
			return tv
		}
	}
	return contracts.ToolVersion{}
}

// A16: stage/activate while a run is pinned; forced health failure reverts;
// rollback restores the previous version; the pinned run is never affected.
func TestStagedUpdateWithPinnedRunAndForcedFailure(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	v1 := f.stageActivate("claude", "2.1.289")
	pin, ok := f.m.Pinned(ctx, "claude")
	if !ok || pin.Version != "2.1.289" || pin.Path != v1.Path || strings.Contains(pin.Path, "current") {
		t.Fatalf("pin: %+v", pin)
	}
	// A running session launched with the pinned executable.
	f.runs.r = []configuration.RunRef{{RunID: "run_synth-standalone-1", SessionID: "ses_synth-1", Harness: contracts.HarnessClaude, Executable: pin.Path}}
	pinnedBefore, _ := os.ReadFile(pin.Path)

	p2, d2 := f.artifact("claude", "2.1.300", 0)
	staged, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "claude", Version: "2.1.300", Source: p2, Digest: d2})
	if err != nil {
		t.Fatal(err)
	}
	if staged.State != StateStaged || staged.Health != "ok" || staged.Digest != d2 {
		t.Fatalf("staged: %+v", staged)
	}
	if pin2, _ := f.m.Pinned(ctx, "claude"); pin2.Version != "2.1.289" {
		t.Fatal("staging changed the default for new runs")
	}
	act, err := f.m.Activate(ctx, contracts.UpdateParams{Tool: "claude", Version: "2.1.300"})
	if err != nil {
		t.Fatal(err)
	}
	if act.State != StateActive {
		t.Fatalf("activate: %+v", act)
	}
	newPin, _ := f.m.Pinned(ctx, "claude")
	if newPin.Version != "2.1.300" || newPin.Path == pin.Path {
		t.Fatalf("new runs should use 2.1.300: %+v", newPin)
	}
	if b, err := os.ReadFile(pin.Path); err != nil || !bytes.Equal(b, pinnedBefore) {
		t.Fatal("running session's pinned executable changed")
	}
	list, _ := f.m.List(ctx)
	old := byVersion(list, "claude", "2.1.289")
	if old.State != StatePrevious || !reflect.DeepEqual(old.InUseByRuns, []string{"run_synth-standalone-1"}) {
		t.Fatalf("old version: %+v", old)
	}
	if removed, _ := f.m.Prune(ctx); len(removed) != 0 {
		t.Fatalf("pruned a referenced rollback target: %+v", removed)
	}

	// Forced health failure on the next activation reverts automatically.
	p3, d3 := f.artifact("claude", "2.1.301", 0)
	if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "claude", Version: "2.1.301", Source: p3, Digest: d3}); err != nil {
		t.Fatal(err)
	}
	failed, err := f.m.Activate(ctx, contracts.UpdateParams{Tool: "claude", Version: "2.1.301", SimulateHealthFailure: true})
	if !contracts.IsCode(err, contracts.ErrConflict) || failed.State != StateFailed || !strings.HasPrefix(failed.Health, "failed: ") {
		t.Fatalf("forced failure: %v %+v", err, failed)
	}
	if !strings.Contains(err.Error(), "reverted to 2.1.300") {
		t.Fatalf("error should say what was restored: %v", err)
	}
	if p, _ := f.m.Pinned(ctx, "claude"); p.Version != "2.1.300" {
		t.Fatalf("not reverted: %+v", p)
	}
	if target, _ := os.Readlink(filepath.Join(f.m.toolDir("claude"), "current")); target != "2.1.300" {
		t.Fatalf("current -> %s", target)
	}
	if _, err := os.Stat(f.m.versionDir("claude", "2.1.301")); !os.IsNotExist(err) {
		t.Fatal("failed payload kept")
	}
	if _, err := f.m.Activate(ctx, contracts.UpdateParams{Tool: "claude", Version: "2.1.301"}); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("activating a failed version: %v", err)
	}

	// Explicit rollback.
	rb, err := f.m.Rollback(ctx, contracts.UpdateParams{Tool: "claude"})
	if err != nil || rb.Version != "2.1.289" || rb.State != StateActive {
		t.Fatalf("rollback: %v %+v", err, rb)
	}
	list, _ = f.m.List(ctx)
	if byVersion(list, "claude", "2.1.300").State != StatePrevious || byVersion(list, "claude", "2.1.301").State != StateFailed {
		t.Fatalf("states after rollback: %+v", list)
	}
	want := []string{
		"update.result:2.1.289:activated", "update.staged:2.1.300:staged", "update.result:2.1.300:activated",
		"update.staged:2.1.301:staged", "update.result:2.1.301:failed_reverted", "update.result:2.1.289:rolled_back",
	}
	got := f.rep.outcomes()
	if !reflect.DeepEqual(got[1:], want) {
		t.Fatalf("events:\n got %v\nwant %v", got, want)
	}
}

func TestChecksumVerification(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	p, _ := f.artifact("codex", "0.161.0", 0)
	bad := contracts.Digest("sha256:" + strings.Repeat("0", 64))
	_, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "codex", Version: "0.161.0", Source: p, Digest: bad})
	if !contracts.IsCode(err, contracts.ErrInvalid) || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("mismatch: %v", err)
	}
	if list, _ := f.m.List(ctx); len(list) != 0 {
		t.Fatalf("mismatched candidate recorded: %+v", list)
	}
	if _, err := os.Stat(f.m.versionDir("codex", "0.161.0")); !os.IsNotExist(err) {
		t.Fatal("payload left behind")
	}
	if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "codex", Source: p, Digest: "md5:abc"}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("bad digest format: %v", err)
	}
	if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "codex", Source: "http://synth.invalid/codex"}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("plain http accepted: %v", err)
	}
}

func tarGz(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		mode := int64(0o644)
		if strings.Contains(name, "bin/") {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(content))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestHTTPSArchiveStageRequiresDigest(t *testing.T) {
	archive := tarGz(t, map[string]string{"codex-0.161.0/bin/codex": fakeScript("0.161.0", 0, 1, 1), "codex-0.161.0/README": "synthetic"})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/codex-0.161.0.tar.gz" {
			http.NotFound(w, r)
			return
		}
		w.Write(archive)
	}))
	defer srv.Close()
	f := newFixture(t, func(o *Options) { o.HTTP = srv.Client() })
	ctx := context.Background()
	url := srv.URL + "/codex-0.161.0.tar.gz"
	if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "codex", Source: url}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("unverified download accepted: %v", err)
	}
	d := extensions.DigestBytes(archive)
	tv, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "codex", Source: url, Digest: d})
	if err != nil {
		t.Fatal(err)
	}
	if tv.Version != "0.161.0" || tv.Digest != d || !strings.HasSuffix(tv.Path, filepath.Join("bin", "codex")) {
		t.Fatalf("staged: %+v", tv)
	}
	if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "codex", Source: srv.URL + "/missing.tar.gz", Digest: d}); !contracts.IsCode(err, contracts.ErrUnavailable) {
		t.Fatalf("404: %v", err)
	}
	evil := tarGz(t, map[string]string{"../evil": "x"})
	p := filepath.Join(f.root, "evil.tar.gz")
	os.WriteFile(p, evil, 0o644)
	if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "codex", Version: "6.6.6", Source: p}); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("path traversal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "state", "tools", "evil")); !os.IsNotExist(err) {
		t.Fatal("traversal wrote outside")
	}
}

func TestStageHealthFailures(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	p, d := f.artifact("claude", "2.1.300", 1)
	tv, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "claude", Version: "2.1.300", Source: p, Digest: d})
	if !contracts.IsCode(err, contracts.ErrConflict) || tv.State != StateFailed || !strings.HasPrefix(tv.Health, "failed: ") {
		t.Fatalf("broken binary: %v %+v", err, tv)
	}
	ce := contracts.AsError(err)
	if _, ok := ce.Details["tool_version"]; !ok {
		t.Fatal("error lacks tool_version detail")
	}
	p2, d2 := f.artifact("claude", "2.1.299", 0)
	if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "claude", Version: "2.1.302", Source: p2, Digest: d2}); err == nil || !strings.Contains(err.Error(), "expected 2.1.302") {
		t.Fatalf("version mismatch: %v", err)
	}
	if _, err := f.m.Activate(ctx, contracts.UpdateParams{Tool: "claude", Version: "2.1.300"}); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("activate failed: %v", err)
	}
	if p, ok := f.m.Pinned(ctx, "claude"); ok {
		t.Fatalf("failed stage became default: %+v", p)
	}
	// Re-staging the same version after fixing it works.
	p3 := writeExe(t, filepath.Join(f.root, "fixed/claude"), fakeScript("2.1.300", 0, 1, 1))
	if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "claude", Version: "2.1.300", Source: p3}); err != nil {
		t.Fatal(err)
	}
	// SimulateHealthFailure outside test mode is refused.
	g := newFixture(t, func(o *Options) { o.TestMode = false })
	if _, err := g.m.Stage(ctx, contracts.UpdateParams{Tool: "claude", Source: p3, SimulateHealthFailure: true}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("simulate outside test mode: %v", err)
	}
	// Smoke command failures fail the stage.
	h := newFixture(t, func(o *Options) { o.Smoke = map[string][]string{"claude": {"--version", "--fail-smoke"}} })
	bad := writeExe(t, filepath.Join(h.root, "smoke/claude"), "#!/bin/sh\n[ \"$2\" = \"--fail-smoke\" ] && exit 3\necho 2.1.300\n")
	if _, err := h.m.Stage(ctx, contracts.UpdateParams{Tool: "claude", Version: "2.1.300", Source: bad}); err == nil || !strings.Contains(err.Error(), "smoke") {
		t.Fatalf("smoke: %v", err)
	}
}

// npm packages install into an isolated prefix inside the version dir.
func TestNPMStageIsolatedPrefix(t *testing.T) {
	root := t.TempDir()
	log := filepath.Join(root, "npm.log")
	npm := writeExe(t, filepath.Join(root, "bin/npm"), `#!/bin/sh
echo "$@ global=$npm_config_global" >> `+log+`
case "$1" in
  pack)
    dest="$4"
    printf 'synthetic tarball for %s' "$2" > "$dest/synth-codex-0.161.0.tgz"
    echo "synth-codex-0.161.0.tgz" ;;
  install)
    prefix="$3"
    mkdir -p "$prefix/node_modules/synth-codex/bin" "$prefix/node_modules/.bin"
    cat > "$prefix/node_modules/synth-codex/bin/codex.js" <<'EOF'
#!/bin/sh
echo "codex-cli 0.161.0"
EOF
    chmod +x "$prefix/node_modules/synth-codex/bin/codex.js"
    ln -s ../synth-codex/bin/codex.js "$prefix/node_modules/.bin/codex" ;;
esac
`)
	f := newFixture(t, func(o *Options) { o.NPM = npm })
	ctx := context.Background()
	if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "codex", Source: "npm:@synth/codex@0.161.0"}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("npm without digest: %v", err)
	}
	want := extensions.DigestBytes([]byte("synthetic tarball for @synth/codex@0.161.0"))
	tv, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "codex", Version: "0.161.0", Source: "npm:@synth/codex@0.161.0", Digest: want})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tv.Path, f.m.versionDir("codex", "0.161.0")) || !strings.Contains(tv.Path, filepath.Join("prefix", "node_modules", ".bin", "codex")) {
		t.Fatalf("npm exe path: %s", tv.Path)
	}
	l, _ := os.ReadFile(log)
	if !strings.Contains(string(l), "--prefix "+filepath.Join(f.root, "state", "tools", "codex")) || !strings.Contains(string(l), "global=false") {
		t.Fatalf("npm not isolated: %s", l)
	}
	if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "codex", Version: "0.161.1", Source: "npm:@synth/codex@0.161.0", Digest: contracts.Digest("sha256:" + strings.Repeat("1", 64))}); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("npm checksum: %v", err)
	}
	if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "codex", Source: "npm:--evil"}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("npm flag injection: %v", err)
	}
}

func TestPyredProtocolCompatibility(t *testing.T) {
	hub := ProtocolRange{Min: 1, Max: 1}
	f := newFixture(t, func(o *Options) { o.HubProtocol = func() (ProtocolRange, bool) { return hub, true } })
	ctx := context.Background()
	newer := writeExe(t, filepath.Join(f.root, "a/pyred-0.9.0"), fakeScript("0.9.0-synth", 0, 2, 2))
	if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "pyred", Version: "0.9.0-synth", Source: newer}); err != nil {
		t.Fatal(err)
	}
	_, err := f.m.Activate(ctx, contracts.UpdateParams{Tool: "pyred", Version: "0.9.0-synth"})
	if !contracts.IsCode(err, contracts.ErrProtocolMismatch) {
		t.Fatalf("protocol mismatch: %v", err)
	}
	if !strings.Contains(err.Error(), "Upgrade the hub") || !strings.Contains(err.Error(), "speaks protocol 2..2; hub supports 1..1") {
		t.Fatalf("message not actionable: %v", err)
	}
	if _, ok := f.m.Pinned(ctx, "pyred"); ok {
		t.Fatal("incompatible pyred activated")
	}
	if _, err := f.m.PlanDaemonSwap(ctx, "0.9.0-synth"); !contracts.IsCode(err, contracts.ErrProtocolMismatch) {
		t.Fatalf("swap plan for incompatible pyred: %v", err)
	}
	compat := writeExe(t, filepath.Join(f.root, "b/pyred-0.8.1"), fakeScript("0.8.1-synth", 0, 1, 2))
	if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "pyred", Version: "0.8.1-synth", Source: compat}); err != nil {
		t.Fatal(err)
	}
	plan, err := f.m.PlanDaemonSwap(ctx, "0.8.1-synth")
	if err != nil || len(plan.Steps) < 4 || len(plan.Rollback) == 0 {
		t.Fatalf("swap plan: %v %+v", err, plan)
	}
	if _, err := f.m.Activate(ctx, contracts.UpdateParams{Tool: "pyred", Version: "0.8.1-synth"}); err != nil {
		t.Fatal(err)
	}
	// Direct helper: older node vs newer hub.
	err = CheckProtocolCompat("0.1.0", ProtocolRange{1, 1}, ProtocolRange{2, 3})
	if !contracts.IsCode(err, contracts.ErrProtocolMismatch) || !strings.Contains(err.Error(), "supports protocol 2") {
		t.Fatalf("old node: %v", err)
	}
	if CheckProtocolCompat("x", LocalProtocol(), LocalProtocol()) != nil {
		t.Fatal("self-compat")
	}
}

type fakeNodes struct {
	mu       sync.Mutex
	failNode string
	calls    []string
}

func (f *fakeNodes) Stage(ctx context.Context, node string, p contracts.UpdateParams) (contracts.ToolVersion, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "stage:"+node)
	f.mu.Unlock()
	if node == f.failNode {
		return contracts.ToolVersion{}, contracts.Errorf(contracts.ErrConflict, "health check failed on %s", node)
	}
	return contracts.ToolVersion{Tool: p.Tool, Version: "2.1.300", State: StateStaged}, nil
}

func (f *fakeNodes) Activate(ctx context.Context, node string, p contracts.UpdateParams) (contracts.ToolVersion, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "activate:"+node)
	f.mu.Unlock()
	if p.Version != "2.1.300" || p.Source != "" {
		return contracts.ToolVersion{}, errors.New("activate must name the staged version")
	}
	return contracts.ToolVersion{Tool: p.Tool, Version: p.Version, State: StateActive}, nil
}

func TestCanaryRollout(t *testing.T) {
	ctx := context.Background()
	params := contracts.UpdateParams{Tool: "claude", Source: "npm:@synth/claude@2.1.300"}
	u := &fakeNodes{failNode: "node_canary"}
	rep := Rollout(ctx, u, RolloutPlan{Canary: "node_canary", Nodes: []string{"node_a", "node_b"}, Params: params})
	if !rep.Stopped || rep.Canary.Error == nil || !rep.Nodes[0].Skipped || !rep.Nodes[1].Skipped || len(u.calls) != 1 {
		t.Fatalf("failed canary must stop the rollout: %+v %v", rep, u.calls)
	}
	u = &fakeNodes{}
	rep = Rollout(ctx, u, RolloutPlan{Canary: "node_canary", Nodes: []string{"node_a", "node_b", "node_c"}, Params: params, Concurrency: 2})
	if rep.Stopped || rep.Canary.Active == nil || u.calls[0] != "stage:node_canary" || u.calls[1] != "activate:node_canary" {
		t.Fatalf("canary first: %+v %v", rep, u.calls)
	}
	for _, n := range rep.Nodes {
		if n.Active == nil || n.Skipped {
			t.Fatalf("node not updated: %+v", n)
		}
	}
	u = &fakeNodes{failNode: "node_a"}
	rep = Rollout(ctx, u, RolloutPlan{Canary: "node_canary", Nodes: []string{"node_a", "node_b"}, Params: params, Concurrency: 1})
	if !rep.Stopped || rep.Nodes[0].Error == nil || !rep.Nodes[1].Skipped {
		t.Fatalf("stop after node failure: %+v", rep)
	}
	u = &fakeNodes{failNode: "node_a"}
	rep = Rollout(ctx, u, RolloutPlan{Canary: "node_canary", Nodes: []string{"node_a", "node_b"}, Params: params, ContinueOnError: true})
	if rep.Nodes[1].Active == nil {
		t.Fatalf("continue on error: %+v", rep)
	}
}

// countingRunner records the maximum number of concurrent commands.
type countingRunner struct {
	cur, max atomic.Int32
}

func (c *countingRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) (string, string, error) {
	n := c.cur.Add(1)
	defer c.cur.Add(-1)
	for {
		m := c.max.Load()
		if n <= m || c.max.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(50 * time.Millisecond)
	return ExecRunner{}.Run(ctx, dir, env, name, args...)
}

func TestStageConcurrencyBounded(t *testing.T) {
	cr := &countingRunner{}
	f := newFixture(t, func(o *Options) { o.Runner = cr; o.MaxConcurrent = 1 })
	ctx := context.Background()
	var wg sync.WaitGroup
	for i, tool := range []string{"claude", "codex", "pyre", "plugin-tool"} {
		p, _ := f.artifact(tool, fmt.Sprintf("1.0.%d", i), 0)
		wg.Add(1)
		go func(tool, p string, i int) {
			defer wg.Done()
			if tool == "pyre" {
				return
			}
			if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: tool, Version: fmt.Sprintf("1.0.%d", i), Source: p}); err != nil {
				t.Error(err)
			}
		}(tool, p, i)
	}
	wg.Wait()
	if cr.max.Load() != 1 {
		t.Fatalf("stage concurrency %d > 1", cr.max.Load())
	}
}

// Plugins staged by the update manager are pinned into Claude launches via
// the configuration overlay (--plugin-dir); external tools are reported.
func TestPluginPinIntegrationAndExternal(t *testing.T) {
	root := t.TempDir()
	ext := writeExe(t, filepath.Join(root, "user/versions/2.1.289/claude"), fakeScript("2.1.289", 0, 1, 1))
	link := filepath.Join(root, "user/bin/claude")
	os.MkdirAll(filepath.Dir(link), 0o755)
	os.Symlink(ext, link)
	f := newFixture(t, func(o *Options) { o.External = map[string]string{"claude": link} })
	ctx := context.Background()
	pin, ok := f.m.Pinned(ctx, "claude")
	if !ok || pin.Path != mustEval(t, ext) || pin.Version != "2.1.289" {
		t.Fatalf("external pin: %+v", pin)
	}
	f.runs.r = []configuration.RunRef{{RunID: "run_synth-ext", Executable: pin.Path}}
	list, _ := f.m.List(ctx)
	if len(list) != 1 || list[0].State != StateActive || list[0].Health != "unchecked" || !reflect.DeepEqual(list[0].InUseByRuns, []string{"run_synth-ext"}) {
		t.Fatalf("external listing: %+v", list)
	}
	if f.m.ActiveVersions(ctx)["claude"] != "2.1.289" {
		t.Fatal("external version")
	}

	pdir := filepath.Join(root, "plugin-src")
	os.MkdirAll(filepath.Join(pdir, ".claude-plugin"), 0o755)
	os.WriteFile(filepath.Join(pdir, ".claude-plugin/plugin.json"), []byte(`{"name":"synth-plug","version":"0.3.1"}`), 0o644)
	if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "plugin:synth-plug", Version: "0.3.1", Source: pdir}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Activate(ctx, contracts.UpdateParams{Tool: "plugin:synth-plug", Version: "0.3.1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "plugin:bad", Version: "1.0.0", Source: root + "/user"}); err == nil {
		t.Fatal("plugin without manifest staged")
	}
	home := filepath.Join(root, "home")
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	cm, err := configuration.New(configuration.Options{StateDir: filepath.Join(root, "state"), NodeID: "node_synth-mac", Homes: extensions.DefaultHomes(home), Versions: f.m})
	if err != nil {
		t.Fatal(err)
	}
	if err := cm.Profiles().Save("", configuration.Profile{Name: "default", Plugins: []configuration.PluginSpec{{Name: "synth-plug", Harness: contracts.HarnessClaude}}}); err != nil {
		t.Fatal(err)
	}
	gen, lo, err := cm.Resolve(ctx, configuration.Scope{Harness: contracts.HarnessClaude})
	if err != nil {
		t.Fatal(err)
	}
	pp, _ := f.m.Pinned(ctx, "plugin:synth-plug")
	found := false
	for i := 0; i < len(lo.ExtraArgs); i++ {
		if lo.ExtraArgs[i] == "--plugin-dir="+pp.Path || (lo.ExtraArgs[i] == "--plugin-dir" && i+1 < len(lo.ExtraArgs) && lo.ExtraArgs[i+1] == pp.Path) {
			found = true
		}
	}
	if !found || gen.RuntimeVersions["plugin:synth-plug"] != "0.3.1" || gen.RuntimeVersions["claude"] != "2.1.289" {
		t.Fatalf("plugin overlay: %v %v", lo.ExtraArgs, gen.RuntimeVersions)
	}
}

func mustEval(t *testing.T, p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type fakeIntegrations map[string]contracts.IntegrationStatus

func (f fakeIntegrations) Status(ctx context.Context) map[string]contracts.IntegrationStatus {
	return f
}

func TestInventoryAndHandlers(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	f.stageActivate("codex", "0.160.0")
	home := filepath.Join(f.root, "home")
	os.MkdirAll(filepath.Join(home, ".claude/skills/s1"), 0o755)
	os.WriteFile(filepath.Join(home, ".claude/skills/s1/SKILL.md"), []byte("x"), 0o644)
	cm, err := configuration.New(configuration.Options{StateDir: filepath.Join(f.root, "state"), NodeID: "node_synth-mac", Homes: extensions.DefaultHomes(home), Versions: f.m})
	if err != nil {
		t.Fatal(err)
	}
	inv := Inventory{NodeID: "node_synth-mac", Tools: f.m, Env: cm,
		Harnesses: func(ctx context.Context) []contracts.HarnessInfo {
			return []contracts.HarnessInfo{{Harness: contracts.HarnessCodex, Version: "0.160.0", Available: true}}
		},
		Integrations: fakeIntegrations{"outfit": {Present: true, Version: "0.4.0", Dirty: true}}}
	out, err := inv.Handler()(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	ni := out.(contracts.NodeInventory)
	if len(ni.Tools) != 1 || len(ni.Extensions) != 1 || len(ni.Harnesses) != 1 || !ni.Integrations["outfit"].Dirty || len(ni.ConfigRoots) != 2 {
		t.Fatalf("inventory: %+v", ni)
	}
	b, _ := json.Marshal(ni)
	var round contracts.NodeInventory
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatal(err)
	}

	h := f.m.Handlers()
	for _, n := range []string{contracts.ActionUpdateStage, contracts.ActionUpdateActivate, contracts.ActionUpdateRollback, contracts.ActionUpdateList} {
		if h[n] == nil {
			t.Fatalf("missing %s", n)
		}
	}
	l, err := h[contracts.ActionUpdateList](ctx, nil)
	if err != nil || len(l.([]contracts.ToolVersion)) != 1 {
		t.Fatalf("list: %v %v", err, l)
	}
	if _, err := h[contracts.ActionUpdateStage](ctx, json.RawMessage(`{"tool":"codex","nope":1}`)); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("unknown field: %v", err)
	}
	if _, err := h[contracts.ActionUpdateRollback](ctx, json.RawMessage(`{"tool":"codex"}`)); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("rollback without target: %v", err)
	}
	p, d := f.artifact("codex", "0.161.0", 0)
	raw, _ := json.Marshal(contracts.UpdateParams{Tool: "codex", Version: "0.161.0", Source: p, Digest: d})
	if _, err := h[contracts.ActionUpdateStage](ctx, raw); err != nil {
		t.Fatal(err)
	}
	// Idempotent re-stage.
	if tv, err := h[contracts.ActionUpdateStage](ctx, raw); err != nil || tv.(contracts.ToolVersion).State != StateStaged {
		t.Fatalf("re-stage: %v", err)
	}
	// No automatic upgrade: staging a newer version never changes the default.
	if pin, _ := f.m.Pinned(ctx, "codex"); pin.Version != "0.160.0" {
		t.Fatal("staged version became default without activation")
	}
}
