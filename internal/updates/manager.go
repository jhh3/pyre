package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/configuration"
	"github.com/jhh3/pyre/internal/contracts"
)

// Tool version states (contracts.ToolVersion.State).
const (
	StateActive   = "active"
	StateStaged   = "staged"
	StatePrevious = "previous"
	StateFailed   = "failed"
)

// Options configures a Manager.
type Options struct {
	StateDir string // tools live in <StateDir>/tools
	NodeID   string
	Clock    contracts.Clock
	Runs     configuration.RunRegistry
	Reporter configuration.Reporter
	Runner   Runner
	// HTTP fetches https sources. nil = a client with a 10 minute timeout.
	HTTP HTTPDoer
	// NPM is the npm executable used for "npm:" sources (default "npm").
	NPM string
	// BinNames maps tool -> executable name inside a package (default: tool).
	BinNames map[string]string
	// HealthArgs maps tool -> arguments of the version probe
	// (default ["--version"]; pyred: ["version", "--json"]).
	HealthArgs map[string][]string
	// Smoke maps tool -> extra arguments run as a smoke test after the probe.
	Smoke map[string][]string
	// MaxConcurrent bounds concurrent stage operations (default 1).
	MaxConcurrent int
	// MaxDownloadBytes bounds a fetched artifact (default 1 GiB).
	MaxDownloadBytes int64
	// HealthTimeout bounds each health command (default 60s).
	HealthTimeout time.Duration
	// Keep is the number of healthy previous versions retained per tool (>= 1).
	Keep int
	// AllowUnverified permits network sources without a digest.
	AllowUnverified bool
	// TestMode honors UpdateParams.SimulateHealthFailure.
	TestMode bool
	// HubProtocol returns the hub's supported protocol range, used when
	// activating pyred. ok=false skips the check (not connected yet).
	HubProtocol func() (ProtocolRange, bool)
	// ProtocolProbe reads a pyred candidate's protocol range (default runs
	// `<exe> version --json`).
	ProtocolProbe func(ctx context.Context, exe string) (ProtocolRange, string, error)
	// ReleaseSigners is the OpenSSH allowed-signers file that pins the Pyre
	// release key (default DefaultReleaseSigners():
	// $PYRE_RELEASE_ALLOWED_SIGNERS or ~/.config/pyre/release-allowed-signers,
	// shared with scripts/install/upgrade.sh). While it exists, staging pyred
	// or pyre requires a release tarball whose SHA256SUMS carries a valid
	// SHA256SUMS.sig from that key, and the tarball must match the signed
	// digest. Absent: legacy behaviour (digest as supplied).
	ReleaseSigners string
	// AllowUnsignedRelease permits pyred/pyre sources without a signature
	// although a key is pinned (self-built development binaries). A present
	// but invalid signature is refused regardless.
	AllowUnsignedRelease bool
	// External maps tool -> an executable outside Pyre's store (e.g. the
	// user's own install found on PATH). It is the default for new runs
	// while no managed version is active, and is never modified.
	External map[string]string
	// DaemonBase is the version of the installed pyred that the service
	// manager starts (the launcher). Activating or rolling back pyred records
	// it, so the managed pyred is handed off to only by that launcher
	// (see daemon.go). "" disables daemon handoff bookkeeping.
	DaemonBase string
	// OnDaemonSwitch is called after pyred was activated, rolled back or
	// returned to the launcher; the daemon restarts itself through the
	// launcher, which hands off to the active managed version.
	OnDaemonSwitch func(version string)
}

// versionRecord is the persisted form of a tool version.
type versionRecord struct {
	contracts.ToolVersion
	Source      string         `json:"source,omitempty"`
	Exe         string         `json:"exe"` // relative to the version dir ("." for plugin dirs)
	Protocol    *ProtocolRange `json:"protocol,omitempty"`
	ActivatedAt *time.Time     `json:"activated_at,omitempty"`
	// Base (pyred only) is the launcher version this version was activated
	// over; only that launcher hands off to it.
	Base string `json:"base,omitempty"`
}

type stateFile struct {
	Tools map[string][]versionRecord `json:"tools"`
}

// Manager manages tool versions on one node.
type Manager struct {
	opt   Options
	sem   chan struct{}
	mu    sync.Mutex // guards state file
	locks sync.Map   // tool -> *sync.Mutex

	extMu       sync.Mutex
	extVersions map[string]string
}

// New creates a Manager.
func New(opt Options) (*Manager, error) {
	if opt.StateDir == "" {
		return nil, fmt.Errorf("updates: StateDir required")
	}
	if opt.Clock == nil {
		opt.Clock = contracts.SystemClock{}
	}
	if opt.Runner == nil {
		opt.Runner = ExecRunner{}
	}
	if opt.HTTP == nil {
		opt.HTTP = defaultHTTP()
	}
	if opt.NPM == "" {
		opt.NPM = "npm"
	}
	if opt.MaxConcurrent <= 0 {
		opt.MaxConcurrent = 1
	}
	if opt.MaxDownloadBytes <= 0 {
		opt.MaxDownloadBytes = 1 << 30
	}
	if opt.HealthTimeout <= 0 {
		opt.HealthTimeout = 60 * time.Second
	}
	if opt.Keep < 1 {
		opt.Keep = 1
	}
	if opt.ReleaseSigners == "" {
		opt.ReleaseSigners = DefaultReleaseSigners()
	}
	m := &Manager{opt: opt, sem: make(chan struct{}, opt.MaxConcurrent), extVersions: map[string]string{}}
	if m.opt.ProtocolProbe == nil {
		m.opt.ProtocolProbe = m.defaultProtocolProbe
	}
	if err := os.MkdirAll(m.toolsDir(), 0o700); err != nil {
		return nil, err
	}
	return m, nil
}

var (
	toolRe    = regexp.MustCompile(`^(plugin:)?[a-z0-9][a-z0-9._@-]{0,63}$`)
	versionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)
)

func validTool(t string) error {
	if !toolRe.MatchString(t) {
		return contracts.Errorf(contracts.ErrInvalid, "invalid tool name %q", t)
	}
	return nil
}

func isPlugin(tool string) bool { return strings.HasPrefix(tool, "plugin:") }

func (m *Manager) toolsDir() string { return filepath.Join(m.opt.StateDir, "tools") }
func (m *Manager) toolDir(tool string) string {
	return filepath.Join(m.toolsDir(), strings.ReplaceAll(tool, ":", "@"))
}
func (m *Manager) versionDir(tool, version string) string {
	return filepath.Join(m.toolDir(tool), version)
}
func (m *Manager) currentLink(tool string) string { return filepath.Join(m.toolDir(tool), "current") }

func (m *Manager) toolLock(tool string) *sync.Mutex {
	l, _ := m.locks.LoadOrStore(tool, &sync.Mutex{})
	return l.(*sync.Mutex)
}

func (m *Manager) load() (*stateFile, error) {
	st := &stateFile{Tools: map[string][]versionRecord{}}
	b, err := os.ReadFile(filepath.Join(m.toolsDir(), "state.json"))
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("tools state: %w", err)
	}
	if st.Tools == nil {
		st.Tools = map[string][]versionRecord{}
	}
	return st, nil
}

func (m *Manager) save(st *stateFile) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	return writeFileAtomic(filepath.Join(m.toolsDir(), "state.json"), append(b, '\n'), 0o600)
}

// mutate runs f on the state under the state lock and saves it.
func (m *Manager) mutate(f func(st *stateFile) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, err := m.load()
	if err != nil {
		return err
	}
	if err := f(st); err != nil {
		return err
	}
	return m.save(st)
}

func findRec(st *stateFile, tool, version string) (int, *versionRecord) {
	for i := range st.Tools[tool] {
		if st.Tools[tool][i].Version == version {
			return i, &st.Tools[tool][i]
		}
	}
	return -1, nil
}

func activeRec(st *stateFile, tool string) *versionRecord {
	for i := range st.Tools[tool] {
		if st.Tools[tool][i].State == StateActive {
			return &st.Tools[tool][i]
		}
	}
	return nil
}

func (m *Manager) exePath(r versionRecord) string {
	return filepath.Join(m.versionDir(r.Tool, r.Version), r.Exe)
}

// List returns all known versions with InUseByRuns filled, plus external
// (unmanaged) tools that are the default while no managed version is active.
func (m *Manager) List(ctx context.Context) ([]contracts.ToolVersion, error) {
	m.mu.Lock()
	st, err := m.load()
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	runs := m.activeRuns(ctx)
	var out []contracts.ToolVersion
	tools := make([]string, 0, len(st.Tools))
	for t := range st.Tools {
		tools = append(tools, t)
	}
	sort.Strings(tools)
	for _, t := range tools {
		recs := st.Tools[t]
		sort.SliceStable(recs, func(i, j int) bool { return recs[i].InstalledAt.After(recs[j].InstalledAt) })
		for _, r := range recs {
			tv := r.ToolVersion
			tv.Path = m.exePath(r)
			tv.InUseByRuns = runsUsing(runs, m.versionDir(r.Tool, r.Version), tv.Path)
			out = append(out, tv)
		}
	}
	for _, t := range sortedKeys(m.opt.External) {
		if activeRec(st, t) != nil {
			continue
		}
		p := m.opt.External[t]
		tv := contracts.ToolVersion{Tool: t, Version: m.externalVersion(ctx, t, p), Path: p, State: StateActive, Health: "unchecked"}
		if real, err := filepath.EvalSymlinks(p); err == nil {
			tv.InUseByRuns = runsUsing(runs, "", real)
		}
		tv.InUseByRuns = append(tv.InUseByRuns, runsUsing(runs, "", p)...)
		out = append(out, tv)
	}
	return out, nil
}

func (m *Manager) activeRuns(ctx context.Context) []configuration.RunRef {
	if m.opt.Runs == nil {
		return nil
	}
	r, err := m.opt.Runs.ActiveRuns(ctx)
	if err != nil {
		return nil
	}
	return r
}

func runsUsing(runs []configuration.RunRef, dir, exe string) []string {
	var out []string
	for _, r := range runs {
		if r.Executable == "" {
			continue
		}
		if r.Executable == exe || (dir != "" && strings.HasPrefix(r.Executable, dir+string(filepath.Separator))) {
			out = append(out, r.RunID)
		}
	}
	sort.Strings(out)
	return out
}

// Pinned returns the executable (or plugin directory) new runs should use:
// a real path inside the active version directory, so a later activation
// does not change what an already-launched run executes. It implements
// configuration.VersionSource.
func (m *Manager) Pinned(ctx context.Context, tool string) (configuration.Pin, bool) {
	m.mu.Lock()
	st, err := m.load()
	m.mu.Unlock()
	if err == nil {
		if r := activeRec(st, tool); r != nil {
			return configuration.Pin{Tool: tool, Version: r.Version, Path: m.exePath(*r), Digest: r.Digest}, true
		}
	}
	if p, ok := m.opt.External[tool]; ok {
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			real = p
		}
		return configuration.Pin{Tool: tool, Version: m.externalVersion(ctx, tool, p), Path: real}, true
	}
	return configuration.Pin{}, false
}

// ActiveVersions returns tool -> version for new runs.
func (m *Manager) ActiveVersions(ctx context.Context) map[string]string {
	out := map[string]string{}
	m.mu.Lock()
	st, err := m.load()
	m.mu.Unlock()
	if err == nil {
		for t := range st.Tools {
			if r := activeRec(st, t); r != nil {
				out[t] = r.Version
			}
		}
	}
	for t, p := range m.opt.External {
		if _, ok := out[t]; !ok {
			if v := m.externalVersion(ctx, t, p); v != "" {
				out[t] = v
			}
		}
	}
	return out
}

var _ configuration.VersionSource = (*Manager)(nil)

func (m *Manager) externalVersion(ctx context.Context, tool, path string) string {
	m.extMu.Lock()
	v, ok := m.extVersions[tool+"\x00"+path]
	m.extMu.Unlock()
	if ok {
		return v
	}
	v, _ = m.probeVersion(ctx, tool, path)
	m.extMu.Lock()
	m.extVersions[tool+"\x00"+path] = v
	m.extMu.Unlock()
	return v
}

// Prune removes payloads that are not active, not staged, not in use by a
// live run and beyond the newest Keep healthy previous versions. Records of
// failed versions are kept for the report; their payloads are already gone.
func (m *Manager) Prune(ctx context.Context) ([]contracts.ToolVersion, error) {
	runs := m.activeRuns(ctx)
	if m.opt.Runs != nil && runs == nil {
		if _, err := m.opt.Runs.ActiveRuns(ctx); err != nil {
			return nil, fmt.Errorf("prune: cannot list runs (%v); nothing removed", err)
		}
	}
	var removed []contracts.ToolVersion
	err := m.mutate(func(st *stateFile) error {
		for tool, recs := range st.Tools {
			sort.SliceStable(recs, func(i, j int) bool { return recs[i].InstalledAt.After(recs[j].InstalledAt) })
			keptPrev := 0
			var keep []versionRecord
			for _, r := range recs {
				inUse := len(runsUsing(runs, m.versionDir(r.Tool, r.Version), m.exePath(r))) > 0
				switch {
				case r.State == StateActive || r.State == StateStaged || inUse:
					keep = append(keep, r)
				case r.State == StatePrevious && keptPrev < m.opt.Keep && r.Health == "ok":
					keptPrev++
					keep = append(keep, r)
				case r.State == StateFailed:
					keep = append(keep, r)
				default:
					if err := os.RemoveAll(m.versionDir(r.Tool, r.Version)); err != nil {
						return err
					}
					removed = append(removed, r.ToolVersion)
				}
			}
			st.Tools[tool] = keep
		}
		return nil
	})
	return removed, err
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func (m *Manager) report(ctx context.Context, kind contracts.EventKind, payload UpdateResult) {
	if m.opt.Reporter == nil {
		return
	}
	b, _ := json.Marshal(payload)
	_ = m.opt.Reporter.Report(ctx, contracts.ReportEvent, contracts.Event{
		EventID: contracts.NewID(contracts.PrefixEvent), Origin: "node:" + m.opt.NodeID, Kind: kind,
		Object: contracts.ObjectRef{Kind: contracts.KindNode, ID: m.opt.NodeID}, PayloadVersion: 1, Payload: b,
		OccurredAt: m.opt.Clock.Now(),
	})
}

// UpdateResult is the payload of update.staged / update.result events.
type UpdateResult struct {
	Tool       string `json:"tool"`
	Version    string `json:"version"`
	Outcome    string `json:"outcome"` // "staged" | "stage_failed" | "activated" | "failed_reverted" | "rolled_back" | "refused"
	Health     string `json:"health,omitempty"`
	RevertedTo string `json:"reverted_to,omitempty"`
	Detail     string `json:"detail,omitempty"`
}
