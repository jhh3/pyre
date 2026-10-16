package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/extensions"
	"github.com/jhh3/pyre/internal/osutil"
)

func randName() string { return contracts.NewID("stg") }

func (m *Manager) failErr(tv contracts.ToolVersion, format string, a ...any) error {
	return contracts.Errorf(contracts.ErrConflict, format, a...).WithDetail("tool_version", tv)
}

// Stage fetches, verifies, installs and health-checks a candidate version.
// It does not change what new runs use.
func (m *Manager) Stage(ctx context.Context, p contracts.UpdateParams) (contracts.ToolVersion, error) {
	if err := validTool(p.Tool); err != nil {
		return contracts.ToolVersion{}, err
	}
	if p.Version != "" && !versionRe.MatchString(p.Version) {
		return contracts.ToolVersion{}, contracts.Errorf(contracts.ErrInvalid, "invalid version %q", p.Version)
	}
	if p.Digest != "" && !validDigest(p.Digest) {
		return contracts.ToolVersion{}, contracts.Errorf(contracts.ErrInvalid, "digest must be sha256:<64 hex>")
	}
	if p.SimulateHealthFailure && !m.opt.TestMode {
		return contracts.ToolVersion{}, contracts.Errorf(contracts.ErrInvalid, "simulate_health_failure is honored only in test mode")
	}
	kind, loc, err := sourceKind(p.Source)
	if err != nil {
		return contracts.ToolVersion{}, err
	}
	_, pinned, _ := m.releaseSigners()
	signedRelease := isReleaseTool(p.Tool) && pinned && kind == "https" // digest comes from the signed SHA256SUMS
	if (kind == "https" || kind == "npm") && p.Digest == "" && !m.opt.AllowUnverified && !signedRelease {
		return contracts.ToolVersion{}, contracts.Errorf(contracts.ErrInvalid, "a sha256 digest is required for %s sources (provenance check)", kind)
	}
	if isPlugin(p.Tool) && p.Version == "" {
		return contracts.ToolVersion{}, contracts.Errorf(contracts.ErrInvalid, "plugin versions must be explicit")
	}
	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-ctx.Done():
		return contracts.ToolVersion{}, contracts.Errorf(contracts.ErrCapacityExhausted, "stage queue busy: %v", ctx.Err())
	}
	l := m.toolLock(p.Tool)
	l.Lock()
	defer l.Unlock()

	if p.Version != "" {
		m.mu.Lock()
		st, err := m.load()
		m.mu.Unlock()
		if err != nil {
			return contracts.ToolVersion{}, err
		}
		if _, r := findRec(st, p.Tool, p.Version); r != nil && r.State != StateFailed {
			if p.Digest != "" && r.Digest != p.Digest {
				return contracts.ToolVersion{}, contracts.Errorf(contracts.ErrConflict, "%s %s is already staged with digest %s", p.Tool, p.Version, r.Digest)
			}
			tv := r.ToolVersion
			tv.Path = m.exePath(*r)
			return tv, nil // idempotent
		}
	}

	staging := filepath.Join(m.toolDir(p.Tool), ".staging-"+randName())
	defer os.RemoveAll(staging)
	payload := filepath.Join(staging, "payload")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return contracts.ToolVersion{}, err
	}
	bin := p.Tool
	if b, ok := m.opt.BinNames[p.Tool]; ok {
		bin = b
	}
	if runtime.GOOS == "windows" && filepath.Ext(bin) == "" && kind != "npm" && !isPlugin(p.Tool) {
		bin += ".exe" // pyred.exe, pyre.exe in the Windows release zip
	}
	var digest contracts.Digest
	var exe string
	want := p.Digest
	signed, err := m.releaseDigest(ctx, p, kind, loc, staging)
	if err != nil {
		return contracts.ToolVersion{}, err
	}
	if signed != "" {
		want = signed // the digest listed in the verified, signed SHA256SUMS
	}
	verify := func(got contracts.Digest) error {
		digest = got
		if want != "" && got != want {
			return contracts.Errorf(contracts.ErrInvalid, "checksum mismatch for %s: got %s, want %s; candidate discarded", p.Source, got, want)
		}
		return nil
	}
	artifact := ""
	switch kind {
	case "file":
		info, err := os.Stat(loc)
		if err != nil {
			return contracts.ToolVersion{}, contracts.Errorf(contracts.ErrInvalid, "source: %v", err)
		}
		if info.IsDir() {
			d, err := extensions.DigestDir(loc)
			if err != nil {
				return contracts.ToolVersion{}, err
			}
			if err := verify(d); err != nil {
				return contracts.ToolVersion{}, err
			}
			if err := copyDir(loc, payload); err != nil {
				return contracts.ToolVersion{}, err
			}
		} else {
			d, err := sha256File(loc)
			if err != nil {
				return contracts.ToolVersion{}, err
			}
			if err := verify(d); err != nil {
				return contracts.ToolVersion{}, err
			}
			artifact = loc
		}
	case "https":
		name := filepath.Base(strings.SplitN(loc, "?", 2)[0])
		if name == "" || name == "/" || name == "." {
			name = "artifact"
		}
		artifact = filepath.Join(staging, "dl-"+name)
		d, err := m.download(ctx, loc, artifact)
		if err != nil {
			return contracts.ToolVersion{}, err
		}
		if err := verify(d); err != nil {
			return contracts.ToolVersion{}, err
		}
	case "npm":
		tarball, err := m.npmPack(ctx, loc, staging)
		if err != nil {
			return contracts.ToolVersion{}, err
		}
		d, err := sha256File(tarball)
		if err != nil {
			return contracts.ToolVersion{}, err
		}
		if err := verify(d); err != nil {
			return contracts.ToolVersion{}, err
		}
		if err := m.npmInstall(ctx, tarball, filepath.Join(payload, "prefix")); err != nil {
			return contracts.ToolVersion{}, err
		}
		exe = filepath.Join("prefix", "node_modules", ".bin", bin)
		if runtime.GOOS == "windows" {
			exe += ".cmd" // npm's Windows shim
		}
	}
	if artifact != "" {
		switch {
		case isArchive(artifact):
			if err := os.MkdirAll(payload, 0o755); err != nil {
				return contracts.ToolVersion{}, err
			}
			if err := extractArchive(artifact, payload, 4*m.opt.MaxDownloadBytes); err != nil {
				return contracts.ToolVersion{}, contracts.Errorf(contracts.ErrInvalid, "extract %s: %v", p.Source, err)
			}
		case isPlugin(p.Tool):
			return contracts.ToolVersion{}, contracts.Errorf(contracts.ErrInvalid, "plugin source must be a directory or a .tar.gz")
		default:
			if err := copyFile(artifact, filepath.Join(payload, "bin", bin), 0o755); err != nil {
				return contracts.ToolVersion{}, err
			}
		}
	}
	if isPlugin(p.Tool) {
		exe = "."
	} else if exe == "" {
		found, err := findExecutable(payload, bin)
		if err != nil {
			return contracts.ToolVersion{}, contracts.Errorf(contracts.ErrInvalid, "%s: %v", p.Source, err)
		}
		exe = found
	}

	detected, health, herr := m.health(ctx, p.Tool, filepath.Join(payload, exe), p.Version, p.SimulateHealthFailure)
	version := p.Version
	if version == "" {
		version = detected
	}
	if !versionRe.MatchString(version) {
		if herr == nil {
			herr = fmt.Errorf("cannot determine version from health output; pass version explicitly")
			health = "failed: " + herr.Error()
		}
		version = "unknown-" + strings.TrimPrefix(string(digest), "sha256:")[:12]
	}
	tv := contracts.ToolVersion{Tool: p.Tool, Version: version, Digest: digest, InstalledAt: m.opt.Clock.Now(), Health: health}
	rec := versionRecord{ToolVersion: tv, Source: p.Source, Exe: exe}
	if herr != nil {
		rec.State = StateFailed
		if err := m.mutate(func(st *stateFile) error {
			if i, _ := findRec(st, p.Tool, version); i >= 0 {
				st.Tools[p.Tool] = append(st.Tools[p.Tool][:i], st.Tools[p.Tool][i+1:]...)
			}
			st.Tools[p.Tool] = append(st.Tools[p.Tool], rec)
			return nil
		}); err != nil {
			return contracts.ToolVersion{}, err
		}
		m.report(ctx, contracts.EvUpdateResult, UpdateResult{Tool: p.Tool, Version: version, Outcome: "stage_failed", Health: health})
		return rec.ToolVersion, m.failErr(rec.ToolVersion, "%s %s failed its health check and was not staged: %s", p.Tool, version, health)
	}
	final := m.versionDir(p.Tool, version)
	m.mu.Lock()
	st, err := m.load()
	m.mu.Unlock()
	if err != nil {
		return contracts.ToolVersion{}, err
	}
	if _, r := findRec(st, p.Tool, version); r != nil && r.State != StateFailed {
		return contracts.ToolVersion{}, contracts.Errorf(contracts.ErrConflict, "%s %s already exists (digest %s)", p.Tool, version, r.Digest)
	}
	_ = os.RemoveAll(final) // leftover payload without a live record
	if err := os.Rename(payload, final); err != nil {
		return contracts.ToolVersion{}, err
	}
	rec.State = StateStaged
	if err := m.mutate(func(st *stateFile) error {
		if i, _ := findRec(st, p.Tool, version); i >= 0 {
			st.Tools[p.Tool] = append(st.Tools[p.Tool][:i], st.Tools[p.Tool][i+1:]...)
		}
		st.Tools[p.Tool] = append(st.Tools[p.Tool], rec)
		return nil
	}); err != nil {
		os.RemoveAll(final)
		return contracts.ToolVersion{}, err
	}
	tv = rec.ToolVersion
	tv.Path = m.exePath(rec)
	m.report(ctx, contracts.EvUpdateStaged, UpdateResult{Tool: p.Tool, Version: version, Outcome: "staged", Health: health})
	return tv, nil
}

// Activate makes a staged (or previous) version the default for new runs.
// Running runs are unaffected. A failed post-switch health check reverts to
// the previous active version automatically.
func (m *Manager) Activate(ctx context.Context, p contracts.UpdateParams) (contracts.ToolVersion, error) {
	if err := validTool(p.Tool); err != nil {
		return contracts.ToolVersion{}, err
	}
	if p.SimulateHealthFailure && !m.opt.TestMode {
		return contracts.ToolVersion{}, contracts.Errorf(contracts.ErrInvalid, "simulate_health_failure is honored only in test mode")
	}
	l := m.toolLock(p.Tool)
	l.Lock()
	defer l.Unlock()
	m.mu.Lock()
	st, err := m.load()
	m.mu.Unlock()
	if err != nil {
		return contracts.ToolVersion{}, err
	}
	var rec *versionRecord
	if p.Version == "" {
		var staged []versionRecord
		for _, r := range st.Tools[p.Tool] {
			if r.State == StateStaged {
				staged = append(staged, r)
			}
		}
		sort.SliceStable(staged, func(i, j int) bool { return staged[i].InstalledAt.After(staged[j].InstalledAt) })
		if len(staged) > 0 {
			rec = &staged[0]
		}
	} else {
		_, rec = findRec(st, p.Tool, p.Version)
	}
	if rec == nil {
		return contracts.ToolVersion{}, contracts.Errorf(contracts.ErrNotFound, "no staged %s %s; stage it first", p.Tool, p.Version)
	}
	target := *rec
	if target.State == StateActive {
		tv := target.ToolVersion
		tv.Path = m.exePath(target)
		return tv, nil
	}
	if target.State == StateFailed {
		return target.ToolVersion, contracts.Errorf(contracts.ErrConflict, "%s %s failed its health check (%s); stage it again", p.Tool, target.Version, target.Health)
	}
	if p.Digest != "" && target.Digest != p.Digest {
		return contracts.ToolVersion{}, contracts.Errorf(contracts.ErrConflict, "%s %s has digest %s, not %s", p.Tool, target.Version, target.Digest, p.Digest)
	}
	if p.Tool == "pyred" && m.opt.HubProtocol != nil {
		if hub, ok := m.opt.HubProtocol(); ok {
			rng, ver, err := m.opt.ProtocolProbe(ctx, m.exePath(target))
			if err != nil {
				return target.ToolVersion, contracts.Errorf(contracts.ErrConflict, "cannot read protocol range of pyred %s: %v; not activated", target.Version, err)
			}
			if ver == "" {
				ver = target.Version
			}
			if err := CheckProtocolCompat(ver, rng, hub); err != nil {
				m.report(ctx, contracts.EvUpdateResult, UpdateResult{Tool: p.Tool, Version: target.Version, Outcome: "refused", Detail: err.Error()})
				return target.ToolVersion, err
			}
			target.Protocol = &rng
		}
	}
	prev := activeRec(st, p.Tool)
	var prevVersion string
	if prev != nil {
		prevVersion = prev.Version
	}
	if err := m.switchCurrent(p.Tool, target.Version); err != nil {
		return contracts.ToolVersion{}, err
	}
	_, health, herr := m.health(ctx, p.Tool, filepath.Join(m.currentLink(p.Tool), target.Exe), target.Version, p.SimulateHealthFailure)
	if herr != nil {
		revertErr := m.revertCurrent(p.Tool, prevVersion)
		var failed contracts.ToolVersion
		_ = m.mutate(func(st *stateFile) error {
			if _, r := findRec(st, p.Tool, target.Version); r != nil {
				r.State, r.Health = StateFailed, health
				failed = r.ToolVersion
			}
			return nil
		})
		_ = os.RemoveAll(m.versionDir(p.Tool, target.Version)) // never active for any run
		detail := "reverted to " + orNone(prevVersion)
		if revertErr != nil {
			detail = "REVERT FAILED: " + revertErr.Error()
		}
		m.report(ctx, contracts.EvUpdateResult, UpdateResult{Tool: p.Tool, Version: target.Version, Outcome: "failed_reverted", Health: health, RevertedTo: prevVersion, Detail: detail})
		return failed, m.failErr(failed, "%s %s failed its post-activation health check (%s); %s. Running sessions were not affected.", p.Tool, target.Version, health, detail)
	}
	now := m.opt.Clock.Now()
	var out contracts.ToolVersion
	err = m.mutate(func(st *stateFile) error {
		for i := range st.Tools[p.Tool] {
			r := &st.Tools[p.Tool][i]
			switch {
			case r.Version == target.Version:
				r.State, r.Health, r.ActivatedAt, r.Protocol = StateActive, health, &now, target.Protocol
				if p.Tool == "pyred" {
					r.Base = m.opt.DaemonBase
				}
				out = r.ToolVersion
				out.Path = m.exePath(*r)
			case r.State == StateActive:
				r.State = StatePrevious
			}
		}
		return nil
	})
	if err != nil {
		return contracts.ToolVersion{}, err
	}
	detail := "previous: " + orNone(prevVersion) + "; running sessions keep their pinned executable"
	if p.Tool == "pyred" {
		detail = "previous: " + orLauncher(prevVersion) + "; the daemon restarts on " + target.Version + " through the launcher (runs are recovered, not restarted); a version that does not become healthy is rolled back automatically"
	}
	m.report(ctx, contracts.EvUpdateResult, UpdateResult{Tool: p.Tool, Version: target.Version, Outcome: "activated", Health: health, Detail: detail})
	if p.Tool == "pyred" {
		m.daemonSwitched(target.Version, prevVersion)
	}
	return out, nil
}

// daemonSwitched records the pyred handoff and asks the daemon to restart
// through its launcher.
func (m *Manager) daemonSwitched(version, previous string) {
	if m.opt.DaemonBase == "" {
		return
	}
	if version == "" {
		m.clearHandoff()
	} else {
		_ = m.writeHandoff(Handoff{Version: version, Base: m.opt.DaemonBase, Previous: previous, State: HandoffPending})
	}
	if m.opt.OnDaemonSwitch != nil {
		m.opt.OnDaemonSwitch(version)
	}
}

// Rollback re-activates a previous healthy version (the newest one, or
// p.Version).
func (m *Manager) Rollback(ctx context.Context, p contracts.UpdateParams) (contracts.ToolVersion, error) {
	if err := validTool(p.Tool); err != nil {
		return contracts.ToolVersion{}, err
	}
	l := m.toolLock(p.Tool)
	l.Lock()
	defer l.Unlock()
	m.mu.Lock()
	st, err := m.load()
	m.mu.Unlock()
	if err != nil {
		return contracts.ToolVersion{}, err
	}
	var cands []versionRecord
	for _, r := range st.Tools[p.Tool] {
		if r.State == StatePrevious && (p.Version == "" || r.Version == p.Version) {
			cands = append(cands, r)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return activatedAt(cands[i]).After(activatedAt(cands[j])) })
	if len(cands) == 0 && p.Tool == "pyred" && p.Version == "" && m.opt.DaemonBase != "" {
		if act := activeRec(st, "pyred"); act != nil {
			return m.returnToLauncher(ctx, act.Version)
		}
	}
	if len(cands) == 0 {
		return contracts.ToolVersion{}, contracts.Errorf(contracts.ErrNotFound, "no previous %s version to roll back to", p.Tool)
	}
	target := cands[0]
	if _, health, err := m.health(ctx, p.Tool, m.exePath(target), target.Version, false); err != nil {
		return target.ToolVersion, m.failErr(target.ToolVersion, "rollback target %s %s is unhealthy (%s); nothing changed", p.Tool, target.Version, health)
	}
	prev := activeRec(st, p.Tool)
	if err := m.switchCurrent(p.Tool, target.Version); err != nil {
		return contracts.ToolVersion{}, err
	}
	now := m.opt.Clock.Now()
	var out contracts.ToolVersion
	err = m.mutate(func(st *stateFile) error {
		for i := range st.Tools[p.Tool] {
			r := &st.Tools[p.Tool][i]
			switch {
			case r.Version == target.Version:
				r.State, r.ActivatedAt = StateActive, &now
				if p.Tool == "pyred" {
					r.Base = m.opt.DaemonBase
				}
				out = r.ToolVersion
				out.Path = m.exePath(*r)
			case r.State == StateActive:
				r.State = StatePrevious
			}
		}
		return nil
	})
	if err != nil {
		return contracts.ToolVersion{}, err
	}
	from := ""
	if prev != nil {
		from = prev.Version
	}
	m.report(ctx, contracts.EvUpdateResult, UpdateResult{Tool: p.Tool, Version: target.Version, Outcome: "rolled_back", RevertedTo: target.Version, Detail: "from " + orNone(from)})
	if p.Tool == "pyred" {
		m.daemonSwitched(target.Version, from)
	}
	return out, nil
}

// returnToLauncher deactivates the managed pyred: the installed pyred (the
// launcher) runs again after the daemon restarts.
func (m *Manager) returnToLauncher(ctx context.Context, from string) (contracts.ToolVersion, error) {
	if err := m.mutate(func(st *stateFile) error {
		if _, r := findRec(st, "pyred", from); r != nil && r.State == StateActive {
			r.State = StatePrevious
		}
		return nil
	}); err != nil {
		return contracts.ToolVersion{}, err
	}
	if err := m.revertCurrent("pyred", ""); err != nil {
		return contracts.ToolVersion{}, err
	}
	out := contracts.ToolVersion{Tool: "pyred", Version: m.opt.DaemonBase, State: StateActive, Health: "unchecked", Path: m.opt.External["pyred"]}
	m.report(ctx, contracts.EvUpdateResult, UpdateResult{Tool: "pyred", Version: m.opt.DaemonBase, Outcome: "rolled_back", RevertedTo: m.opt.DaemonBase,
		Detail: "from " + from + " to the installed pyred (launcher); the daemon restarts on it"})
	m.daemonSwitched("", from)
	return out, nil
}

func activatedAt(r versionRecord) time.Time {
	if r.ActivatedAt != nil {
		return *r.ActivatedAt
	}
	return r.InstalledAt
}

func orNone(s string) string {
	if s == "" {
		return "none (external/unmanaged default)"
	}
	return s
}

// switchCurrent atomically points current at version.
func (m *Manager) switchCurrent(tool, version string) error {
	link := m.currentLink(tool)
	tmp := link + ".tmp-" + randName()
	if runtime.GOOS == "windows" {
		// A junction (no admin/Developer Mode) to the absolute version dir.
		// Windows cannot rename a directory link over an existing one, so
		// the old link goes first (a brief gap; readers retry).
		if err := osutil.Junction(filepath.Join(m.toolDir(tool), version), tmp); err != nil {
			return err
		}
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			os.Remove(tmp)
			return err
		}
		if err := os.Rename(tmp, link); err != nil {
			os.Remove(tmp)
			return err
		}
		return nil
	}
	if err := os.Symlink(version, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (m *Manager) revertCurrent(tool, version string) error {
	if version == "" {
		err := os.Remove(m.currentLink(tool))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return m.switchCurrent(tool, version)
}

var semverRe = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?`)

// health runs the version probe (+ smoke) for a candidate executable.
func (m *Manager) health(ctx context.Context, tool, exe, want string, simulate bool) (version, health string, err error) {
	fail := func(f string, a ...any) (string, string, error) {
		msg := fmt.Sprintf(f, a...)
		return version, "failed: " + msg, fmt.Errorf("%s", msg)
	}
	if isPlugin(tool) {
		ok := false
		for _, mf := range []string{".claude-plugin/plugin.json", ".codex-plugin/plugin.json"} {
			if b, err := os.ReadFile(filepath.Join(exe, mf)); err == nil && json.Valid(b) {
				ok = true
			}
		}
		version = want
		if !ok {
			return fail("plugin has no valid .claude-plugin/plugin.json or .codex-plugin/plugin.json")
		}
	} else {
		v, err := m.probeVersion(ctx, tool, exe)
		version = v
		if err != nil {
			return fail("%v", err)
		}
		if want != "" && version != "" && version != want && !strings.HasPrefix(version, want+"+") {
			return fail("reports version %s, expected %s", version, want)
		}
		if args, ok := m.opt.Smoke[tool]; ok {
			hctx, cancel := context.WithTimeout(ctx, m.opt.HealthTimeout)
			_, stderr, err := m.opt.Runner.Run(hctx, "", healthEnv, exe, args...)
			cancel()
			if err != nil {
				return fail("smoke %s %s: %v %s", filepath.Base(exe), strings.Join(args, " "), err, trim(stderr))
			}
		}
	}
	if simulate {
		return fail("simulated health-check failure (test mode)")
	}
	return version, "ok", nil
}

// healthEnv keeps health probes from self-updating or prompting.
var healthEnv = []string{"DISABLE_AUTOUPDATER=1", "NO_COLOR=1", "CI=1"}

func (m *Manager) probeVersion(ctx context.Context, tool, exe string) (string, error) {
	args, ok := m.opt.HealthArgs[tool]
	if !ok {
		args = []string{"--version"}
		if tool == "pyred" || tool == "pyre" {
			args = []string{"version", "--json"}
		}
	}
	hctx, cancel := context.WithTimeout(ctx, m.opt.HealthTimeout)
	defer cancel()
	stdout, stderr, err := m.opt.Runner.Run(hctx, "", healthEnv, exe, args...)
	if err != nil {
		return "", fmt.Errorf("%s %s: %v %s", filepath.Base(exe), strings.Join(args, " "), err, trim(stderr))
	}
	var j struct {
		Version string `json:"version"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(stdout)), &j) == nil && j.Version != "" {
		return j.Version, nil
	}
	return semverRe.FindString(stdout), nil
}

func trim(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

func (m *Manager) npmPack(ctx context.Context, spec, staging string) (string, error) {
	dl := filepath.Join(staging, "dl")
	if err := os.MkdirAll(dl, 0o700); err != nil {
		return "", err
	}
	stdout, stderr, err := m.opt.Runner.Run(ctx, dl, npmEnv(staging), m.opt.NPM, "pack", spec, "--pack-destination", dl, "--silent")
	if err != nil {
		return "", contracts.Errorf(contracts.ErrUnavailable, "npm pack %s: %v %s", spec, err, trim(stderr))
	}
	lines := strings.Fields(strings.TrimSpace(stdout))
	if len(lines) == 0 {
		return "", contracts.Errorf(contracts.ErrUnavailable, "npm pack %s produced no tarball", spec)
	}
	name := filepath.Base(lines[len(lines)-1])
	p := filepath.Join(dl, name)
	if _, err := os.Stat(p); err != nil {
		return "", contracts.Errorf(contracts.ErrUnavailable, "npm pack %s: tarball %s missing", spec, name)
	}
	return p, nil
}

func (m *Manager) npmInstall(ctx context.Context, tarball, prefix string) error {
	if err := os.MkdirAll(prefix, 0o755); err != nil {
		return err
	}
	_, stderr, err := m.opt.Runner.Run(ctx, prefix, npmEnv(filepath.Dir(prefix)), m.opt.NPM, "install", "--prefix", prefix, "--no-audit", "--no-fund", "--no-save", "--global=false", tarball)
	if err != nil {
		return contracts.Errorf(contracts.ErrUnavailable, "npm install into isolated prefix: %v %s", err, trim(stderr))
	}
	return nil
}

// npmEnv isolates npm from the user's global prefix and cache settings.
func npmEnv(dir string) []string {
	return []string{"npm_config_global=false", "npm_config_update_notifier=false", "npm_config_fund=false", "npm_config_cache=" + filepath.Join(dir, ".npm-cache")}
}
