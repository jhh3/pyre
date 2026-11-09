package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Automatic updates (codex).
//
// Every node's pyred runs an Auto for codex: soon after it starts and then
// daily it reads the newest stable release (ReleaseSource), and when that is
// newer than the version new runs use (the active managed version, else the
// user's own install on PATH) it stages the platform artifact with its
// release digest, health-checks it and activates it through Stage/Activate.
// Running sessions keep the executable they were launched with. It never
// downgrades, never retries a version that failed its health check or that
// the owner rolled back from (the next release is tried), and files one
// machine issue per failed version (FileIssue; retried until the hub takes
// it). The state lives in <state>/tools/<tool>/auto.json.

// Auto-update outcomes (contracts.AutoUpdateStatus.Outcome).
const (
	AutoUpToDate       = "up_to_date"
	AutoUpdated        = "updated"
	AutoCheckFailed    = "check_failed"
	AutoStageFailed    = "stage_failed"
	AutoActivateFailed = "activate_failed"
	AutoSkippedFailed  = "skipped_failed"
	AutoSkippedRolled  = "skipped_rolled_back"
)

// Release is the newest stable release of a tool for this node's platform.
type Release struct {
	Version string
	Source  string           // https URL of the platform artifact
	Digest  contracts.Digest // sha256 of that artifact, published by the release source
}

// ReleaseSource finds the newest stable release.
type ReleaseSource interface {
	Latest(ctx context.Context) (Release, error)
}

// CodexReleasesURL is openai/codex's latest (non-draft, non-prerelease)
// GitHub release.
const CodexReleasesURL = "https://api.github.com/repos/openai/codex/releases/latest"

// GitHubCodex reads the latest openai/codex GitHub release and picks the
// codex-package-<target>.tar.gz for this platform: the layout of Codex's
// own standalone install (bin/codex plus its helpers: codex-exec-server,
// codex-path/rg, codex-resources/...). GitHub publishes a sha256 digest per
// asset, which Stage verifies.
type GitHubCodex struct {
	HTTP         HTTPDoer // nil = a 1 minute client
	URL          string   // default CodexReleasesURL
	GOOS, GOARCH string   // default runtime.GOOS/GOARCH
}

// CodexTarget is the Rust target triple of codex release artifacts for a
// platform.
func CodexTarget(goos, goarch string) (string, error) {
	t := map[string]string{
		"linux/amd64":   "x86_64-unknown-linux-musl",
		"linux/arm64":   "aarch64-unknown-linux-musl",
		"darwin/arm64":  "aarch64-apple-darwin",
		"darwin/amd64":  "x86_64-apple-darwin",
		"windows/amd64": "x86_64-pc-windows-msvc",
		"windows/arm64": "aarch64-pc-windows-msvc",
	}[goos+"/"+goarch]
	if t == "" {
		return "", contracts.Errorf(contracts.ErrUnsupported, "no codex release artifact for %s/%s", goos, goarch)
	}
	return t, nil
}

var releaseTagRe = regexp.MustCompile(`^(?:rust-)?v?(\d+\.\d+\.\d+)$`)

func (g GitHubCodex) Latest(ctx context.Context) (Release, error) {
	goos, goarch := g.GOOS, g.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	target, err := CodexTarget(goos, goarch)
	if err != nil {
		return Release{}, err
	}
	u := g.URL
	if u == "" {
		u = CodexReleasesURL
	}
	hc := g.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "pyred-updates")
	resp, err := hc.Do(req)
	if err != nil {
		return Release{}, contracts.Errorf(contracts.ErrUnavailable, "codex releases: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, contracts.Errorf(contracts.ErrUnavailable, "codex releases: HTTP %d from %s", resp.StatusCode, u)
	}
	var rel struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&rel); err != nil {
		return Release{}, contracts.Errorf(contracts.ErrUnavailable, "codex releases: %v", err)
	}
	if rel.Draft || rel.Prerelease {
		return Release{}, contracts.Errorf(contracts.ErrUnavailable, "codex release %s is a draft or prerelease", rel.Tag)
	}
	m := releaseTagRe.FindStringSubmatch(rel.Tag)
	if m == nil {
		return Release{}, contracts.Errorf(contracts.ErrUnavailable, "codex release tag %q is not a stable version", rel.Tag)
	}
	name := "codex-package-" + target + ".tar.gz"
	for _, a := range rel.Assets {
		if a.Name != name {
			continue
		}
		d := contracts.Digest(a.Digest)
		if !validDigest(d) {
			return Release{}, contracts.Errorf(contracts.ErrUnavailable, "codex %s: %s has no sha256 digest; not installing an unverified artifact", m[1], name)
		}
		if !strings.HasPrefix(a.URL, "https://") {
			return Release{}, contracts.Errorf(contracts.ErrUnavailable, "codex %s: %s is not an https download", m[1], name)
		}
		return Release{Version: m[1], Source: a.URL, Digest: d}, nil
	}
	return Release{}, contracts.Errorf(contracts.ErrUnavailable, "codex %s has no %s", m[1], name)
}

// compareVersions compares x.y.z[-pre] versions; ok=false when either does
// not parse.
func compareVersions(a, b string) (int, bool) {
	pa, prea, ok1 := parseVersion(a)
	pb, preb, ok2 := parseVersion(b)
	if !ok1 || !ok2 {
		return 0, false
	}
	if c := slices.Compare(pa, pb); c != 0 {
		return c, true
	}
	switch { // a release sorts after its prereleases
	case prea == preb:
		return 0, true
	case prea == "":
		return 1, true
	case preb == "":
		return -1, true
	}
	return strings.Compare(prea, preb), true
}

func parseVersion(v string) ([]int, string, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	v, _, _ = strings.Cut(v, "+")
	core, pre, _ := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return nil, "", false
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, "", false
		}
		out[i] = n
	}
	return out, pre, true
}

// AutoOptions configures an Auto.
type AutoOptions struct {
	Tool       string // default "codex"
	Source     ReleaseSource
	Interval   time.Duration // default 24h
	FirstDelay time.Duration // first check after Run starts (default 2m)
	// FileIssue files a machine issue about this node; nil = not filed.
	FileIssue func(ctx context.Context, title, body string) error
	Logf      func(format string, a ...any)
}

// Auto is one tool's automatic updater on a node.
type Auto struct {
	m       *Manager
	o       AutoOptions
	trigger chan struct{}

	mu      sync.Mutex
	running bool
	queued  bool
	next    time.Time
}

type autoIssue struct {
	Version string `json:"version"`
	Title   string `json:"title"`
	Body    string `json:"body"`
}

type autoState struct {
	Latest       string           `json:"latest,omitempty"`
	LatestSource string           `json:"latest_source,omitempty"`
	LatestDigest contracts.Digest `json:"latest_digest,omitempty"`
	CheckedAt    *time.Time       `json:"checked_at,omitempty"`
	Outcome      string           `json:"outcome,omitempty"`
	Detail       string           `json:"detail,omitempty"`
	// Reported lists versions whose failure was filed as an issue;
	// Pending holds reports the hub has not taken yet.
	Reported []string    `json:"reported,omitempty"`
	Pending  []autoIssue `json:"pending,omitempty"`
}

// NewAuto creates the automatic updater for o.Tool.
func (m *Manager) NewAuto(o AutoOptions) *Auto {
	if o.Tool == "" {
		o.Tool = "codex"
	}
	if o.Interval <= 0 {
		o.Interval = 24 * time.Hour
	}
	if o.FirstDelay <= 0 {
		o.FirstDelay = 2 * time.Minute
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	return &Auto{m: m, o: o, trigger: make(chan struct{}, 1)}
}

func (a *Auto) statePath() string { return filepath.Join(a.m.toolDir(a.o.Tool), "auto.json") }

func (a *Auto) load() autoState {
	var st autoState
	if b, err := os.ReadFile(a.statePath()); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func (a *Auto) save(st autoState) {
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := writeFileAtomic(a.statePath(), append(b, '\n'), 0o600); err != nil {
		a.o.Logf("auto-update %s: saving state: %v", a.o.Tool, err)
	}
}

// Run checks FirstDelay after it starts, then every Interval, and whenever
// Trigger is called, until ctx ends.
func (a *Auto) Run(ctx context.Context) {
	a.setNext(a.m.opt.Clock.Now().Add(a.o.FirstDelay))
	t := time.NewTimer(a.o.FirstDelay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.trigger:
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
		}
		a.setNext(a.m.opt.Clock.Now().Add(a.o.Interval))
		a.Check(ctx)
		t.Reset(a.o.Interval)
	}
}

func (a *Auto) setNext(t time.Time) {
	a.mu.Lock()
	a.next = t
	a.mu.Unlock()
}

// Trigger asks Run for a check now (no-op while one is queued).
func (a *Auto) Trigger() {
	a.mu.Lock()
	a.queued = true
	a.mu.Unlock()
	select {
	case a.trigger <- struct{}{}:
	default:
	}
}

// current is the version new runs launch.
func (a *Auto) current(ctx context.Context) string {
	if pin, ok := a.m.Pinned(ctx, a.o.Tool); ok {
		return pin.Version
	}
	return ""
}

// Status reports the updater's state.
func (a *Auto) Status(ctx context.Context) contracts.AutoUpdateStatus {
	st := a.load()
	a.mu.Lock()
	running, next := a.running || a.queued, a.next
	a.mu.Unlock()
	out := contracts.AutoUpdateStatus{Tool: a.o.Tool, Enabled: true, Current: a.current(ctx), Latest: st.Latest,
		LatestSource: st.LatestSource, CheckedAt: st.CheckedAt, Running: running, Outcome: st.Outcome, Detail: st.Detail}
	if !next.IsZero() {
		out.NextCheckAt = &next
	}
	return out
}

// Check finds the newest release and installs it when it is newer than what
// new runs use. It returns the resulting status; a check already in progress
// is not repeated.
func (a *Auto) Check(ctx context.Context) contracts.AutoUpdateStatus {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return a.Status(ctx)
	}
	a.running, a.queued = true, false
	a.mu.Unlock()
	func() {
		defer func() {
			a.mu.Lock()
			a.running = false
			a.mu.Unlock()
		}()
		st := a.load()
		a.check(ctx, &st)
		a.flushIssues(ctx, &st)
		a.save(st)
		if st.Outcome != AutoUpToDate {
			a.o.Logf("auto-update %s: %s: %s", a.o.Tool, st.Outcome, st.Detail)
		}
	}()
	return a.Status(ctx)
}

func (a *Auto) check(ctx context.Context, st *autoState) {
	tool := a.o.Tool
	now := a.m.opt.Clock.Now()
	st.CheckedAt = &now
	if a.o.Source == nil {
		st.Outcome, st.Detail = AutoCheckFailed, "no release source configured"
		return
	}
	rel, err := a.o.Source.Latest(ctx)
	if err != nil {
		st.Outcome, st.Detail = AutoCheckFailed, err.Error()
		return
	}
	st.Latest, st.LatestSource, st.LatestDigest = rel.Version, rel.Source, rel.Digest
	cur := a.current(ctx)
	if c, ok := compareVersions(rel.Version, cur); ok && c <= 0 {
		st.Outcome, st.Detail = AutoUpToDate, fmt.Sprintf("new runs use %s %s; latest release %s", tool, cur, rel.Version)
		return
	}
	a.m.mu.Lock()
	file, err := a.m.load()
	a.m.mu.Unlock()
	if err != nil {
		st.Outcome, st.Detail = AutoCheckFailed, "tool state: "+err.Error()
		return
	}
	if _, r := findRec(file, tool, rel.Version); r != nil {
		switch r.State {
		case StateFailed:
			st.Outcome, st.Detail = AutoSkippedFailed, fmt.Sprintf("%s %s failed before (%s); %s stays active; the next release is tried", tool, rel.Version, r.Health, orNone(cur))
			return
		case StatePrevious:
			st.Outcome, st.Detail = AutoSkippedRolled, fmt.Sprintf("%s %s was rolled back from; %s stays active; the next release is tried", tool, rel.Version, orNone(cur))
			return
		}
	}
	p := contracts.UpdateParams{Tool: tool, Version: rel.Version, Source: rel.Source, Digest: rel.Digest}
	if _, err := a.m.Stage(ctx, p); err != nil {
		st.Outcome, st.Detail = AutoStageFailed, err.Error()
		a.queueIssue(st, rel, cur, "could not be staged", err)
		return
	}
	if _, err := a.m.Activate(ctx, contracts.UpdateParams{Tool: tool, Version: rel.Version, Digest: rel.Digest}); err != nil {
		st.Outcome, st.Detail = AutoActivateFailed, err.Error()
		a.queueIssue(st, rel, cur, "failed activation", err)
		return
	}
	st.Outcome, st.Detail = AutoUpdated, fmt.Sprintf("%s %s is active for new runs (was %s); running sessions keep theirs", tool, rel.Version, orNone(cur))
	if removed, err := a.m.PruneTool(ctx, tool); err != nil {
		a.o.Logf("auto-update %s: prune: %v", tool, err)
	} else if len(removed) > 0 {
		a.o.Logf("auto-update %s: pruned %d old version(s)", tool, len(removed))
	}
}

// queueIssue records one issue per failed version.
func (a *Auto) queueIssue(st *autoState, rel Release, cur, what string, err error) {
	if slices.Contains(st.Reported, rel.Version) || slices.ContainsFunc(st.Pending, func(i autoIssue) bool { return i.Version == rel.Version }) {
		return
	}
	title := fmt.Sprintf("%s auto-update: %s %s %s", a.o.Tool, a.o.Tool, rel.Version, what)
	body := fmt.Sprintf("Pyre's automatic %s updater on this machine could not install %s %s.\n\n"+
		"- Error: %s\n- Artifact: %s (%s)\n- Still active for new runs: %s (unchanged; running sessions were not touched)\n\n"+
		"This version is not retried automatically; the next release is. To retry it by hand: "+
		"`pyre update stage --node <this node> --tool %s --version %s --source %s --digest %s`, then `pyre update activate`.",
		a.o.Tool, a.o.Tool, rel.Version, trim(err.Error()), rel.Source, rel.Digest, orNone(cur), a.o.Tool, rel.Version, rel.Source, rel.Digest)
	st.Pending = append(st.Pending, autoIssue{Version: rel.Version, Title: title, Body: body})
}

func (a *Auto) flushIssues(ctx context.Context, st *autoState) {
	if a.o.FileIssue == nil || len(st.Pending) == 0 {
		return
	}
	var keep []autoIssue
	for _, is := range st.Pending {
		if err := a.o.FileIssue(ctx, is.Title, is.Body); err != nil {
			a.o.Logf("auto-update %s: filing issue for %s: %v (retried at the next check)", a.o.Tool, is.Version, err)
			keep = append(keep, is)
			continue
		}
		st.Reported = append(st.Reported, is.Version)
	}
	st.Pending = keep
	if n := len(st.Reported); n > 50 {
		st.Reported = st.Reported[n-50:]
	}
}

// AutoHandlers returns the update.auto / update.auto.status node actions
// over the given updaters.
func AutoHandlers(autos ...*Auto) map[string]Handler {
	pick := func(raw json.RawMessage) ([]*Auto, error) {
		var p contracts.AutoUpdateParams
		if s := strings.TrimSpace(string(raw)); s != "" && s != "null" {
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, contracts.Errorf(contracts.ErrInvalid, "invalid params: %v", err)
			}
		}
		var out []*Auto
		for _, a := range autos {
			if p.Tool == "" || p.Tool == a.o.Tool {
				out = append(out, a)
			}
		}
		if len(out) == 0 {
			return nil, contracts.Errorf(contracts.ErrUnsupported, "%s is not updated automatically (automatic: codex)", p.Tool)
		}
		return out, nil
	}
	return map[string]Handler{
		contracts.ActionUpdateAuto: func(ctx context.Context, raw json.RawMessage) (any, error) {
			sel, err := pick(raw)
			if err != nil {
				return nil, err
			}
			out := []contracts.AutoUpdateStatus{}
			for _, a := range sel {
				a.Trigger()
				out = append(out, a.Status(ctx))
			}
			return out, nil
		},
		contracts.ActionUpdateAutoStatus: func(ctx context.Context, raw json.RawMessage) (any, error) {
			sel, err := pick(raw)
			if err != nil {
				return nil, err
			}
			out := []contracts.AutoUpdateStatus{}
			for _, a := range sel {
				out = append(out, a.Status(ctx))
			}
			return out, nil
		},
	}
}
