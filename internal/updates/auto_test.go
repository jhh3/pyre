package updates

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/configuration"
	"github.com/jhh3/pyre/internal/contracts"
)

// fakeRelease is a ReleaseSource serving one release from a local package
// archive (the codex-package layout: bin/codex plus helpers).
type fakeRelease struct {
	mu    sync.Mutex
	rel   Release
	err   error
	calls int
}

func (s *fakeRelease) Latest(ctx context.Context) (Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.rel, s.err
}

func (s *fakeRelease) set(r Release) {
	s.mu.Lock()
	s.rel, s.err = r, nil
	s.mu.Unlock()
}

// issues records filed issues; failNext makes the next filings fail.
type issues struct {
	mu       sync.Mutex
	titles   []string
	failNext int
}

func (i *issues) file(ctx context.Context, title, body string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.failNext > 0 {
		i.failNext--
		return errors.New("synthetic: hub unreachable")
	}
	i.titles = append(i.titles, title)
	return nil
}

func (i *issues) count() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.titles)
}

// autoNode is one fake node: its own tool store, an optional user install
// of codex on PATH, and an Auto over a shared release source.
type autoNode struct {
	*fixture
	auto   *Auto
	issues *issues
}

func newAutoNode(t *testing.T, src ReleaseSource, external string) *autoNode {
	t.Helper()
	n := &autoNode{issues: &issues{}}
	n.fixture = newFixture(t, func(o *Options) {
		o.Smoke = map[string][]string{"codex": {"smoke"}}
		if external != "" {
			o.External = map[string]string{"codex": writeExe(t, filepath.Join(o.StateDir, "..", "home", "bin", "codex"), fakeScript(external, 0, 1, 1))}
		}
	})
	n.auto = n.m.NewAuto(AutoOptions{Tool: "codex", Source: src, FileIssue: n.issues.file})
	return n
}

// codexPackage writes a codex-package-<target>.tar.gz whose bin/codex
// reports version (exit code code) and returns it as a Release.
func codexPackage(t *testing.T, dir, version string, code int) Release {
	t.Helper()
	p := filepath.Join(dir, "codex-package-"+version+".tar.gz")
	b := tarGz(t, map[string]string{"bin/codex": fakeScript(version, code, 1, 1), "bin/codex-exec-server": "#!/bin/sh\n",
		"codex-path/rg": "#!/bin/sh\n", "codex-package.json": `{"synthetic":true}`})
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return Release{Version: version, Source: p, Digest: digestOf(t, p)}
}

func (n *autoNode) pinned() string {
	pin, _ := n.m.Pinned(context.Background(), "codex")
	return pin.Version
}

// A fleet of fake nodes on one release source: a node on an older user
// install gets the new release staged and activated (bin/codex of the
// package); a node whose user install is already that version is left
// alone; a later release replaces the managed one while a running session
// keeps its pinned executable.
func TestAutoStagesAndActivatesNewRelease(t *testing.T) {
	ctx := context.Background()
	src := &fakeRelease{}
	src.set(codexPackage(t, t.TempDir(), "0.166.0", 0))
	old := newAutoNode(t, src, "0.160.1")
	current := newAutoNode(t, src, "0.166.0")

	st := old.auto.Check(ctx)
	if st.Outcome != AutoUpdated || st.Current != "0.166.0" || st.Latest != "0.166.0" || st.CheckedAt == nil || st.Running {
		t.Fatalf("old node: %+v", st)
	}
	pin, _ := old.m.Pinned(ctx, "codex")
	if pin.Digest != src.rel.Digest || !strings.HasSuffix(pin.Path, filepath.Join("codex", "0.166.0", "bin", "codex")) {
		t.Fatalf("new runs launch %+v", pin)
	}
	if st := current.auto.Check(ctx); st.Outcome != AutoUpToDate || st.Current != "0.166.0" {
		t.Fatalf("current node: %+v", st)
	}
	if tools, _ := current.m.List(ctx); len(tools) != 1 || tools[0].Digest != "" {
		t.Fatalf("current node staged something: %+v", tools)
	}
	if st := old.auto.Check(ctx); st.Outcome != AutoUpToDate {
		t.Fatalf("second check: %+v", st)
	}

	// A session launched on 0.166.0 keeps it across the next update.
	old.runs.r = append(old.runs.r, configuration.RunRef{RunID: "run_synth-1", Harness: contracts.HarnessCodex, Executable: pin.Path})
	src.set(codexPackage(t, t.TempDir(), "0.167.0", 0))
	if st := old.auto.Check(ctx); st.Outcome != AutoUpdated || old.pinned() != "0.167.0" {
		t.Fatalf("next release: %+v", st)
	}
	if _, err := os.Stat(pin.Path); err != nil {
		t.Fatalf("the running session's executable was removed: %v", err)
	}
	if old.issues.count()+current.issues.count() != 0 {
		t.Fatalf("issues filed on success: %v %v", old.issues.titles, current.issues.titles)
	}
	got := old.rep.outcomes()
	for _, want := range []string{"update.staged:0.166.0:staged", "update.result:0.166.0:activated", "update.result:0.167.0:activated"} {
		if !strings.Contains(strings.Join(got, " "), want) {
			t.Errorf("events %v lack %s", got, want)
		}
	}
}

// A release failing its health check is not activated: the old version
// stays the default, one issue is filed, and the version is not retried
// (nor reported again) until a newer release appears.
func TestAutoFailedHealthCheckKeepsActive(t *testing.T) {
	ctx := context.Background()
	src := &fakeRelease{}
	dir := t.TempDir()
	src.set(codexPackage(t, dir, "0.165.0", 0))
	n := newAutoNode(t, src, "0.160.1")
	if st := n.auto.Check(ctx); st.Outcome != AutoUpdated {
		t.Fatalf("setup: %+v", st)
	}
	src.set(codexPackage(t, dir, "0.166.0", 3)) // --version exits 3
	st := n.auto.Check(ctx)
	if st.Outcome != AutoStageFailed || n.pinned() != "0.165.0" || st.Current != "0.165.0" || st.Latest != "0.166.0" {
		t.Fatalf("failed health: %+v (pinned %s)", st, n.pinned())
	}
	if n.issues.count() != 1 || !strings.Contains(n.issues.titles[0], "codex 0.166.0") {
		t.Fatalf("issues: %v", n.issues.titles)
	}
	for range 3 {
		if st := n.auto.Check(ctx); st.Outcome != AutoSkippedFailed || n.pinned() != "0.165.0" {
			t.Fatalf("retry: %+v", st)
		}
	}
	if n.issues.count() != 1 {
		t.Fatalf("issue re-filed: %v", n.issues.titles)
	}
	src.set(codexPackage(t, dir, "0.167.0", 0))
	if st := n.auto.Check(ctx); st.Outcome != AutoUpdated || n.pinned() != "0.167.0" {
		t.Fatalf("next release after a failed one: %+v", st)
	}
}

// A failed post-activation check (the smoke command) reverts to the
// previous version, like a manual activate.
func TestAutoActivationFailureReverts(t *testing.T) {
	ctx := context.Background()
	src := &fakeRelease{}
	dir := t.TempDir()
	src.set(codexPackage(t, dir, "0.165.0", 0))
	n := newAutoNode(t, src, "")
	if st := n.auto.Check(ctx); st.Outcome != AutoUpdated || st.Current != "0.165.0" {
		t.Fatalf("setup without a user install: %+v", st)
	}
	src.set(codexPackage(t, dir, "0.166.0", 0))
	// Staged healthy (an earlier check that stopped before activating),
	// then the binary breaks: the activation's own health check fails.
	if _, err := n.m.Stage(ctx, contracts.UpdateParams{Tool: "codex", Version: "0.166.0", Source: src.rel.Source, Digest: src.rel.Digest}); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(n.m.versionDir("codex", "0.166.0"), "bin", "codex")
	if err := os.Remove(exe); err != nil {
		t.Fatal(err)
	}
	writeExe(t, exe, fakeScript("0.166.0", 4, 1, 1))
	st := n.auto.Check(ctx)
	if st.Outcome != AutoActivateFailed || n.pinned() != "0.165.0" || n.issues.count() != 1 {
		t.Fatalf("activation failure: %+v pinned %s issues %v", st, n.pinned(), n.issues.titles)
	}
	if st := n.auto.Check(ctx); st.Outcome != AutoSkippedFailed || n.issues.count() != 1 {
		t.Fatalf("after activation failure: %+v", st)
	}
}

// Never a downgrade: an older latest release than the managed version or
// the user's own newer install changes nothing; nor does re-activating a
// version the owner rolled back from.
func TestAutoNeverDowngrades(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := &fakeRelease{}
	src.set(codexPackage(t, dir, "0.166.0", 0))
	n := newAutoNode(t, src, "0.160.1")
	if st := n.auto.Check(ctx); st.Outcome != AutoUpdated {
		t.Fatalf("setup: %+v", st)
	}
	src.set(codexPackage(t, dir, "0.165.0", 0))
	if st := n.auto.Check(ctx); st.Outcome != AutoUpToDate || n.pinned() != "0.166.0" {
		t.Fatalf("older release: %+v", st)
	}
	if _, r := findRecT(t, n.m, "0.165.0"); r != nil {
		t.Fatalf("older release staged: %+v", r)
	}

	newer := newAutoNode(t, src, "0.170.0")
	src.set(codexPackage(t, dir, "0.166.0", 0))
	if st := newer.auto.Check(ctx); st.Outcome != AutoUpToDate || newer.pinned() != "0.170.0" {
		t.Fatalf("user install newer than the release: %+v", st)
	}

	// Rolled back by hand from 0.167.0: not re-activated.
	src.set(codexPackage(t, dir, "0.167.0", 0))
	if st := n.auto.Check(ctx); st.Outcome != AutoUpdated {
		t.Fatalf("0.167.0: %+v", st)
	}
	if _, err := n.m.Rollback(ctx, contracts.UpdateParams{Tool: "codex"}); err != nil || n.pinned() != "0.166.0" {
		t.Fatalf("rollback: %v %s", err, n.pinned())
	}
	if st := n.auto.Check(ctx); st.Outcome != AutoSkippedRolled || n.pinned() != "0.166.0" {
		t.Fatalf("after rollback: %+v", st)
	}
}

func findRecT(t *testing.T, m *Manager, version string) (int, *versionRecord) {
	t.Helper()
	st, err := m.load()
	if err != nil {
		t.Fatal(err)
	}
	return findRec(st, "codex", version)
}

// Issue dedupe: a stage that fails without a record (download error) is
// retried every check but reported once; a report the hub did not take is
// kept and filed at the next check, once.
func TestAutoIssueDedupe(t *testing.T) {
	ctx := context.Background()
	src := &fakeRelease{}
	n := newAutoNode(t, src, "0.160.1")
	n.issues.failNext = 1
	src.set(Release{Version: "0.166.0", Source: filepath.Join(t.TempDir(), "missing.tar.gz"),
		Digest: contracts.Digest("sha256:" + strings.Repeat("ab", 32))})
	for i := range 4 {
		st := n.auto.Check(ctx)
		if st.Outcome != AutoStageFailed || n.pinned() != "0.160.1" {
			t.Fatalf("check %d: %+v", i, st)
		}
	}
	if n.issues.count() != 1 {
		t.Fatalf("issues: %v", n.issues.titles)
	}
	b, _ := os.ReadFile(n.auto.statePath())
	var st autoState
	if err := json.Unmarshal(b, &st); err != nil || len(st.Pending) != 0 || len(st.Reported) != 1 || st.Reported[0] != "0.166.0" {
		t.Fatalf("state: %s %v", b, err)
	}
	// A failed release check is not an issue (the network may be down).
	src.mu.Lock()
	src.err = errors.New("synthetic: no network")
	src.mu.Unlock()
	if st := n.auto.Check(ctx); st.Outcome != AutoCheckFailed || st.Latest != "0.166.0" || n.issues.count() != 1 {
		t.Fatalf("check failed: %+v", st)
	}
}

func TestAutoHandlers(t *testing.T) {
	ctx := context.Background()
	src := &fakeRelease{}
	src.set(codexPackage(t, t.TempDir(), "0.166.0", 0))
	n := newAutoNode(t, src, "0.160.1")
	h := AutoHandlers(n.auto)
	v, err := h[contracts.ActionUpdateAutoStatus](ctx, json.RawMessage(`{"tool":"codex"}`))
	if ss := v.([]contracts.AutoUpdateStatus); err != nil || len(ss) != 1 || ss[0].Current != "0.160.1" || ss[0].Latest != "" || !ss[0].Enabled {
		t.Fatalf("status: %+v %v", v, err)
	}
	if _, err := h[contracts.ActionUpdateAutoStatus](ctx, json.RawMessage(`{"tool":"claude"}`)); !contracts.IsCode(err, contracts.ErrUnsupported) {
		t.Fatalf("claude: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { n.auto.Run(runCtx); close(done) }()
	v, err = h[contracts.ActionUpdateAuto](ctx, nil)
	if ss := v.([]contracts.AutoUpdateStatus); err != nil || len(ss) != 1 || !ss[0].Running {
		t.Fatalf("trigger: %+v %v", v, err)
	}
	for n.auto.Status(ctx).Running {
		select {
		case <-done:
			t.Fatal("Run exited")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if st := n.auto.Status(ctx); st.Outcome != AutoUpdated || st.Current != "0.166.0" || st.NextCheckAt == nil {
		t.Fatalf("after trigger: %+v", st)
	}
	cancel()
	<-done
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
		ok   bool
	}{
		{"0.166.0", "0.165.9", 1, true}, {"0.99.0", "0.166.0", -1, true}, {"1.0.0", "1.0.0", 0, true},
		{"0.166.0", "0.166.0-alpha.1", 1, true}, {"0.166.0-alpha.2", "0.166.0-alpha.1", 1, true},
		{"v0.166.0", "0.166.0+linux", 0, true}, {"0.166.0", "", 0, false}, {"0.166", "0.165.0", 0, false},
	} {
		if got, ok := compareVersions(c.a, c.b); got != c.want || ok != c.ok {
			t.Errorf("compare(%q,%q) = %d,%v want %d,%v", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}

// GitHubCodex picks codex-package-<target>.tar.gz for the platform with
// GitHub's sha256 digest and refuses an asset without one.
func TestGitHubCodexPicksPlatformArtifact(t *testing.T) {
	d := "sha256:" + strings.Repeat("0f", 32)
	assets := []map[string]string{}
	for _, tg := range []string{"x86_64-unknown-linux-musl", "aarch64-apple-darwin", "x86_64-pc-windows-msvc", "aarch64-unknown-linux-musl"} {
		assets = append(assets,
			map[string]string{"name": "codex-package-" + tg + ".tar.gz", "browser_download_url": "https://example.invalid/dl/codex-package-" + tg + ".tar.gz", "digest": d},
			map[string]string{"name": "codex-package-" + tg + ".tar.zst", "browser_download_url": "https://example.invalid/dl/codex-package-" + tg + ".tar.zst", "digest": d},
			map[string]string{"name": "codex-" + tg + ".tar.gz", "browser_download_url": "https://example.invalid/dl/codex-" + tg + ".tar.gz", "digest": d})
	}
	assets = append(assets, map[string]string{"name": "codex-package-x86_64-apple-darwin.tar.gz", "browser_download_url": "https://example.invalid/dl/x"})
	body := map[string]any{"tag_name": "rust-v0.166.0", "assets": assets}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			http.Error(w, "accept", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	for plat, want := range map[string]string{
		"linux/amd64": "x86_64-unknown-linux-musl", "darwin/arm64": "aarch64-apple-darwin",
		"windows/amd64": "x86_64-pc-windows-msvc", "linux/arm64": "aarch64-unknown-linux-musl",
	} {
		goos, goarch, _ := strings.Cut(plat, "/")
		rel, err := GitHubCodex{URL: srv.URL, GOOS: goos, GOARCH: goarch}.Latest(context.Background())
		if err != nil || rel.Version != "0.166.0" || rel.Digest != contracts.Digest(d) || rel.Source != "https://example.invalid/dl/codex-package-"+want+".tar.gz" {
			t.Errorf("%s: %+v %v", plat, rel, err)
		}
	}
	if _, err := (GitHubCodex{URL: srv.URL, GOOS: "darwin", GOARCH: "amd64"}).Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "no sha256 digest") {
		t.Errorf("asset without digest: %v", err)
	}
	if _, err := (GitHubCodex{URL: srv.URL, GOOS: "freebsd", GOARCH: "amd64"}).Latest(context.Background()); !contracts.IsCode(err, contracts.ErrUnsupported) {
		t.Errorf("unsupported platform: %v", err)
	}
	body["prerelease"] = true
	if _, err := (GitHubCodex{URL: srv.URL, GOOS: "linux", GOARCH: "amd64"}).Latest(context.Background()); err == nil {
		t.Error("prerelease accepted")
	}
	body["prerelease"], body["tag_name"] = false, "rust-v0.167.0-alpha.1"
	if _, err := (GitHubCodex{URL: srv.URL, GOOS: "linux", GOARCH: "amd64"}).Latest(context.Background()); err == nil {
		t.Error("alpha tag accepted")
	}
}
