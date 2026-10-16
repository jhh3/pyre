package contracts

import "time"

// Node control protocol: JSON frames over a WebSocket the node opens to the
// hub at PathNodeConnect with its node bearer token.
//
// Durability rules:
//   - Commands: the hub persists each command with a per-node monotonically
//     increasing Seq before sending. The node durably records a command
//     before sending CommandAck. On reconnect the hub resends, in Seq order,
//     every command the node has not acknowledged (CommandAck or a result),
//     whatever its Seq: commands can arrive out of order, so
//     NodeHello.LastCommandSeq is informational (it only flags a node whose
//     journal regressed). The node deduplicates by CommandID and returns its
//     stored result for duplicates (never re-executes). Sends to one node are
//     serialized in Seq order.
//   - Reports: durable reports carry a node-side monotonically increasing
//     OriginSeq. The node keeps them in a local outbox until the hub's
//     ReportAck covers them. The hub deduplicates by (node_id, origin_seq).
//     Replaceable observations (status heartbeats) use OriginSeq 0 and are
//     never queued.
//   - A heartbeat gap marks the node disconnected with last_seen_at; it does
//     not change any run's state except to mark observations stale.
type NodeFrameType string

const (
	FrameHello      NodeFrameType = "hello"       // node -> hub (first frame)
	FrameWelcome    NodeFrameType = "welcome"     // hub -> node
	FrameReport     NodeFrameType = "report"      // node -> hub
	FrameReportAck  NodeFrameType = "report_ack"  // hub -> node
	FrameCommand    NodeFrameType = "command"     // hub -> node
	FrameCommandAck NodeFrameType = "command_ack" // node -> hub (persisted, not executed)
	FramePing       NodeFrameType = "ping"
	FramePong       NodeFrameType = "pong"
	FrameError      NodeFrameType = "error" // fatal; connection closes (e.g. revoked, protocol mismatch)
)

type NodeFrame struct {
	Type       NodeFrameType `json:"type"`
	Hello      *NodeHello    `json:"hello,omitempty"`
	Welcome    *NodeWelcome  `json:"welcome,omitempty"`
	Report     *NodeReport   `json:"report,omitempty"`
	ReportAck  *ReportAck    `json:"report_ack,omitempty"`
	Command    *NodeCommand  `json:"command,omitempty"`
	CommandAck *CommandAck   `json:"command_ack,omitempty"`
	Error      *Error        `json:"error,omitempty"`
	At         time.Time     `json:"at"`
}

type NodeHello struct {
	NodeID         string            `json:"node_id"`
	Protocol       int               `json:"protocol"`
	MinProtocol    int               `json:"min_protocol"`
	PyredVersion   string            `json:"pyred_version"`
	LastCommandSeq int64             `json:"last_command_seq"`
	Hostname       string            `json:"hostname"`
	Platform       string            `json:"platform"`
	Arch           string            `json:"arch"`
	Harnesses      []HarnessInfo     `json:"harnesses"`
	Versions       map[string]string `json:"versions"`
	Capabilities   []string          `json:"capabilities"`
	Resources      NodeResources     `json:"resources"`
	// LiveRuns lists runs the node currently supervises (reconciliation).
	LiveRuns []Run `json:"live_runs"`
}

type NodeWelcome struct {
	HubID              string    `json:"hub_id"`
	Protocol           int       `json:"protocol"`
	MinProtocol        int       `json:"min_protocol"`
	ServerTime         time.Time `json:"server_time"`
	CommittedReportSeq uint64    `json:"committed_report_seq"` // resend reports after this
	HeartbeatSeconds   int       `json:"heartbeat_seconds"`
}

// ReportKind names a node report payload type.
type ReportKind string

const (
	ReportStatus          ReportKind = "status"              // NodeStatusReport (replaceable, seq 0)
	ReportRun             ReportKind = "run"                 // Run (observation; latest wins per run by OriginSeq)
	ReportReceipt         ReportKind = "receipt"             // DeliveryReceipt (durable fact)
	ReportApproval        ReportKind = "approval"            // Approval (requested/resolved/withdrawn)
	ReportCommandResult   ReportKind = "command_result"      // CommandResult
	ReportAccountObs      ReportKind = "account_observation" // AccountObservation
	ReportGrant           ReportKind = "grant"               // DeviceGrant
	ReportAssignment      ReportKind = "assignment"          // AccountAssignment
	ReportFallback        ReportKind = "fallback"            // FallbackPlan
	ReportEnvironment     ReportKind = "environment"         // EnvironmentGeneration
	ReportConnections     ReportKind = "connections"         // []Connection (metadata from Outfit)
	ReportEvent           ReportKind = "event"               // generic Event (Kind/Object/Payload); hub assigns cursor
	ReportSessionActivity ReportKind = "activity"            // SessionActivity (lightweight preview line)
	// ReportRunSnapshot (v1.5) carries the node's recent run records with
	// their sessions. A node sends it when the hub's committed report
	// watermark is behind what the hub acknowledged before (e.g. the hub was
	// restored from an older backup), so runs that ended meanwhile and
	// sessions created meanwhile are reconciled instead of staying unknown.
	ReportRunSnapshot ReportKind = "run_snapshot" // RunSnapshot
)

// SessionRun is one run with its session as the node knows it.
type SessionRun struct {
	Session Session `json:"session"`
	Run     Run     `json:"run"`
}

// RunSnapshot is the payload of a run_snapshot report: the node's most
// recent run records (live and ended), oldest first.
type RunSnapshot struct {
	Records []SessionRun `json:"records"`
	// Reason says why it was sent (e.g. "hub committed 12 < acked 40").
	Reason string `json:"reason,omitempty"`
}

type NodeReport struct {
	OriginSeq uint64     `json:"origin_seq"`
	Kind      ReportKind `json:"kind"`
	At        time.Time  `json:"at"`
	Payload   RawJSON    `json:"payload"`
}

type ReportAck struct {
	CommittedSeq uint64 `json:"committed_seq"`
}

type NodeStatusReport struct {
	Resources NodeResources     `json:"resources"`
	Harnesses []HarnessInfo     `json:"harnesses,omitempty"`
	Versions  map[string]string `json:"versions,omitempty"`
}

// SessionActivity is a small preview update (last line, state) for UIs.
type SessionActivity struct {
	SessionID string    `json:"session_id"`
	RunID     string    `json:"run_id"`
	Line      string    `json:"line"`
	At        time.Time `json:"at"`
}

// CommandKind names a hub->node command.
type CommandKind string

const (
	CmdStartRun       CommandKind = "start_run"       // StartRunCommand -> Run
	CmdDeliver        CommandKind = "deliver"         // DeliverCommand -> (ack only; receipts via reports)
	CmdCancelDelivery CommandKind = "cancel_delivery" // {message_id}
	CmdInterrupt      CommandKind = "interrupt"       // RunCommand -> CommandReceipt
	CmdStop           CommandKind = "stop"            // RunCommand -> CommandReceipt
	CmdAttach         CommandKind = "attach"          // AttachCommand -> AttachDescriptor
	CmdAnswer         CommandKind = "answer"          // AnswerCommand -> CommandReceipt
	CmdResume         CommandKind = "resume"          // ResumeCommand -> Run
	CmdReadTranscript CommandKind = "read_transcript" // ReadSessionRequest -> ReadSessionResponse
	CmdLeaseChanged   CommandKind = "lease_changed"   // InputLease (informational)
	CmdAction         CommandKind = "action"          // NodeActionRequest -> action result
	CmdTerminalOpen   CommandKind = "terminal_open"   // TerminalOpenCommand -> node dials PathNodeTerminal and bridges a PTY
)

type NodeCommand struct {
	CommandID string      `json:"command_id"`
	Seq       int64       `json:"seq"`
	Kind      CommandKind `json:"kind"`
	Actor     Principal   `json:"actor"`
	IssuedAt  time.Time   `json:"issued_at"`
	ExpiresAt *time.Time  `json:"expires_at,omitempty"` // node must not start an expired command
	Payload   RawJSON     `json:"payload"`
}

type CommandAck struct {
	CommandID string `json:"command_id"`
	Seq       int64  `json:"seq"`
}

// CommandResult is reported (durably) when a command finishes.
type CommandResult struct {
	CommandID string         `json:"command_id"`
	Kind      CommandKind    `json:"kind"`
	Outcome   CommandOutcome `json:"outcome"`
	Result    RawJSON        `json:"result,omitempty"`
	Error     *Error         `json:"error,omitempty"`
	At        time.Time      `json:"at"`
	// Interim marks an acknowledgement of a long-running command (async
	// node actions): the node accepted it and a final CommandResult with
	// the same command_id follows. The hub records it as delivered, not
	// finished (contract v1.4).
	Interim bool `json:"interim,omitempty"`
}

// StartRunCommand asks a node to launch a run for a session.
type StartRunCommand struct {
	Session       Session          `json:"session"`
	RunID         string           `json:"run_id"`
	Cwd           string           `json:"cwd"`
	Checkout      *CheckoutRequest `json:"checkout,omitempty"`
	EnvProfile    string           `json:"env_profile,omitempty"`
	AccountPolicy AccountPolicy    `json:"account_policy"`
	InitialPrompt string           `json:"initial_prompt,omitempty"`
	Args          []string         `json:"args,omitempty"`
	LaunchContext *LaunchContext   `json:"launch_context,omitempty"`
	// SessionToken is a session-scoped credential for agent tools inside the
	// run. Secret: the node passes it only via the run's environment and
	// never logs or reports it.
	SessionToken string `json:"session_token,omitempty"`
	// Resume, if set, continues from a previous run.
	PreviousRun  *Run `json:"previous_run,omitempty"`
	AllowHandoff bool `json:"allow_handoff,omitempty"`
}

// DeliverCommand hands a stored message to the recipient's node.
type DeliverCommand struct {
	Message   MessageEnvelope `json:"message"`
	SessionID string          `json:"session_id"`
	// BoundRunID is set if an earlier attempt bound this message to a run;
	// the node must not deliver it to a different run.
	BoundRunID string `json:"bound_run_id,omitempty"`
}

type RunCommand struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	Reason    string `json:"reason,omitempty"`
}

type AttachCommand struct {
	SessionID    string            `json:"session_id"`
	RunID        string            `json:"run_id"`
	ClientNodeID string            `json:"client_node_id,omitempty"`
	Preferred    []AttachMechanism `json:"preferred,omitempty"`
	ReadOnly     bool              `json:"read_only,omitempty"`
}

type AnswerCommand struct {
	Approval Approval `json:"approval"`
	OptionID string   `json:"option_id,omitempty"`
	Text     string   `json:"text,omitempty"`
}

// TerminalOpenCommand asks the node to open a web-terminal stream for a run:
// it dials PathNodeTerminal with its node token and StreamID, runs the run's
// attach command in a PTY, and relays bytes until either side closes.
type TerminalOpenCommand struct {
	StreamID string `json:"stream_id"`
	RunID    string `json:"run_id"`
	ReadOnly bool   `json:"read_only"`
	Cols     int    `json:"cols"`
	Rows     int    `json:"rows"`
	// Fit (read-only streams only) sizes the stream to the run's tmux
	// window instead of Cols x Rows, so watching never resizes the window:
	// the node reads the window size, attaches `-r -f ignore-size` (tmux
	// >= 3.2) on a PTY of exactly that size, sends a {"type":"resize"}
	// text frame with it before any terminal bytes (and again whenever the
	// window changes), and ignores client resize frames. Requires
	// TerminalFit on both the hub and the node.
	Fit bool `json:"fit,omitempty"`
}

// TerminalFit names fitted read-only terminal streams
// (TerminalOpenCommand.Fit): a hub feature (HelloResponse.Features) when
// its relay forwards the fit request and the node's size frames, and a
// node capability (Node.Capabilities) when its bridge serves them. A
// client asks for fit only when both advertise it; an older hub or node
// would otherwise attach at the client's size.
const TerminalFit = "terminal.fit"

type ResumeCommand struct {
	StartRunCommand
}
