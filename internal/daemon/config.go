package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/messaging/delivery"
)

// Reporter is the node's hub link: durable reports enqueued inside the
// caller's transaction (OriginSeq assigned), replaceable status, and
// connection state. *transport.NodeLink implements it.
type Reporter interface {
	delivery.Reporter
	Enqueue(ctx context.Context, kind contracts.ReportKind, payload any) (uint64, error)
	SendStatus(ctx context.Context, st contracts.NodeStatusReport) error
	Connected() bool
}

// AccountResolver binds an account to a run before launch (WP13 / Rota).
// It returns the isolated profile directory and environment for the run
// and, when a Pyre account was selected, the assignment to report. It never
// returns credential bytes for persistence: env is passed only to the
// launched process.
type AccountResolver interface {
	Resolve(ctx context.Context, harness contracts.Harness, policy contracts.AccountPolicy, runID string) (profileDir string, env map[string]string, assignment *contracts.AccountAssignment, err error)
}

// LimitHandler is optionally implemented by an AccountResolver to plan a
// fallback when a run reports a usage limit or unusable login.
type LimitHandler interface {
	OnLimit(ctx context.Context, run contracts.Run, ev contracts.LimitEvent) (*contracts.FallbackPlan, error)
}

// AmbientAccounts is the default resolver: runs use the node's existing
// default login. It reports that honestly (no assignment, run marked
// ambient) and refuses explicit account requests it cannot honour.
type AmbientAccounts struct{}

func (AmbientAccounts) Resolve(ctx context.Context, h contracts.Harness, p contracts.AccountPolicy, runID string) (string, map[string]string, *contracts.AccountAssignment, error) {
	if p.Mode == contracts.AccountExplicit || p.AccountID != "" {
		return "", nil, nil, contracts.Errorf(contracts.ErrUnsupported, "no account resolver on this node; cannot bind explicit account %q (only the ambient login is available)", p.AccountID)
	}
	return "", nil, nil, nil
}

func (AmbientAccounts) OnLimit(ctx context.Context, run contracts.Run, ev contracts.LimitEvent) (*contracts.FallbackPlan, error) {
	return &contracts.FallbackPlan{SessionID: run.SessionID, FromRunID: run.RunID, FromAccountID: run.AccountID,
		Outcome: contracts.FallbackIntervention, UncertainWork: true, PlannedAt: ev.At,
		Reason: "run on the ambient login hit " + ev.Kind + "; no account resolver configured for fallback"}, nil
}

// EnvRequest asks the environment manager (WP14 / Outfit) to resolve the
// environment generation for a launch.
type EnvRequest struct {
	NodeID    string
	Harness   contracts.Harness
	Profile   string
	ProjectID *string
	SessionID string
	RunID     string
	Cwd       string
	// ConfigHome is the isolated harness home bound by the account binder
	// (CLAUDE_CONFIG_DIR / CODEX_HOME), or "" for the user's default home.
	ConfigHome string
}

// EnvResult is a resolved, immutable environment generation for a run.
type EnvResult struct {
	GenerationID string
	Env          map[string]string // may contain secrets; never persisted
	Args         []string          // extra harness args (e.g. --settings)
	ProfileDir   string            // config home, if the profile provides one
	// Executable pins the harness binary for this run (WP14 updates:
	// upd.Pinned(ctx, tool).Path). Empty keeps the probed executable.
	Executable     string
	RuntimeVersion string // version of the pinned executable
	// ConnectionGrants are the generation's named connection IDs, passed
	// to LaunchPrep (credential provisioning re-checks each grant).
	ConnectionGrants []string
}

// LaunchPrepRequest asks integrations (e.g. connection provisioning, WP12)
// for per-launch additions after the environment is resolved.
type LaunchPrepRequest struct {
	SessionID        string
	RunID            string
	ProjectID        *string
	Harness          contracts.Harness
	ConnectionGrants []string
}

// LaunchPrepResult adds environment (in memory only) and a nonsecret
// manifest recorded with the run (AdapterData["pyre.connections"]).
type LaunchPrepResult struct {
	Env      map[string]string
	Manifest json.RawMessage
}

// LaunchPrepFunc prepares integration resources for a launch. An error
// fails the launch with an actionable reason.
type LaunchPrepFunc func(ctx context.Context, req LaunchPrepRequest) (LaunchPrepResult, error)

// RunEnded describes a run that just ended on this node.
type RunEnded struct {
	Run     contracts.Run
	Session contracts.Session
	// ProfileDir is the run's harness config home (CLAUDE_CONFIG_DIR /
	// CODEX_HOME) when Pyre bound one, else "".
	ProfileDir string
}

// HandleProfileDir is the RunHandle.Data key recording the run's bound
// harness config home (nonsecret path).
const HandleProfileDir = "pyre.profile_dir"

// PeriodicReport emits a durable report on an interval, only when the
// payload changes (e.g. ReportConnections from WP12).
type PeriodicReport struct {
	Kind     contracts.ReportKind
	Interval time.Duration
	Collect  func(ctx context.Context) (any, error)
}

// EnvResolver resolves environment generations before launch.
type EnvResolver interface {
	Resolve(ctx context.Context, req EnvRequest) (EnvResult, error)
}

// GuideFunc returns the short agent tool guide for a session (WP08 owns
// the text). Empty means no guide.
type GuideFunc func(h contracts.Harness, s contracts.Session) string

// UpstreamFunc returns a hub API client authenticating with bearer.
// internal/client.New(hubURL, bearer, opts) is the production value.
type UpstreamFunc func(bearer string) contracts.API

// TerminalOpener starts a relayed web-terminal stream running argv (the
// run's local attach command) and returns once it is connected.
// internal/terminal.NodeBridge is the production value.
type TerminalOpener interface {
	Open(ctx context.Context, cmd contracts.TerminalOpenCommand, argv []string) error
}

// PollConfig bounds the observation loop.
type PollConfig struct {
	Interval      time.Duration // active poll interval, default 2s
	IdleMax       time.Duration // max interval when nothing changes, default 30s
	MaxConcurrent int           // concurrent Observe calls, default 4
	Timeout       time.Duration // per Observe call, default 10s
}

// Config configures a Daemon.
type Config struct {
	// StateDir holds pyred.db, pyred.sock, runs/, logs/ (created 0700).
	StateDir string
	// DB, if set, is the node database (shared with the NodeLink). Else
	// StateDir/pyred.db is opened.
	DB *sql.DB

	// NodeID from the enrollment credential. Empty: a stable local ID is
	// generated and persisted (standalone node).
	NodeID   string
	NodeName string
	// SSHAlias is how other machines reach this node for ssh_tmux attach.
	SSHAlias string
	Labels   map[string]string

	Adapters []contracts.RuntimeAdapter
	// Reporter is the hub link; nil = standalone (reports are dropped and
	// local reads keep working).
	Reporter Reporter

	Accounts AccountResolver // default AmbientAccounts
	// OwnLogin identifies the machine's own login per harness for the
	// inventory (HarnessInfo.Login); read-only, nil = not reported.
	OwnLogin     func(contracts.Harness) *contracts.HarnessLogin
	Environments EnvResolver // nil: env profiles unsupported
	AgentGuide   GuideFunc
	LaunchPrep   LaunchPrepFunc // optional integration launch hook
	// OnRunEnded, when set, is called once a run reaches a terminal state,
	// after its end is recorded and BEFORE its account assignment is
	// released, so an integration can hold the profile (e.g. hand the
	// transcript to a memory system) before a rota check-in. It must not
	// block: queue work and return.
	OnRunEnded func(RunEnded)
	// Integrations reports personal-tool status (hive/outfit/rota) for
	// the inventory action; it overrides the built-in PATH probe.
	Integrations func(ctx context.Context) map[string]contracts.IntegrationStatus
	Periodic     []PeriodicReport
	// RepoIndex configures repo discovery and session worktree tracking
	// (repos.go).
	RepoIndex RepoIndexConfig

	// PyreBinDir is prepended to PATH in runs (pyre CLI for agent tools).
	// Default: the directory of the running executable.
	PyreBinDir string
	// ActionsUnavailable, if set, says why integration-provided node
	// actions (accounts, connections, environment, updates, memory) are
	// not registered, e.g. "pyred runs with --no-integrations". It is
	// appended to the unsupported error for an unknown action.
	ActionsUnavailable string
	// PyreCLI is the absolute path of the pyre CLI, exported to runs as
	// PYRE_CLI (agents' shells may re-source a profile that resets PATH).
	PyreCLI string
	// AgentMCP is the command (argv) of the agent tool MCP server, e.g.
	// [<pyre>, "mcp"]. Claude runs get it via --mcp-config unless the
	// environment overlay already supplied an --mcp-config.
	AgentMCP []string
	// SocketPath is the local API socket. Default StateDir/pyred.sock.
	SocketPath string
	// Upstream builds hub clients for the local API proxy; nil = no hub.
	Upstream UpstreamFunc
	// OwnerToken is the owner client credential used for local socket
	// requests that carry no bearer (the socket is 0600: same user only).
	OwnerToken string
	// Terminal opens relayed web-terminal streams (terminal.NodeBridge);
	// nil: terminal_open commands are unsupported on this node.
	Terminal TerminalOpener

	Clock  contracts.Clock
	Logger *slog.Logger

	Poll              PollConfig
	HeartbeatInterval time.Duration // default 30s (hub marks stale after ~90s)
	MaxRuns           int           // advertised launch capacity; 0 = unlimited
	// Permissions is the node's default run permission mode: "ask"
	// (default) or "bypass" (no approval prompts, no sandbox). A launch may
	// override it (PermissionsArg).
	Permissions    string
	LaunchTimeout  time.Duration // default 60s
	CommandTimeout time.Duration // interrupt/stop/attach/answer/read, default 20s
	MaxActions     int           // concurrent async actions, default 4
	OutboxFlush    time.Duration // local outbox flush interval, default 5s
	MaxOutbox      int           // pending local messages, default 1000
	ActivityMinGap time.Duration // min gap between activity reports per run, default 5s
	// ReadCacheQueue bounds hub reads waiting to be written to the node
	// read cache by its background writer (default DefaultReadCacheQueue).
	ReadCacheQueue int
	// BackgroundQueues, when set, reports integration queue depths and
	// limits (Hive capture, bridge) in Diagnostics.
	BackgroundQueues func() (queues, limits map[string]int)
}

func (c *Config) defaults() {
	if c.Clock == nil {
		c.Clock = contracts.SystemClock{}
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	if c.Accounts == nil {
		c.Accounts = AmbientAccounts{}
	}
	if c.Poll.Interval <= 0 {
		c.Poll.Interval = 2 * time.Second
	}
	if c.Poll.IdleMax <= 0 {
		c.Poll.IdleMax = 30 * time.Second
	}
	if c.Poll.IdleMax < c.Poll.Interval {
		c.Poll.IdleMax = c.Poll.Interval
	}
	if c.Poll.MaxConcurrent <= 0 {
		c.Poll.MaxConcurrent = 4
	}
	if c.Poll.Timeout <= 0 {
		c.Poll.Timeout = 10 * time.Second
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = 30 * time.Second
	}
	if c.LaunchTimeout <= 0 {
		c.LaunchTimeout = 60 * time.Second
	}
	if c.CommandTimeout <= 0 {
		c.CommandTimeout = 20 * time.Second
	}
	if c.MaxActions <= 0 {
		c.MaxActions = 4
	}
	if c.OutboxFlush <= 0 {
		c.OutboxFlush = 5 * time.Second
	}
	if c.MaxOutbox <= 0 {
		c.MaxOutbox = 1000
	}
	if c.ActivityMinGap <= 0 {
		c.ActivityMinGap = 5 * time.Second
	}
	if c.ReadCacheQueue <= 0 {
		c.ReadCacheQueue = DefaultReadCacheQueue
	}
}
