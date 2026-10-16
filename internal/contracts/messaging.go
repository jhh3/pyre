package contracts

import (
	"context"
	"time"
)

// MessageMode separates ordinary queued input from explicit steering.
type MessageMode string

const (
	ModeQueue MessageMode = "queue" // deliver when the runtime can accept input
	ModeSteer MessageMode = "steer" // explicit immediate steering; only where supported
	ModeInfo  MessageMode = "info"  // stored and visible; never injected into a runtime
)

// AddressKind is the kind of message endpoint.
type AddressKind string

const (
	AddrSession AddressKind = "session"
	AddrOwner   AddressKind = "owner"   // the human owner's inbox
	AddrChannel AddressKind = "channel" // optional fan-out channel (additive)
	AddrProject AddressKind = "project" // project discussion thread
)

type Address struct {
	Kind AddressKind `json:"kind"`
	ID   string      `json:"id"` // session ID, "owner", channel name, project ID
}

// MessageEnvelope is the durable addressed message. The hub sets Sender from
// authentication; a client cannot claim a different sender.
type MessageEnvelope struct {
	SchemaVersion int          `json:"schema_version"`
	MessageID     string       `json:"message_id"`
	OperationID   string       `json:"operation_id"`
	Sender        Principal    `json:"sender"`
	Recipient     Address      `json:"recipient"`
	ProjectID     *string      `json:"project_id"`
	ThreadID      string       `json:"thread_id,omitempty"`
	InReplyTo     string       `json:"in_reply_to,omitempty"`
	Reissues      string       `json:"reissues,omitempty"` // explicit reissue of an earlier message
	Mode          MessageMode  `json:"mode"`
	Body          MessageBody  `json:"body"`
	Attachments   []Attachment `json:"attachments,omitempty"`
	CreatedAt     time.Time    `json:"created_at"` // client time; informational
	ExpiresAt     *time.Time   `json:"expires_at,omitempty"`
	RequestAck    bool         `json:"request_ack,omitempty"`
}

type MessageBody struct {
	Text string `json:"text"`
}

// Attachment references an authorized published object. Filesystem paths
// are not transferable.
type Attachment struct {
	ArtifactVersionID  string `json:"artifact_version_id,omitempty"`
	DocumentRevisionID string `json:"document_revision_id,omitempty"`
	NoteID             string `json:"note_id,omitempty"`
}

// StorageState: has the hub durably accepted the message?
type StorageState string

const (
	StoragePendingLocal StorageState = "pending_local" // in an origin outbox only
	StorageStored       StorageState = "stored"
	StorageRejected     StorageState = "rejected"
)

// RuntimeDeliveryState: did a recipient run's supported input accept it?
type RuntimeDeliveryState string

const (
	DeliveryNotApplicable RuntimeDeliveryState = "not_applicable" // owner/info/channel-archive messages
	DeliveryQueued        RuntimeDeliveryState = "queued"         // waiting (recipient busy/offline/leased)
	DeliveryNodeReceived  RuntimeDeliveryState = "node_received"  // recipient node committed it to its inbox
	DeliveryAttempting    RuntimeDeliveryState = "attempting"     // attempt durably claimed, bound to a run
	DeliveryAccepted      RuntimeDeliveryState = "accepted"       // runtime input interface accepted (evidence attached)
	DeliveryRejected      RuntimeDeliveryState = "rejected"       // runtime refused; safe to retry/reissue
	DeliveryIndeterminate RuntimeDeliveryState = "indeterminate"  // may have been accepted; needs reconciliation
	DeliveryExpired       RuntimeDeliveryState = "expired"        // retention ended before delivery
	DeliveryCancelled     RuntimeDeliveryState = "cancelled"      // cancelled before any attempt
)

// AckState: has the recipient explicitly acknowledged or replied?
type AckState string

const (
	AckNone         AckState = "none"
	AckAcknowledged AckState = "acknowledged"
	AckReplied      AckState = "replied"
)

// EvidenceCategory says how runtime acceptance was established.
type EvidenceCategory string

const (
	EvidenceProtocolAck   EvidenceCategory = "protocol_ack"   // structured API returned an accepted turn/item ID
	EvidenceHookReceipt   EvidenceCategory = "hook_receipt"   // harness hook observed the marked input
	EvidenceTranscript    EvidenceCategory = "transcript"     // marked input found in the native transcript
	EvidenceTerminalWrite EvidenceCategory = "terminal_write" // bytes written to a PTY only (weak; never "accepted")
	EvidenceNone          EvidenceCategory = "none"
	EvidenceMock          EvidenceCategory = "mock" // deterministic mock; not runtime validation
	// Harness-specific evidence kinds (WP01 report §10). EvidenceRef carries
	// the hook event / transcript uuid / item id.
	EvidenceClaudePromptHook EvidenceCategory = "claude.hook.user_prompt_submit"
	EvidenceClaudeQueue      EvidenceCategory = "claude.transcript.queue_enqueue"
	EvidenceClaudeUserRecord EvidenceCategory = "claude.transcript.user_record"
	EvidenceClaudeChannel    EvidenceCategory = "claude.channel.prompt"
	EvidenceClaudeStopHook   EvidenceCategory = "claude.stop_hook.injected"
	EvidenceCodexUserMessage EvidenceCategory = "codex.item.user_message.client_id"
	// v1.6 (additive): Claude doorbell (capability input.monitor).
	// EvidenceClaudeInboxRead: a pyre inbox/get_message tool call made by
	// the recipient run returned the message (PostToolUse hook). The agent
	// pulled the message itself, so it is acceptance (and consumption)
	// evidence even without a Pyre delivery attempt; the recipient node
	// mints the attempt when it records it (decision I16).
	EvidenceClaudeInboxRead EvidenceCategory = "claude.tool.inbox_read"
	// EvidenceClaudeMonitorNotified: the doorbell notification reached the
	// session. Informational only; it never establishes acceptance.
	EvidenceClaudeMonitorNotified EvidenceCategory = "claude.monitor.notified"
	// EvidencePyreRead (W-delivery, additive): the recipient session itself
	// read the message body through the Pyre API (MCP get_message / inbox /
	// wait_reply, or the same CLI commands run with the session's token), or
	// acknowledged / replied to it. Recorded by the hub; the agent has the
	// message, so it is acceptance (and consumption) and pull evidence: a node
	// that still holds it queued records the acceptance and never pastes it.
	EvidencePyreRead EvidenceCategory = "pyre.read"
	// EvidenceClaudePluginPrompt (W-plugdeliver, additive): Pyre's per-run
	// Claude plugin submitted the marked message with $.prompt.submit and
	// Claude resolved it as entered (its turn started). Acceptance evidence.
	EvidenceClaudePluginPrompt EvidenceCategory = "claude.plugin.prompt_submit"
)

// AcceptanceEvidence reports whether an evidence category can establish
// runtime acceptance. Terminal writes, "none" and informational kinds
// (claude.monitor.notified) never can; unknown kinds are treated as weak.
func AcceptanceEvidence(e EvidenceCategory) bool {
	switch e {
	case EvidenceProtocolAck, EvidenceHookReceipt, EvidenceTranscript, EvidenceMock,
		EvidenceClaudePromptHook, EvidenceClaudeQueue, EvidenceClaudeUserRecord,
		EvidenceClaudeChannel, EvidenceClaudeStopHook, EvidenceCodexUserMessage,
		EvidenceClaudeInboxRead, EvidencePyreRead, EvidenceClaudePluginPrompt:
		return true
	}
	return false
}

// ConsumptionEvidence reports whether evidence shows the runtime took the
// input into a turn (not merely queued it): a Claude transcript user
// record, a Codex userMessage item, or the agent reading the message with
// the pyre inbox tool. Such a receipt sets DeliveryStatus.ConsumedAt.
func ConsumptionEvidence(e EvidenceCategory) bool {
	return e == EvidenceClaudeUserRecord || e == EvidenceCodexUserMessage || e == EvidenceClaudeInboxRead || e == EvidencePyreRead
}

// PullEvidence reports evidence that the recipient runtime fetched the
// message itself (no Pyre delivery attempt was needed). A node that holds
// such a message still queued records the acceptance by minting an attempt
// bound to the reporting run (decision I16).
func PullEvidence(e EvidenceCategory) bool {
	return e == EvidenceClaudeInboxRead || e == EvidencePyreRead
}

// MessageMarker is the prefix rendered at the start of input delivered to a
// runtime so acceptance evidence can be matched: "[pyre-msg:<message_id>]".
// The agent sees it; agent guides explain it.
func MessageMarker(messageID string) string { return "[pyre-msg:" + messageID + "]" }

// DeliveryStatus is the set of independent facts about one message.
type DeliveryStatus struct {
	Storage       StorageState         `json:"storage"`
	StoredAt      *time.Time           `json:"stored_at,omitempty"`
	Runtime       RuntimeDeliveryState `json:"runtime"`
	RuntimeReason string               `json:"runtime_reason,omitempty"`
	BoundRunID    string               `json:"bound_run_id,omitempty"` // set when an attempt begins; released only when every attempt was deferred (runtime busy, nothing submitted); never rebound
	Ack           AckState             `json:"ack"`
	AckRef        string               `json:"ack_ref,omitempty"` // reply message ID or ack ID
	// ConsumedAt is set when evidence shows the runtime actually took the
	// accepted input into a turn (e.g. dequeued from Claude's native queue).
	ConsumedAt *time.Time        `json:"consumed_at,omitempty"`
	OutcomeRef *ObjectRef        `json:"outcome_ref,omitempty"`
	Receipts   []DeliveryReceipt `json:"receipts,omitempty"`
	UpdatedAt  time.Time         `json:"updated_at"`
}

// DeliveryReceipt is durable evidence about one delivery attempt.
type DeliveryReceipt struct {
	MessageID string `json:"message_id"`
	// AttemptID names the node's durable attempt. Adapter observations of
	// pull evidence (PullEvidence) may leave it empty; the node mints the
	// attempt before reporting, so receipts sent to the hub for attempts
	// carry one (decision I16). Hubs must still accept an empty value.
	AttemptID   string               `json:"attempt_id"`
	SessionID   string               `json:"session_id"`
	RunID       string               `json:"run_id"`
	NodeID      string               `json:"node_id"`
	State       RuntimeDeliveryState `json:"state"`
	Evidence    EvidenceCategory     `json:"evidence"`
	EvidenceRef string               `json:"evidence_ref,omitempty"` // e.g. codex turn id, hook event id
	Detail      string               `json:"detail,omitempty"`
	ObservedAt  time.Time            `json:"observed_at"`
	OriginSeq   uint64               `json:"origin_seq,omitempty"` // informational; hubs dedupe by NodeReport.OriginSeq
	// Consumed marks a receipt proving the runtime took accepted input into
	// a turn (sets DeliveryStatus.ConsumedAt); State stays "accepted".
	Consumed bool `json:"consumed,omitempty"`
}

// MessageView is a stored message plus its delivery facts.
type MessageView struct {
	Envelope MessageEnvelope `json:"envelope"`
	Status   DeliveryStatus  `json:"status"`
	Cursor   int64           `json:"cursor"` // hub ingestion order within the recipient's inbox
	// Delivery (W-stuckmail, additive) is a derived, human-readable summary
	// of how far the message got and, while it waits, why. Hubs fill it on
	// messages.send and messages.get (not on lists); older hubs omit it and
	// clients may derive it themselves (internal/messaging/msgstatus). It is
	// a presentation of Status plus the recipient's current run, never a
	// new fact: it claims delivery only with acceptance evidence.
	Delivery *DeliverySummary `json:"delivery_status,omitempty"`
}

// DeliverySummaryState is the coarse class of a DeliverySummary.
type DeliverySummaryState string

const (
	SummaryPending       DeliverySummaryState = "pending"       // not yet stored on the hub (origin outbox)
	SummaryStored        DeliverySummaryState = "stored"        // stored; no runtime delivery applies (owner, channel, project, info)
	SummaryDelivering    DeliverySummaryState = "delivering"    // recipient can take input; Pyre is submitting it
	SummaryQueued        DeliverySummaryState = "queued"        // waiting; Cause says why
	SummaryHeld          DeliverySummaryState = "held"          // recipient session ended; waits for a resume
	SummaryDelivered     DeliverySummaryState = "delivered"     // runtime accepted it (evidence recorded)
	SummaryRead          DeliverySummaryState = "read"          // the recipient fetched it itself
	SummaryAcknowledged  DeliverySummaryState = "acknowledged"  // explicit acknowledgement
	SummaryReplied       DeliverySummaryState = "replied"       // the recipient replied
	SummaryIndeterminate DeliverySummaryState = "indeterminate" // may have been accepted; needs reconciliation
	SummaryUndeliverable DeliverySummaryState = "undeliverable" // rejected, expired, cancelled, or recipient archived
)

// Delivery summary causes (DeliverySummary.Cause): why a message waits or
// will not be delivered.
const (
	CauseRecipientWorking  = "recipient_working"
	CauseRecipientFailed   = "recipient_last_turn_failed"
	CauseNodeDisconnected  = "node_disconnected"
	CauseRecipientUnknown  = "recipient_state_unknown"
	CauseWaitingApproval   = "recipient_waiting_approval"
	CauseRecipientStarting = "recipient_starting"
	CauseRecipientExiting  = "recipient_exiting"
	CauseRecipientEnded    = "recipient_ended"
	CauseRecipientArchived = "recipient_archived"
	CauseInputBlocked      = "input_blocked" // recipient idle, but its input is held (lease, attach, doorbell, ...)
	CauseRuntimeRejected   = "runtime_rejected"
	CauseExpired           = "expired"
	CauseCancelled         = "cancelled"
	CauseStorageRejected   = "storage_rejected"
)

// DeliverySummary is the derived delivery status of one message.
type DeliverySummary struct {
	State DeliverySummaryState `json:"state"`
	// Cause is a stable machine-readable reason (Cause* constants) for
	// queued, held and undeliverable messages.
	Cause string `json:"cause,omitempty"`
	// Summary is one line for people and agents, e.g. "queued: recipient's
	// last turn failed (model at capacity); Pyre retries at 19:52".
	Summary string `json:"summary"`
	// Since is when the current state began (stored, accepted, ended,
	// last seen), when known.
	Since *time.Time `json:"since,omitempty"`
	// Final: no reply can arrive unless someone acts (the recipient ended,
	// was archived, or delivery was refused, expired or cancelled).
	Final bool `json:"final,omitempty"`
}

type SendMessageRequest struct {
	Mutation
	// MessageID is optional; when supplied by an origin outbox it is stable
	// across retries. Same ID + different content => conflict.
	MessageID   string       `json:"message_id,omitempty"`
	Recipient   Address      `json:"recipient"`
	ProjectID   *string      `json:"project_id,omitempty"`
	ThreadID    string       `json:"thread_id,omitempty"`
	InReplyTo   string       `json:"in_reply_to,omitempty"`
	Mode        MessageMode  `json:"mode,omitempty"` // default queue
	Body        MessageBody  `json:"body"`
	Attachments []Attachment `json:"attachments,omitempty"`
	TTLSeconds  int          `json:"ttl_seconds,omitempty"` // default retention
	RequestAck  bool         `json:"request_ack,omitempty"`
}

type ListMessagesRequest struct {
	PageRequest
	// Box selects inbox (recipient) or outbox (sender) for Address; or a
	// thread. Exactly one of Address or ThreadID.
	Box       string   `json:"box,omitempty"` // "inbox" | "outbox" | "all"
	Address   *Address `json:"address,omitempty"`
	ThreadID  string   `json:"thread_id,omitempty"`
	ProjectID *string  `json:"project_id,omitempty"`
	// Unacked filters to messages without explicit ack/reply.
	Unacked bool `json:"unacked,omitempty"`
	// RuntimeStates filters by runtime delivery state (e.g. indeterminate).
	RuntimeStates []RuntimeDeliveryState `json:"runtime_states,omitempty"`
	// AfterCursor returns messages after this inbox cursor (for awaiting).
	AfterCursor int64 `json:"after_cursor,omitempty"`
	// WaitSeconds long-polls up to this many seconds for a new match (<=60).
	WaitSeconds int `json:"wait_seconds,omitempty"`
}

// OpMarkMessagesRead (W-delivery, additive, optional): the calling session
// reports that it read these messages (their bodies were returned to it by
// get_message / inbox / wait_reply). The hub records EvidencePyreRead for
// each one addressed to that session whose runtime delivery is still
// pending and withdraws its paste delivery. Not part of API: callers
// type-assert MessageReadAPI; a hub without it answers 404 and the read is
// simply not recorded (the paste path stays as before).
const OpMarkMessagesRead = "messages.mark_read"

type MarkMessagesReadRequest struct {
	MessageIDs []string `json:"message_ids"`
	// Via names the read for the receipt detail (e.g. "mcp get_message").
	Via string `json:"via,omitempty"`
}

type MarkMessagesReadResponse struct {
	// Recorded lists the messages whose pending delivery the read resolved.
	Recorded []string `json:"recorded"`
}

// MessageReadAPI is implemented by hubs (and the client and node-local
// proxy) that offer OpMarkMessagesRead.
type MessageReadAPI interface {
	MarkMessagesRead(ctx context.Context, req MarkMessagesReadRequest) (MarkMessagesReadResponse, error)
}

type AckMessageRequest struct {
	Mutation
	MessageID string `json:"message_id"`
	Note      string `json:"note,omitempty"`
}

// ReissueMessageRequest explicitly re-sends uncertain/rejected work as a new
// message referencing the original; records who accepted duplication risk.
type ReissueMessageRequest struct {
	Mutation
	MessageID           string       `json:"message_id"`
	AcceptDuplicateRisk bool         `json:"accept_duplicate_risk"`
	Body                *MessageBody `json:"body,omitempty"` // default original body
}

// ResolveDeliveryRequest records a human/coordinator decision about an
// indeterminate delivery after inspecting evidence.
type ResolveDeliveryRequest struct {
	Mutation
	MessageID  string               `json:"message_id"`
	Resolution RuntimeDeliveryState `json:"resolution"` // accepted | rejected
	Note       string               `json:"note,omitempty"`
}

type CancelMessageRequest struct {
	Mutation
	MessageID string `json:"message_id"`
}
