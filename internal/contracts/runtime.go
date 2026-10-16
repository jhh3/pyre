package contracts

import (
	"context"
	"strings"
	"time"
)

// Capability strings advertised by runtime adapters (per harness, version,
// platform). Clients must check capabilities; an unsupported action returns
// ErrUnsupported and never silently degrades to unverified terminal typing.
const (
	CapAttachNative       = "attach.native_terminal" // real harness TUI via tmux
	CapAttachWeb          = "attach.web_terminal"
	CapAttachReadOnly     = "attach.read_only"
	CapInputQueue         = "input.queue"      // structured queued delivery with acceptance evidence
	CapInputSteer         = "input.steer"      // explicit steering mid-turn
	CapInputIdempotent    = "input.idempotent" // runtime dedups by message key; safe retry
	CapInputReconcile     = "input.reconcile"  // can find evidence after a crash window
	CapApprovals          = "approvals.answer"
	CapQuestions          = "questions.answer"
	CapInterrupt          = "interrupt"
	CapStop               = "stop"
	CapResumeNative       = "resume.native"
	CapResumeHandoff      = "resume.handoff"
	CapSubagentsObserve   = "subagents.observe"
	CapTranscript         = "transcript.read"
	CapScreen             = "screen.read"
	CapAccountProfile     = "account.isolated_profile"
	CapAccountCrossResume = "account.cross_account_resume"
	CapHooks              = "hooks.events"
	CapAgentTools         = "agent.tools" // pyre CLI/MCP tools injected into the session
	// Mechanism flags (WP01 report §10): how input/attach is implemented.
	CapInputPaste          = "input.paste"             // bracketed paste gated on hook state (Claude TUI)
	CapInputChannel        = "input.channel"           // Claude dev channel (gated; consent dialog)
	CapInputStopHook       = "input.stop_hook"         // turn-end steer via Stop hook
	CapInputAppServer      = "input.app_server"        // Codex app-server turn/start
	CapApprovalsRaceTUI    = "approvals.race_with_tui" // native TUI may answer first; check effect
	CapAttachCodexRemote   = "attach.codex_remote"     // codex resume --remote inside Pyre tmux
	CapRequiresTrustDialog = "requires_trust_dialog"
	// CapInputMonitor (v1.6): queued mail reaches the agent while paste is
	// refused (human attached, dialog or approval open) through a per-run
	// Claude plugin monitor ("doorbell") plus the pyre inbox tool;
	// acceptance arrives asynchronously as claude.tool.inbox_read.
	CapInputMonitor = "input.monitor"
	// CapInputPlugin (W-plugdeliver, additive): queued input reaches Claude
	// through the per-run plugin's hooks module ($.prompt.submit between
	// turns), never typed into the TUI, also while a human is attached;
	// acceptance is claude.plugin.prompt_submit. Per run it applies only once
	// the module reported it is loaded; paste is the fallback.
	CapInputPlugin = "input.plugin_prompt"
	// CapPermissionsBypass: the run executes without approval prompts or
	// sandbox (pyred/pyre start --permissions bypass); the approval broker
	// sees no requests.
	CapPermissionsBypass = "permissions.bypass"
)

// HandleCapsUnavailable (v1.6) is a RunHandle.Data key an adapter sets at
// Launch: comma-separated capabilities it advertises in general (probe)
// that this run does not have, e.g. input.monitor when the launch
// environment disables Claude plugin monitors. The node removes them from
// the run's capabilities.
const HandleCapsUnavailable = "caps_unavailable"

// WithoutCaps returns caps minus the comma-separated list remove.
func WithoutCaps(caps []string, remove string) []string {
	if remove == "" {
		return caps
	}
	drop := map[string]bool{}
	for _, c := range strings.Split(remove, ",") {
		if c = strings.TrimSpace(c); c != "" {
			drop[c] = true
		}
	}
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		if !drop[c] {
			out = append(out, c)
		}
	}
	return out
}

// HasCap reports whether caps contains c.
func HasCap(caps []string, c string) bool {
	for _, x := range caps {
		if x == c {
			return true
		}
	}
	return false
}

// RunHandle is the adapter's serializable reference to a live run. The
// daemon persists it so a restarted daemon can Recover without relaunching.
type RunHandle struct {
	RunID      string            `json:"run_id"`
	SessionID  string            `json:"session_id"`
	Adapter    string            `json:"adapter"`
	Harness    Harness           `json:"harness"`
	TmuxSocket string            `json:"tmux_socket,omitempty"`
	TmuxTarget string            `json:"tmux_target,omitempty"`
	PID        int               `json:"pid,omitempty"`
	StateDir   string            `json:"state_dir"` // per-run private dir (hooks logs, sockets)
	NativeRef  *NativeRef        `json:"native_ref,omitempty"`
	Data       map[string]string `json:"data,omitempty"` // adapter-private, nonsecret
}

// ResolvedLaunch is fully resolved on the executing node. It contains local
// paths and credential references; it is never returned to general clients.
type ResolvedLaunch struct {
	RunID      string            `json:"run_id"`
	SessionID  string            `json:"session_id"`
	Harness    Harness           `json:"harness"`
	Title      string            `json:"title"`
	Cwd        string            `json:"cwd"`
	Executable string            `json:"executable"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"-"` // may include session token; never serialized
	// Unset lists inherited environment variables the launcher must remove
	// (e.g. ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN, OPENAI_API_KEY,
	// CODEX_API_KEY) so an ambient credential cannot override the bound account.
	Unset         []string   `json:"unset,omitempty"`
	StateDir      string     `json:"state_dir"`
	ProfileDir    string     `json:"profile_dir,omitempty"` // isolated account/config home
	InitialPrompt string     `json:"initial_prompt,omitempty"`
	EnvGeneration string     `json:"env_generation,omitempty"`
	Resume        *NativeRef `json:"resume,omitempty"` // native continuation target
	// AgentGuide is the short tool guide (and optional bounded brief) given
	// to agent sessions, e.g. via an appended system prompt.
	AgentGuide string `json:"agent_guide,omitempty"`
}

// ProbeResult reports an adapter's availability on this node.
type ProbeResult struct {
	Harness      Harness  `json:"harness"`
	Adapter      string   `json:"adapter"`
	Version      string   `json:"version"`
	Executable   string   `json:"executable"`
	Available    bool     `json:"available"`
	Reason       string   `json:"reason,omitempty"`
	Capabilities []string `json:"capabilities"`
}

// Observation is a batch of adapter observations since a cursor.
type Observation struct {
	State       RunState              `json:"state"`
	StateReason string                `json:"state_reason,omitempty"`
	ObservedAt  time.Time             `json:"observed_at"`
	NativeRef   *NativeRef            `json:"native_ref,omitempty"`
	Approvals   []Approval            `json:"approvals,omitempty"` // current pending + newly resolved
	Subagents   []SubagentObservation `json:"subagents,omitempty"`
	Activity    string                `json:"activity,omitempty"` // latest preview line
	// Doing is the short "doing now" phrase for a working run (v1.9,
	// additive): see Run.Activity. Empty when idle or unknown.
	Doing    string `json:"doing,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	// LimitEvent is set when the adapter detects a provider usage limit or
	// unusable login (input to account fallback).
	LimitEvent *LimitEvent `json:"limit_event,omitempty"`
	// Receipts are acceptance evidence discovered asynchronously (e.g. hook
	// receipts for marked input).
	Receipts   []DeliveryReceipt `json:"receipts,omitempty"`
	NextCursor string            `json:"next_cursor"`
}

// LimitEvent describes a detected limit/login failure.
type LimitEvent struct {
	Kind    string     `json:"kind"` // "usage_limit" | "auth_invalid" | "rate_limited"
	Detail  string     `json:"detail,omitempty"`
	ResetAt *time.Time `json:"reset_at,omitempty"`
	At      time.Time  `json:"at"`
}

// DeliveryInput is one message to deliver to a run.
type DeliveryInput struct {
	MessageID string      `json:"message_id"`
	AttemptID string      `json:"attempt_id"`
	Mode      MessageMode `json:"mode"`
	// Text is the rendered input including a Pyre marker the adapter can use
	// to find acceptance evidence, e.g. "[pyre msg_x from ses_y] ...".
	Text string `json:"text"`
}

// DeliveryResult is the adapter's evidence-backed outcome.
type DeliveryResult struct {
	State       RuntimeDeliveryState `json:"state"` // accepted | rejected | indeterminate | queued (busy; try later)
	Evidence    EvidenceCategory     `json:"evidence"`
	EvidenceRef string               `json:"evidence_ref,omitempty"`
	Detail      string               `json:"detail,omitempty"`
}

// RuntimeAdapter is the boundary between the node daemon and a harness.
// Adapters must document how input acceptance is known, busy handling,
// idempotency, and crash-window behavior (docs/capabilities.md).
type RuntimeAdapter interface {
	Name() string
	Harness() Harness
	Probe(ctx context.Context) (ProbeResult, error)
	// Launch starts a new execution. It must not be called for a run that
	// already has a live handle.
	Launch(ctx context.Context, l ResolvedLaunch) (RunHandle, error)
	// Recover re-establishes control of a live run after a daemon restart
	// without launching a duplicate. Returns ErrNotFound if it is gone.
	Recover(ctx context.Context, h RunHandle) (RunHandle, error)
	Observe(ctx context.Context, h RunHandle, cursor string) (Observation, error)
	// Deliver submits input. Callers persist an attempt before calling. If
	// the runtime is busy and cannot queue, return State=queued (no attempt
	// consumed). Unknown outcome => indeterminate, never accepted.
	Deliver(ctx context.Context, h RunHandle, in DeliveryInput) (DeliveryResult, error)
	// Reconcile looks for acceptance evidence for an earlier attempt.
	Reconcile(ctx context.Context, h RunHandle, messageID string) (DeliveryResult, error)
	Attach(ctx context.Context, h RunHandle, req AttachCommand) (AttachDescriptor, error)
	Interrupt(ctx context.Context, h RunHandle) (CommandReceipt, error)
	Stop(ctx context.Context, h RunHandle) (CommandReceipt, error)
	Answer(ctx context.Context, h RunHandle, a Approval, optionID, text string) (CommandReceipt, error)
	ReadTranscript(ctx context.Context, h RunHandle, req ReadSessionRequest) (ReadSessionResponse, error)
	// ResumeSpec returns how to continue a previous run (native ref to pass
	// in ResolvedLaunch.Resume) or ErrUnsupported.
	ResumeSpec(ctx context.Context, prev Run, sameAccount bool) (ContinuationKind, *NativeRef, error)
}
