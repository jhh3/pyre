// Package accounts is the node side of Pyre's subscription management
// (R08; decisions I08; the accounts spike report). It owns:
//
//   - the profile registry: one isolated profile per (account, node, harness)
//     under <root>/<account-id>/<harness>/, canonical and 0700, composed from
//     the user's configuration without ever copying a credential;
//   - device grant records (kind, canonical profile dir, generation, process
//     limit, refresh owner "harness", state) and local identity verification
//     before every launch, without any network refresh;
//   - race-safe selection/admission of a same-provider account for a run, with
//     per-grant process leases (Codex: one app-server binding per home);
//   - bounded usage observation that reads tokens from the profile store only
//     and never refreshes them;
//   - fallback planning on a limit/login failure (native resume, handoff,
//     waiting, intervention), never across providers or into API billing.
//
// Credentials never leave the profile store: they are not logged, stored in
// the database, reported, or returned.
package accounts

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Reporter is the daemon's durable report hook (node outbox). Payloads are
// the contract types named by the report kind: AccountObservation,
// DeviceGrant, AccountAssignment, FallbackPlan, Event.
type Reporter interface {
	Report(ctx context.Context, kind contracts.ReportKind, payload any) error
}

// RunController is implemented by the node daemon so fallback planning can
// quiesce the old run and start its successor. The old run is never mutated
// beyond being stopped; it stays immutable for audit.
type RunController interface {
	// CurrentRun describes the session's current run on this node.
	CurrentRun(ctx context.Context, sessionID string) (RunInfo, error)
	// Quiesce stops the run (interrupt/stop as the adapter supports) and
	// waits until it has exited or the context ends. TurnInFlight reports
	// whether a turn was running when the limit hit (its external effects are
	// uncertain and its instruction must not be replayed).
	Quiesce(ctx context.Context, sessionID, runID string) (QuiesceResult, error)
	// StartContinuation starts the successor run bound to AccountID (the
	// daemon calls Resolver.ResolveFor with an explicit policy for it).
	// Kind native_resume: same session, Resume = NativeRef. Kind handoff:
	// request a handoff note and create a linked successor session
	// (Lineage.ForkedFrom/HandoffNoteID). The interrupted instruction is
	// never replayed. Return ErrConflict/ErrCapacityExhausted when the account
	// could not be bound so the planner can try the next candidate.
	StartContinuation(ctx context.Context, c Continuation) (ContinuationResult, error)
}

// RunInfo is the planner's view of a run.
type RunInfo struct {
	SessionID    string
	RunID        string
	Harness      contracts.Harness
	Capabilities []string
	NativeRef    *contracts.NativeRef
	TurnInFlight bool
	// State is the run's state (the rebalancer moves only waiting_input).
	State contracts.RunState
	// RecentInput: a message is queued for the session or was delivered
	// within the last minute (the rebalancer leaves it alone).
	RecentInput bool
	// Live: the run's process is still up.
	Live bool
}

// QuiesceResult reports how a run was stopped.
type QuiesceResult struct {
	Exited       bool
	TurnInFlight bool
	Detail       string
}

// Continuation asks the daemon to start a successor run.
type Continuation struct {
	SessionID     string
	PreviousRunID string
	Harness       contracts.Harness
	AccountID     string
	Kind          contracts.ContinuationKind // native_resume | handoff
	Resume        *contracts.NativeRef       // native_resume only
	UncertainWork bool                       // do not replay the interrupted instruction
	Reason        string
	// Prompt is the successor's first prompt: a short "continue where you
	// left off (switched from account "X" to account "Y" because of the usage limit)"
	// so the work goes on without a human "go". Never the interrupted
	// instruction itself.
	Prompt string
	// NativeOnly: start the successor only as a native resume (never a
	// handoff that would lose the conversation); proactive moves.
	NativeOnly bool
}

// ContinuationResult identifies the successor.
type ContinuationResult struct {
	NewRunID     string
	NewSessionID string // set for handoff successors (linked session)
	// Kind is the continuation the daemon actually used ("" = as planned):
	// a planned native resume whose thread turned out unreachable from the
	// new account starts as a handoff instead.
	Kind contracts.ContinuationKind
}

// CodexRateLimitReader is implemented by the Codex adapter (WP07). It calls
// `account/rateLimits/read` through the single Pyre app-server bound to
// codexHome and returns the raw result object. It must never start a second
// Codex process on a home whose lease is held by another holder.
type CodexRateLimitReader interface {
	ReadRateLimits(ctx context.Context, codexHome string) (json.RawMessage, error)
}

// Config configures a Service. Zero values select safe defaults.
type Config struct {
	NodeID string
	// DB is an open database (sqlitex.Open). Required.
	DB *sql.DB
	// ProfileRoot is where isolated profiles live
	// (default ~/.local/state/pyre/profiles).
	ProfileRoot string
	// SharedRoot holds the Pyre shared transcript stores
	// (default <ProfileRoot>/../shared).
	SharedRoot string
	// User configuration sources for composition (defaults: ~/.claude,
	// ~/.claude.json, $CODEX_HOME or ~/.codex). They are only read and linked;
	// never modified, moved or copied for credentials.
	UserClaudeDir  string
	UserClaudeJSON string
	UserCodexDir   string
	// UserAgyDir is agy's state dir (default ~/.gemini/antigravity-cli):
	// only its sign-in's identity claims are read (OwnLogin).
	UserAgyDir string
	// AgyBin is the agy executable for its usage reading (agyusage.go;
	// default "agy" on PATH).
	AgyBin string

	// Usage observation.
	ClaudeUsageURL  string        // default https://api.anthropic.com/api/oauth/usage
	ClaudeUserAgent string        // default "claude-cli/2.0.0 (external, cli)" (as rota)
	HTTPClient      *http.Client  // default 20s timeout, no redirects
	PollInterval    time.Duration // per account; default 5m, minimum 5m unless TestMode
	FreshFor        time.Duration // observation freshness window; default 15m
	MaxBackoff      time.Duration // default 6h
	CodexReader     CodexRateLimitReader
	// LoginRefresher lets the harness refresh its own login inside a
	// checked-out rota profile whose access token expired at rest (see
	// recovery.go). Nil: such accounts stay unverified-unknown and are chosen
	// only explicitly. Pyre itself never refreshes a token.
	LoginRefresher LoginRefresher
	// SecurityPath is the macOS `security` binary (tests use a shim).
	SecurityPath string
	// DefaultAccount is the policy for launches without an explicit
	// account (mode eligible): auto (default; ambient when this node has no
	// Pyre account for the harness, else an eligible subscription),
	// subscription (always an eligible subscription; waits when none) or
	// ambient (always the node's own login).
	DefaultAccount string
	// ClaudeBin, when set, makes grant verification also ask the harness
	// itself: `CLAUDE_CONFIG_DIR=<profile> claude auth status --json`
	// (Claude Code 2.1.268+) must report the profile as its
	// configDirectory and a subscription (not API-key) login. Local only,
	// ~0.2 s; it never refreshes. Empty: file/keychain signals only.
	ClaudeBin string
	// GOOS overrides runtime.GOOS (tests exercise the keychain path anywhere).
	GOOS string

	// Selection.
	// AllowUnknown lets eligible-policy selection fall back to accounts whose
	// availability is unknown/stale when no fresh account has headroom.
	// Default false: unknown is never treated as capacity.
	AllowUnknown bool
	// Fleet is the hub's account directory (active assignments per account
	// on every machine) for machine affinity (affinity.go). Nil: no spread
	// across machines, only local stickiness and headroom.
	Fleet FleetDirectory
	// FleetTimeout bounds one fleet read at selection (default 2s).
	FleetTimeout time.Duration
	// FleetCache is how long a fleet read is reused (default 5s; negative:
	// never). A failed read is reused for 30s so launches do not each wait.
	FleetCache time.Duration
	// NodeName is this machine's name for assignment reasons (default NodeID).
	NodeName string
	// MaxAssignmentsPerAccount bounds concurrent local runs per account
	// (0 = unlimited).
	MaxAssignmentsPerAccount int
	// SpreadRuns: an account already carrying this many active runs on this
	// machine is ranked after accounts with fewer (spread.go; default 4,
	// negative: off). Never a hard limit.
	SpreadRuns int
	// SpreadFivePercent: an account whose 5-hour window is at or above this
	// used-percent is ranked after accounts below it (default 50, negative:
	// off).
	SpreadFivePercent float64
	// ExhaustedPercent is the used-percent at or above which a window counts
	// as exhausted (default 100).
	ExhaustedPercent float64
	// DefaultCooldown applies after a limit event without a reset hint
	// (default 30m).
	DefaultCooldown time.Duration
	// SessionSwitchWindow is the circuit breaker for automatic account
	// switches: at most one per session within it (default 30m). A further
	// limit inside the window pauses switching for that session (the run is
	// left as it is and the owner is asked) instead of switching again.
	SessionSwitchWindow time.Duration
	// NodeSwitchMax / NodeSwitchWindow pause automatic switching once that
	// many different accounts ran out on this node within the window
	// (default 10 per 10m). Moving more sessions off an account already
	// counted is never paused: one account running out moves all of them.
	NodeSwitchMax    int
	NodeSwitchWindow time.Duration
	// ReauthCooldown holds a login the provider rejected (401) out of
	// selection before it is checked out and probed again (default 1h; a
	// rota snapshot rewritten by a new login ends it at once; reauth.go).
	ReauthCooldown time.Duration
	// CodexLeaseHolder identifies the shared per-home app-server binding
	// (default "codex-app-server"). Runs resolved with this holder share the
	// home's single-process lease (refcounted).
	CodexLeaseHolder string

	// Rota enables rota-checkout grants (accounts rota already holds).
	Rota RotaConfig
	// Tokens fetches hub-held account tokens (oauth-token grants; token.go).
	// Nil: token accounts are never launchable on this node.
	Tokens TokenSource
	// ClaudeMessagesURL is the token usage probe endpoint (default
	// https://api.anthropic.com/v1/messages).
	ClaudeMessagesURL string

	Reporter   Reporter
	Controller RunController
	Logger     *slog.Logger
	Now        func() time.Time
	// TestMode enables account.inject_limit and sub-5-minute poll intervals.
	TestMode bool
	// InjectReset is the reset an injected usage/rate limit announces
	// (test mode only; default 1h). Env PYRE_ACCOUNTS_INJECT_RESET (a Go
	// duration, e.g. "3m") sets it for real-stack recovery checks.
	InjectReset time.Duration
}

// EnvTestMode, set to "1" in pyred's environment, enables test mode
// (account.inject_limit) without a code change in the daemon.
const EnvTestMode = "PYRE_ACCOUNTS_TEST_MODE"

// EnvInjectReset overrides Config.InjectReset (test mode only).
const EnvInjectReset = "PYRE_ACCOUNTS_INJECT_RESET"

// Service is the node account manager. It is safe for concurrent use.
type Service struct {
	cfg  Config
	db   *sql.DB
	log  *slog.Logger
	now  func() time.Time
	rng  *rand.Rand
	rmu  sync.Mutex
	obmu sync.Mutex    // serializes usage probes (max concurrency 1)
	fbmu sync.Mutex    // serializes fallback planning (one replacement per run)
	comu sync.Mutex    // serializes rota checkout / write-back / check-in
	own  ownObsState   // own-login observation throttle (ownlogin.go)
	agy  agyUsageState // agy's own quota reading (agyusage.go)
	// refreshFailed backs off accounts whose harness could not refresh an
	// expired-at-rest login at checkout (automatic selection only).
	rfmu          sync.Mutex
	refreshFailed map[string]time.Time
	// holds counts in-process users of a profile that outlive its run (e.g.
	// a memory capture of the run's transcript); check-in waits for them.
	hmu   sync.Mutex
	holds map[string]int
	// tok caches hub-held tokens in memory only (token.go).
	tok tokenCache
	// fleet caches the hub's view of other machines' runs (affinity.go).
	fleet fleetCache
	// rb remembers idle runs and failed moves between sweeps (rebalance.go).
	rb rebalanceState
}

// New validates cfg, applies defaults and migrates the "accounts" namespace.
func New(ctx context.Context, cfg Config) (*Service, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("accounts: Config.DB is required")
	}
	if cfg.NodeID == "" {
		cfg.NodeID = "node_local"
	}
	home, _ := os.UserHomeDir()
	if cfg.ProfileRoot == "" {
		if home == "" {
			return nil, fmt.Errorf("accounts: no home directory; set ProfileRoot")
		}
		cfg.ProfileRoot = filepath.Join(home, ".local", "state", "pyre", "profiles")
	}
	if cfg.SharedRoot == "" {
		cfg.SharedRoot = filepath.Join(filepath.Dir(filepath.Clean(cfg.ProfileRoot)), "shared")
	}
	if cfg.UserClaudeDir == "" && home != "" {
		cfg.UserClaudeDir = filepath.Join(home, ".claude")
	}
	if cfg.UserClaudeJSON == "" && home != "" {
		cfg.UserClaudeJSON = filepath.Join(home, ".claude.json")
	}
	if cfg.UserCodexDir == "" {
		if v := os.Getenv("CODEX_HOME"); v != "" {
			cfg.UserCodexDir = v
		} else if home != "" {
			cfg.UserCodexDir = filepath.Join(home, ".codex")
		}
	}
	if cfg.UserAgyDir == "" && home != "" {
		cfg.UserAgyDir = filepath.Join(home, ".gemini", "antigravity-cli")
	}
	if cfg.ClaudeUsageURL == "" {
		cfg.ClaudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	}
	if cfg.ClaudeUserAgent == "" {
		cfg.ClaudeUserAgent = "claude-cli/2.0.0 (external, cli)"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{
			Timeout: 20 * time.Second,
			// Never follow redirects with a bearer attached.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Minute
	}
	if cfg.PollInterval < 5*time.Minute && !cfg.TestMode {
		cfg.PollInterval = 5 * time.Minute
	}
	if cfg.FreshFor <= 0 {
		cfg.FreshFor = 15 * time.Minute
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 6 * time.Hour
	}
	if cfg.ReauthCooldown <= 0 {
		cfg.ReauthCooldown = time.Hour
	}
	if cfg.SecurityPath == "" {
		cfg.SecurityPath = "/usr/bin/security"
	}
	if cfg.GOOS == "" {
		cfg.GOOS = runtime.GOOS
	}
	if cfg.ExhaustedPercent <= 0 {
		cfg.ExhaustedPercent = 100
	}
	if cfg.SpreadRuns == 0 {
		cfg.SpreadRuns = defaultSpreadRuns
	}
	if cfg.SpreadFivePercent == 0 {
		cfg.SpreadFivePercent = defaultSpreadFivePercent
	}
	if cfg.DefaultCooldown <= 0 {
		cfg.DefaultCooldown = 30 * time.Minute
	}
	if cfg.SessionSwitchWindow <= 0 {
		cfg.SessionSwitchWindow = 30 * time.Minute
	}
	if cfg.NodeSwitchMax <= 0 {
		cfg.NodeSwitchMax = 10
	}
	if cfg.NodeSwitchWindow <= 0 {
		cfg.NodeSwitchWindow = 10 * time.Minute
	}
	if cfg.FleetTimeout <= 0 {
		cfg.FleetTimeout = defaultFleetTimeout
	}
	if cfg.FleetCache == 0 {
		cfg.FleetCache = defaultFleetCache
	}
	if cfg.CodexLeaseHolder == "" {
		cfg.CodexLeaseHolder = "codex-app-server"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if os.Getenv(EnvTestMode) == "1" {
		// Opt-in for real-stack verification (account.inject_limit).
		cfg.TestMode = true
	}
	if v := os.Getenv(EnvInjectReset); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.InjectReset = d
		}
	}
	if cfg.InjectReset <= 0 {
		cfg.InjectReset = time.Hour
	}
	s := &Service{cfg: cfg, db: cfg.DB, log: cfg.Logger, now: func() time.Time { return cfg.Now().UTC() }}
	s.rng = rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0x9e3779b97f4a7c15))
	s.rotaDefaults()
	if err := sqlitex.Migrate(ctx, s.db, Migrations()...); err != nil {
		return nil, err
	}
	return s, nil
}

// jitter returns a random duration in [0, d).
func (s *Service) jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	s.rmu.Lock()
	defer s.rmu.Unlock()
	return time.Duration(s.rng.Int64N(int64(d)))
}

func (s *Service) report(ctx context.Context, kind contracts.ReportKind, payload any) {
	if s.cfg.Reporter == nil {
		return
	}
	if err := s.cfg.Reporter.Report(ctx, kind, payload); err != nil {
		s.log.Warn("accounts: report failed", "kind", string(kind), "err", redact(err.Error()))
	}
}
