package dev.jhh3.pyre.model

/** RPC op names (internal/contracts/routes.go). */
object Ops {
    const val HELLO = "system.hello"
    const val WHOAMI = "system.whoami"
    const val LIST_NODES = "nodes.list"
    const val GET_NODE = "nodes.get"
    const val LIST_SESSIONS = "sessions.list"
    const val GET_SESSION = "sessions.get"
    const val START_SESSION = "sessions.start"
    const val READ_SESSION = "sessions.read"
    const val ATTACH_SESSION = "sessions.attach"
    const val INTERRUPT_SESSION = "sessions.interrupt"
    const val STOP_SESSION = "sessions.stop"
    const val UPDATE_SESSION = "sessions.update"
    const val ACQUIRE_LEASE = "leases.acquire"
    const val LIST_APPROVALS = "approvals.list"
    const val GET_APPROVAL = "approvals.get"
    const val ANSWER_APPROVAL = "approvals.answer"
    const val SEND_MESSAGE = "messages.send"
    const val GET_MESSAGE = "messages.get"
    const val LIST_MESSAGES = "messages.list"
    const val ACK_MESSAGE = "messages.ack"
    const val REISSUE_MESSAGE = "messages.reissue"
    const val RESOLVE_DELIVERY = "messages.resolve"
    const val CANCEL_MESSAGE = "messages.cancel"
    const val GET_PROJECT = "projects.get"
    const val LIST_PROJECTS = "projects.list"
    const val GET_DOCUMENT = "documents.get"
    const val LIST_DOCUMENTS = "documents.list"
    const val LIST_REVISIONS = "documents.revisions"
    const val PROPOSE_REVISION = "documents.propose"
    const val FINALIZE_UPLOAD = "artifacts.finalize"
    const val GET_ARTIFACT = "artifacts.get"
    const val LIST_ARTIFACTS = "artifacts.list"
    const val LIST_VERSIONS = "artifacts.versions"
    const val GET_VERSION = "artifacts.get_version"
    const val PREVIEW_URL = "artifacts.preview_url"
    const val CREATE_COMMENT = "comments.create"
    const val GET_COMMENT = "comments.get"
    const val LIST_COMMENTS = "comments.list"
    const val LIST_ACCOUNTS = "accounts.list"
    const val LIST_ATTENTION = "attention.list"
    const val GET_ATTENTION = "attention.get"
    const val UPDATE_ATTENTION = "attention.update"
    const val GET_NOTIFICATION_PREFS = "notifications.get_prefs"
    const val SET_NOTIFICATION_PREFS = "notifications.set_prefs"
    const val REGISTER_PUSH = "notifications.register"
    const val LIST_EVENTS = "events.list"
    const val INVOKE_NODE_ACTION = "nodes.action"
    const val LIST_CONNECTIONS = "connections.list"
}

/** Non-RPC paths (routes.go). */
object Paths {
    const val PAIR_EXCHANGE = "/v1/pair/exchange"
    const val EVENT_STREAM = "/v1/events/stream"
    const val TERMINAL = "/v1/terminal"
    const val HEALTH = "/healthz"
    fun content(versionId: String) = "/v1/content/$versionId"
}

object Protocol {
    const val VERSION = 1
    const val HEADER = "X-Pyre-Protocol"
}

object ErrorCodes {
    const val INVALID = "invalid_argument"
    const val UNAUTHORIZED = "unauthorized"
    const val FORBIDDEN = "forbidden"
    const val NOT_FOUND = "not_found"
    const val CONFLICT = "conflict"
    const val EXPIRED = "expired"
    const val RESET_REQUIRED = "reset_required"
    const val UNSUPPORTED = "unsupported"
    const val CAPACITY_EXHAUSTED = "capacity_exhausted"
    const val UNAVAILABLE = "unavailable"
    const val OUTCOME_INDETERMINATE = "outcome_indeterminate"
    const val PROTOCOL_MISMATCH = "protocol_mismatch"
    const val PAYLOAD_TOO_LARGE = "payload_too_large"
    const val INTERNAL = "internal"
}

object ConnectionState {
    const val CONNECTED = "connected"
    const val DISCONNECTED = "disconnected"
    const val NEVER = "never"
    const val REVOKED = "revoked"
}

/** Node labels set by the hub (fleet.go). */
object NodeLabels {
    const val ENROLLMENT = "pyre.enrollment"
    const val ENROLLMENT_PENDING = "pending"
}

/** Account binding mechanisms (accounts.go CredentialKind). Unknown values are tolerated. */
object CredentialKinds {
    const val CLAUDE_PROFILE_STORE = "claude-profile-store"
    const val CLAUDE_TOKEN_FD = "claude-oauth-token-fd"
    const val CODEX_HOME_FILE = "codex-home-file"
    const val AMBIENT = "ambient"
    const val OAUTH_TOKEN = "oauth-token"
    const val ROTA_CHECKOUT = "rota-checkout"

    fun label(kind: String): String = when (kind) {
        CLAUDE_PROFILE_STORE -> "isolated Claude profile"
        CLAUDE_TOKEN_FD -> "Claude setup token (no refresh)"
        CODEX_HOME_FILE -> "isolated Codex home (1 process)"
        AMBIENT -> "ambient default login (no isolation)"
        OAUTH_TOKEN -> "token held by the hub (every machine at once)"
        ROTA_CHECKOUT -> "rota login (one machine at a time)"
        "" -> "—"
        else -> "$kind (unrecognized)"
    }
}

object RunState {
    const val STARTING = "starting"
    const val RUNNING = "running"
    const val WAITING_INPUT = "waiting_input"
    const val WAITING_APPROVAL = "waiting_approval"
    const val EXITING = "exiting"
    const val EXITED = "exited"
    const val FAILED = "failed"
    const val UNKNOWN = "unknown"
    /** Idle at the prompt, but background tasks/subagents may start a turn on their own. */
    const val WAITING_BACKGROUND = "waiting_on_background"
}

object ApprovalState {
    const val PENDING = "pending"
    const val RESOLVED = "resolved"
    const val EXPIRED = "expired"
    const val WITHDRAWN = "withdrawn"
}

object ApprovalKind {
    const val PERMISSION = "permission"
    const val QUESTION = "question"
    const val PLAN = "plan"
}

object MessageMode {
    const val QUEUE = "queue"
    const val STEER = "steer"
    const val INFO = "info"
}

object StorageState {
    const val PENDING_LOCAL = "pending_local"
    const val STORED = "stored"
    const val REJECTED = "rejected"
}

object DeliveryState {
    const val NOT_APPLICABLE = "not_applicable"
    const val QUEUED = "queued"
    const val NODE_RECEIVED = "node_received"
    const val ATTEMPTING = "attempting"
    const val ACCEPTED = "accepted"
    const val REJECTED = "rejected"
    const val INDETERMINATE = "indeterminate"
    const val EXPIRED = "expired"
    const val CANCELLED = "cancelled"
}

object AckState {
    const val NONE = "none"
    const val ACKNOWLEDGED = "acknowledged"
    const val REPLIED = "replied"
}

object AttachMechanism {
    const val LOCAL_TMUX = "local_tmux"
    const val SSH_TMUX = "ssh_tmux"
    const val WEB_TERMINAL = "web_terminal"
    const val READ_ONLY = "read_only"
}

object AttentionState {
    const val OPEN = "open"
    const val READ = "read"
    const val HANDLED = "handled"
    const val WITHDRAWN = "withdrawn"
    const val SNOOZED = "snoozed"
}

object AttentionKind {
    const val APPROVAL = "approval"
    const val QUESTION = "question"
    const val FAILURE = "failure"
    const val ACCOUNT_EXHAUSTED = "account_exhausted"
    const val REVIEW_READY = "review_ready"
    const val MESSAGE = "message"
    const val INDETERMINATE = "indeterminate_delivery"
    const val CONFLICT = "document_conflict"
    const val COMMENT = "comment"
    const val UPDATE = "update"
    const val NODE = "node"
    /** A session flagged itself ("I need you"). */
    const val FLAG = "flag"
    /** An open ask (AsksOps); handled when the ask is decided. */
    const val ASK = "ask"
}

object Eligibility {
    const val ELIGIBLE = "eligible"
    const val COOLDOWN = "cooldown"
    const val EXHAUSTED = "exhausted"
    const val UNUSABLE = "unusable"
    const val UNKNOWN = "unknown"
}

object AccountPolicyMode {
    const val ELIGIBLE = "eligible"
    const val EXPLICIT = "explicit"
    const val AMBIENT = "ambient"
}

object Harness {
    const val CLAUDE = "claude"
    const val CODEX = "codex"
    const val SHELL = "shell"
    const val MOCK = "mock"

    fun provider(h: String): String = when (h) {
        CLAUDE -> "anthropic"
        CODEX -> "openai"
        else -> ""
    }
}

/** FCM data-only push keys (contracts.PushDataKeys). */
object PushKeys {
    const val ATTENTION_ID = "attention_id"
    const val KIND = "kind"
    const val SEVERITY = "severity"
    const val DEEP_LINK = "deep_link"
    const val GROUP_KEY = "group_key"
    const val ACTION = "action"
    const val TITLE = "title"
    const val ACTION_NOTIFY = "notify"
    const val ACTION_DISMISS = "dismiss"
}

object EventKinds {
    const val ATTENTION_CHANGED = "attention.changed"
    const val APPROVAL_RESOLVED = "approval.resolved"
    const val APPROVAL_WITHDRAWN = "approval.withdrawn"
    const val APPROVAL_REQUESTED = "approval.requested"
    /** Every ask change (payload: Ask). */
    const val ASK_CHANGED = "ask.changed"
}

object NodeActions {
    const val DIR_LIST = "dir.list"
    const val ACCOUNT_SELECT = "account.select"
}

/** Runtime adapter capability strings (runtime.go). */
object Caps {
    const val ATTACH_NATIVE = "attach.native_terminal"
    const val ATTACH_WEB = "attach.web_terminal"
    const val ATTACH_READ_ONLY = "attach.read_only"
    const val INPUT_QUEUE = "input.queue"
    const val INPUT_STEER = "input.steer"
    const val INPUT_IDEMPOTENT = "input.idempotent"
    const val INPUT_RECONCILE = "input.reconcile"
    const val APPROVALS = "approvals.answer"
    const val QUESTIONS = "questions.answer"
    const val INTERRUPT = "interrupt"
    const val STOP = "stop"
    const val TRANSCRIPT = "transcript.read"
    const val SCREEN = "screen.read"
}

object CommandOutcome {
    const val PENDING = "pending"
    const val DELIVERED = "delivered"
    const val SUCCEEDED = "succeeded"
    const val FAILED = "failed"
    const val UNSUPPORTED = "unsupported"
    const val INDETERMINATE = "indeterminate"
}
