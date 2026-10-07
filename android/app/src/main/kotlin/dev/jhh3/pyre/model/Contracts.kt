package dev.jhh3.pyre.model

import kotlinx.serialization.Serializable
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject

// Kotlin mirror of internal/contracts (protocol v1). Property names are
// camelCase and mapped to the wire's snake_case by PyreJson's naming strategy.
//
// Nullability follows the Go types: pointer fields (`*string`, `*time.Time`,
// `*Run`, ...) are nullable; value fields have defaults so that `omitempty`
// omissions and Go's `null` encoding of nil slices/maps decode cleanly.
// Enum-like values are Strings (constants in Constants.kt) so a newer hub's
// additional states decode and are displayed honestly instead of crashing.
// Timestamps are RFC 3339 strings; parse with core.Times.

@Serializable
data class ObjectRef(val kind: String, val id: String)

@Serializable
data class Principal(
    val kind: String,
    val id: String,
    val name: String = "",
    val onBehalfOf: Principal? = null,
)

@Serializable
data class Freshness(
    val observedAt: String = "",
    val source: String = "",
    val stale: Boolean = false,
    val reason: String = "",
)

@Serializable
data class Page<T>(
    val items: List<T> = emptyList(),
    val nextCursor: String = "",
    val snapshotAt: String = "",
)

@Serializable
data class ApiError(
    val code: String,
    val message: String = "",
    val retryable: Boolean = false,
    val ref: ObjectRef? = null,
    val details: JsonObject? = null,
)

@Serializable
data class ErrorBody(val error: ApiError? = null)

// ---- system ----

@Serializable
data class HelloResponse(
    val protocol: Int,
    val minProtocol: Int = 0,
    val hubId: String = "",
    val hubVersion: String = "",
    val serverTime: String = "",
    val principal: Principal? = null,
    val mock: Boolean = false,
    val features: List<String> = emptyList(),
)

// ---- fleet ----

@Serializable
data class Node(
    val nodeId: String,
    val name: String = "",
    val hostname: String = "",
    val sshAlias: String = "",
    val platform: String = "",
    val arch: String = "",
    val harnesses: List<HarnessInfo> = emptyList(),
    val versions: Map<String, String> = emptyMap(),
    val capabilities: List<String> = emptyList(),
    val connection: NodeConnection = NodeConnection(),
    val resources: NodeResources = NodeResources(),
    val enrolledAt: String = "",
    val labels: Map<String, String> = emptyMap(),
)

@Serializable
data class HarnessInfo(
    val harness: String,
    val version: String = "",
    val executable: String = "",
    val available: Boolean = false,
    val reason: String = "",
    val capabilities: List<String> = emptyList(),
    val probedAt: String = "",
    /** The machine's own login for this harness (identity only, never a credential); null when none. */
    val login: HarnessLogin? = null,
)

@Serializable
data class HarnessLogin(
    val email: String = "",
    val providerRef: String = "",
    val plan: String = "",
)

@Serializable
data class NodeConnection(
    val state: String = ConnectionState.NEVER,
    val lastSeenAt: String? = null,
    val connectedSince: String? = null,
    val remoteAddr: String = "",
    val protocol: Int = 0,
)

@Serializable
data class NodeResources(
    val cpuCount: Int = 0,
    val load1: Double = 0.0,
    val memTotalMb: Long = 0,
    val memAvailMb: Long = 0,
    val activeRuns: Int = 0,
    val maxRuns: Int = 0,
    val pyreRssMb: Double = 0.0,
    val pyreCpuPct: Double = 0.0,
    /** Supervised harness processes (agent/model work, not Pyre overhead). */
    val agentRssMb: Double = 0.0,
    val agentCpuPct: Double = 0.0,
    val observedAt: String = "",
    // Machine stats (contract v1.8; zero/empty = not reported).
    val cpuPct: Double = 0.0,
    val load5: Double = 0.0,
    val load15: Double = 0.0,
    val swapTotalMb: Long = 0,
    val swapUsedMb: Long = 0,
    /** "normal" | "warn" | "critical" | "" (unknown). */
    val memPressure: String = "",
    val memPressureSome10: Double = 0.0,
    val disks: List<DiskUsage> = emptyList(),
    val runs: List<RunUsage> = emptyList(),
)

@Serializable
data class DiskUsage(
    val path: String = "",
    val roles: List<String> = emptyList(),
    val totalMb: Long = 0,
    val freeMb: Long = 0,
)

/** One run's process tree: summed RSS and CPU % of one core. */
@Serializable
data class RunUsage(
    val runId: String,
    val sessionId: String = "",
    val procs: Int = 0,
    val rssMb: Double = 0.0,
    val cpuPct: Double = 0.0,
    val top: ProcUsage? = null,
    val observedAt: String = "",
)

@Serializable
data class ProcUsage(val pid: Int = 0, val name: String = "", val rssMb: Double = 0.0)

@Serializable
data class ClientCredential(val clientId: String, val token: String) {
    override fun toString(): String = "ClientCredential(clientId=$clientId, token=<redacted>)"
}

@Serializable
data class ClientInfo(
    val clientId: String,
    val name: String = "",
    val kind: String = "",
    val createdAt: String = "",
    val lastSeenAt: String? = null,
    val revoked: Boolean = false,
)

// ---- sessions / runs ----

@Serializable
data class Session(
    val sessionId: String,
    val harness: String = "",
    val title: String = "",
    val projectId: String? = null,
    val createdBy: Principal? = null,
    val createdAt: String = "",
    val currentRunId: String = "",
    val lineage: Lineage = Lineage(),
    val tags: List<String> = emptyList(),
    val archived: Boolean = false,
)

@Serializable
data class Lineage(
    val parentSessionId: String = "",
    val forkedFrom: String = "",
    val handoffNoteId: String = "",
)

@Serializable
data class Run(
    val runId: String,
    val sessionId: String,
    val nodeId: String = "",
    val adapter: String = "",
    val harness: String = "",
    val cwd: String = "",
    val checkout: CheckoutBinding? = null,
    val runtime: RuntimeVersion = RuntimeVersion(),
    val envGenerationId: String = "",
    val accountId: String = "",
    val assignmentId: String = "",
    val state: String = RunState.UNKNOWN,
    val stateReason: String = "",
    val freshness: Freshness = Freshness(),
    val startedAt: String = "",
    val endedAt: String? = null,
    val exitCode: Int? = null,
    val nativeRef: NativeRef? = null,
    val capabilities: List<String> = emptyList(),
    val previousRunId: String = "",
    val continuationKind: String = "",
    val subagents: List<SubagentObservation> = emptyList(),
    val lease: InputLease? = null,
    val pendingApprovalIds: List<String> = emptyList(),
    val adapterData: Map<String, String> = emptyMap(),
    /** Latest process-tree sample, filled by the hub for live runs (v1.8). */
    val usage: RunUsage? = null,
    /** What a working run is doing now ("running tests"), set only while running (v1.9). */
    val activity: RunActivity? = null,
)

@Serializable
data class RunActivity(val text: String = "", val since: String = "")

@Serializable
data class RuntimeVersion(
    val harness: String = "",
    val version: String = "",
    val executable: String = "",
    val digest: String = "",
)

@Serializable
data class NativeRef(val kind: String, val id: String)

@Serializable
data class SubagentObservation(
    val id: String,
    val kind: String = "",
    val state: String = "",
    val startedAt: String? = null,
    val endedAt: String? = null,
    val summary: String = "",
)

@Serializable
data class CheckoutBinding(
    val nodeId: String = "",
    val path: String = "",
    val repoId: String = "",
    val mode: String = "",
    val branch: String = "",
)

@Serializable
data class SessionView(
    val session: Session,
    val run: Run? = null,
    val nodeName: String = "",
    val attention: Int = 0,
    /** Why the session wants the owner (v1.3), null when nothing does. Blocking reasons outrank messages. */
    val flag: SessionFlag? = null,
    val freshness: Freshness = Freshness(),
)

/** contracts.SessionFlag: level needs_you (blocking) or message (informational). */
@Serializable
data class SessionFlag(
    val level: String = "",
    val reason: String = "",
    val text: String = "",
    val attentionId: String = "",
    val kind: String = "",
    val count: Int = 0,
    val messages: Int = 0,
    val since: String = "",
)

@Serializable
data class StartSessionResponse(
    val session: SessionView,
    val commandId: String = "",
)

@Serializable
data class TranscriptEntry(
    val seq: Long,
    val runId: String = "",
    val at: String = "",
    val kind: String = "",
    val text: String = "",
    val messageId: String = "",
    val truncated: Boolean = false,
)

@Serializable
data class ReadSessionResponse(
    val entries: List<TranscriptEntry> = emptyList(),
    val nextCursor: String = "",
    val gap: Boolean = false,
    val freshness: Freshness = Freshness(),
    val screen: String = "",
)

@Serializable
data class AttachDescriptor(
    val runId: String = "",
    val nodeId: String = "",
    val mechanism: String,
    val command: List<String> = emptyList(),
    val url: String = "",
    val ticket: String = "",
    val expiresAt: String = "",
    val readOnly: Boolean = false,
    val notes: String = "",
    val lease: InputLease? = null,
) {
    // The ticket is a one-time credential; keep it out of logs/crash reports.
    override fun toString(): String =
        "AttachDescriptor(runId=$runId, mechanism=$mechanism, url=$url, ticket=<redacted>, readOnly=$readOnly)"
}

@Serializable
data class CommandReceipt(
    val commandId: String = "",
    val accepted: Boolean = false,
    val outcome: String = "",
    val detail: String = "",
    val observedAt: String = "",
)

// ---- interaction ----

@Serializable
data class InputLease(
    val leaseId: String,
    val runId: String = "",
    val holder: Principal? = null,
    val acquiredAt: String = "",
    val renewedAt: String = "",
    val expiresAt: String = "",
    val takeoverOf: String = "",
    val releasedAt: String? = null,
)

@Serializable
data class Approval(
    val approvalId: String,
    val sessionId: String = "",
    val runId: String = "",
    val requestId: String = "",
    val kind: String = "",
    val title: String = "",
    val body: String = "",
    val options: List<ApprovalOption> = emptyList(),
    val allowText: Boolean = false,
    val createdAt: String = "",
    val expiresAt: String? = null,
    val state: String = ApprovalState.PENDING,
    val resolution: ApprovalResolution? = null,
)

@Serializable
data class ApprovalOption(val id: String, val label: String = "", val kind: String = "")

@Serializable
data class ApprovalResolution(
    val optionId: String = "",
    val text: String = "",
    val actor: Principal? = null,
    val resolvedAt: String = "",
    val source: String = "",
    /** "submitted" | "effective" | "superseded" (unknown values tolerated). */
    val effect: String = "",
    /** e.g. "answered_in_terminal", "interrupted", "run_replaced", "timeout". */
    val reason: String = "",
)

// ---- messaging ----

@Serializable
data class Address(val kind: String, val id: String)

@Serializable
data class MessageBody(val text: String = "")

@Serializable
data class Attachment(
    val artifactVersionId: String = "",
    val documentRevisionId: String = "",
    val noteId: String = "",
)

@Serializable
data class MessageEnvelope(
    val schemaVersion: Int = 1,
    val messageId: String,
    val operationId: String = "",
    val sender: Principal? = null,
    val recipient: Address,
    val projectId: String? = null,
    val threadId: String = "",
    val inReplyTo: String = "",
    val reissues: String = "",
    val mode: String = MessageMode.QUEUE,
    val body: MessageBody = MessageBody(),
    val attachments: List<Attachment> = emptyList(),
    val createdAt: String = "",
    val expiresAt: String? = null,
    val requestAck: Boolean = false,
)

@Serializable
data class DeliveryStatus(
    val storage: String = StorageState.PENDING_LOCAL,
    val storedAt: String? = null,
    val runtime: String = DeliveryState.QUEUED,
    val runtimeReason: String = "",
    val boundRunId: String = "",
    val ack: String = AckState.NONE,
    val ackRef: String = "",
    /** Evidence that the runtime actually took the accepted input into a turn. */
    val consumedAt: String? = null,
    val outcomeRef: ObjectRef? = null,
    val receipts: List<DeliveryReceipt> = emptyList(),
    val updatedAt: String = "",
)

@Serializable
data class DeliveryReceipt(
    val messageId: String = "",
    val attemptId: String = "",
    val sessionId: String = "",
    val runId: String = "",
    val nodeId: String = "",
    val state: String = "",
    val evidence: String = "",
    val evidenceRef: String = "",
    val detail: String = "",
    val observedAt: String = "",
    val originSeq: Long = 0,
    val consumed: Boolean = false,
)

@Serializable
data class MessageView(
    val envelope: MessageEnvelope,
    val status: DeliveryStatus = DeliveryStatus(),
    val cursor: Long = 0,
)

// ---- projects / documents / artifacts / reviews ----

@Serializable
data class Project(
    val projectId: String,
    val name: String = "",
    val description: String = "",
    val briefDocumentId: String = "",
    val repos: List<RepoBinding> = emptyList(),
    val checkouts: List<CheckoutBinding> = emptyList(),
    val links: List<Link> = emptyList(),
    val envProfile: String = "",
    val connections: List<String> = emptyList(),
    val archived: Boolean = false,
    val version: Long = 0,
    val createdBy: Principal? = null,
    val createdAt: String = "",
    val updatedAt: String = "",
)

@Serializable
data class RepoBinding(val repoId: String, val name: String = "", val url: String = "")

@Serializable
data class Link(val title: String = "", val url: String = "")

@Serializable
data class Document(
    val documentId: String,
    val projectId: String? = null,
    val title: String = "",
    val slug: String = "",
    val mediaType: String = "text/markdown",
    val headRevisionId: String = "",
    val headSeq: Long = 0,
    val createdBy: Principal? = null,
    val createdAt: String = "",
    val updatedAt: String = "",
)

@Serializable
data class DocumentRevision(
    val revisionId: String,
    val documentId: String = "",
    val seq: Long = 0,
    val parentRevisionId: String = "",
    val digest: String = "",
    val size: Long = 0,
    val author: Principal? = null,
    val createdAt: String = "",
    val summary: String = "",
    val content: String = "",
    val provenance: Provenance? = null,
)

@Serializable
data class DocumentProposal(
    val proposalId: String,
    val documentId: String = "",
    val baseRevisionId: String = "",
    val content: String = "",
    val author: Principal? = null,
    val createdAt: String = "",
    val state: String = "",
    val headAtConflict: String = "",
)

@Serializable
data class DocumentWithRevision(val document: Document, val revision: DocumentRevision)

@Serializable
data class Provenance(
    val sessionId: String = "",
    val runId: String = "",
    val nodeId: String = "",
    val projectId: String? = null,
    val sourcePath: String = "",
    val commit: String = "",
)

@Serializable
data class Artifact(
    val artifactId: String,
    val projectId: String? = null,
    val sessionId: String = "",
    val title: String = "",
    val description: String = "",
    val latestVersionId: String = "",
    val versionCount: Int = 0,
    val createdBy: Principal? = null,
    val createdAt: String = "",
    val updatedAt: String = "",
)

@Serializable
data class ArtifactVersion(
    val versionId: String,
    val artifactId: String = "",
    val seq: Long = 0,
    val previousVersionId: String = "",
    val digest: String = "",
    val size: Long = 0,
    val mediaType: String = "application/octet-stream",
    val filename: String = "",
    val author: Principal? = null,
    val provenance: Provenance = Provenance(),
    val createdAt: String = "",
    val note: String = "",
    val reviewRequested: Boolean = false,
    val contentUrl: String = "",
    val previewUrl: String = "",
)

/** artifacts.preview_url (v1.2): one-time isolated preview link. */
@Serializable
data class PreviewUrl(val url: String, val expiresAt: String = "") {
    override fun toString(): String = "PreviewUrl(url=<redacted>, expiresAt=$expiresAt)"
}

@Serializable
data class PublishReceipt(val artifact: Artifact, val version: ArtifactVersion)

@Serializable
data class ReviewComment(
    val commentId: String,
    val target: ObjectRef,
    val anchor: Anchor? = null,
    val author: Principal? = null,
    val body: String = "",
    val inReplyTo: String = "",
    val mentions: List<String> = emptyList(),
    val resolved: Boolean = false,
    val resolvedBy: Principal? = null,
    val createdAt: String = "",
    val updatedAt: String = "",
    val projectId: String? = null,
)

@Serializable
data class Anchor(val kind: String, val value: String)

@Serializable
data class Note(
    val noteId: String,
    val kind: String = "",
    val projectId: String? = null,
    val sessionId: String = "",
    val title: String = "",
    val body: String = "",
    val author: Principal? = null,
    val createdAt: String = "",
    val provenance: Provenance = Provenance(),
    val hiveRef: String = "",
    val syncState: String = "",
    val promotedTo: String = "",
)

// ---- accounts ----

@Serializable
data class Account(
    val accountId: String,
    val provider: String = "",
    val label: String = "",
    val kind: String = "",
    val disabled: Boolean = false,
    val createdAt: String = "",
    val rotaRef: String = "",
    val providerRef: String = "",
    val plan: String = "",
)

@Serializable
data class DeviceGrant(
    val grantId: String,
    val accountId: String = "",
    val nodeId: String = "",
    val harness: String = "",
    val profileRef: String = "",
    val mechanism: String = "",
    val kind: String = "",
    val profileDir: String = "",
    val maxProcs: Int = 0,
    val state: String = "",
    val refreshOwner: String = "",
    val generation: Long = 0,
    val observedAt: String = "",
    val detail: String = "",
)

@Serializable
data class AccountAssignment(
    val assignmentId: String,
    val accountId: String = "",
    val grantId: String = "",
    /** See CredentialKinds; unknown values are displayed as-is. */
    val credentialKind: String = "",
    val grantGeneration: Long = 0,
    val lease: String = "",
    val runId: String = "",
    val sessionId: String = "",
    val nodeId: String = "",
    val reason: String = "",
    val assignedAt: String = "",
    val releasedAt: String? = null,
    val freshness: Freshness = Freshness(),
)

@Serializable
data class AccountObservation(
    val accountId: String,
    val provider: String = "",
    val reportingNode: String = "",
    val observedAt: String = "",
    val source: String = "",
    val confidence: String = "",
    val eligibility: String = Eligibility.UNKNOWN,
    val windows: List<UsageWindow> = emptyList(),
    val resetAt: String? = null,
    val stale: Boolean = false,
    val detail: String = "",
)

@Serializable
data class UsageWindow(
    val name: String,
    val usedPercent: Double = 0.0,
    val resetAt: String? = null,
)

@Serializable
data class AccountView(
    val account: Account,
    val latest: AccountObservation? = null,
    /** Newest observation from each reporting node (ordered by node id); [latest] is whichever reported last. */
    val byNode: List<AccountObservation> = emptyList(),
    val grants: List<DeviceGrant> = emptyList(),
    val activeAssignments: List<AccountAssignment> = emptyList(),
)

@Serializable
data class FallbackPlan(
    val sessionId: String,
    val fromRunId: String = "",
    val fromAccountId: String = "",
    val outcome: String = "",
    val toAccountId: String = "",
    val newRunId: String = "",
    val reason: String = "",
    val uncertainWork: Boolean = false,
    val provider: String = "",
    val plannedAt: String = "",
)

@Serializable
data class AccountSelection(
    val accountId: String = "",
    val provider: String = "",
    val grantId: String = "",
    val reason: String = "",
    val freshness: Freshness = Freshness(),
    val outcome: String = "",
)

// ---- attention / events / notifications ----

@Serializable
data class Event(
    val eventId: String,
    val cursor: Long,
    val origin: String = "",
    val originSeq: Long = 0,
    val kind: String,
    val `object`: ObjectRef,
    val projectId: String? = null,
    val sessionId: String = "",
    val payloadVersion: Int = 0,
    val payload: JsonElement? = null,
    val occurredAt: String = "",
    val ingestedAt: String = "",
)

@Serializable
data class ListEventsResponse(
    val events: List<Event> = emptyList(),
    val nextCursor: Long = 0,
    val headCursor: Long = 0,
)

@Serializable
data class AttentionItem(
    val attentionId: String,
    val kind: String = "",
    val severity: String = "normal",
    val groupKey: String = "",
    val `object`: ObjectRef,
    val sessionId: String = "",
    val projectId: String? = null,
    val title: String = "",
    val summary: String = "",
    val state: String = AttentionState.OPEN,
    val count: Int = 1,
    val createdAt: String = "",
    val updatedAt: String = "",
    val snoozedUntil: String? = null,
    val deepLink: String = "",
    val notification: NotificationStatus? = null,
)

@Serializable
data class NotificationStatus(
    val provider: String = "",
    val state: String = "",
    val sentAt: String? = null,
    val detail: String = "",
)

@Serializable
data class NotificationPrefs(
    val quietHoursStart: String = "",
    val quietHoursEnd: String = "",
    val tz: String = "",
    val mutedProjects: List<String> = emptyList(),
    val minSeverity: String = "",
    val kinds: Map<String, Boolean> = emptyMap(),
)

@Serializable
data class PushRegistrationResult(
    val provider: String = "",
    val enabled: Boolean = false,
    val detail: String = "",
)

// ---- node actions / environment ----

@Serializable
data class NodeActionResult(
    val commandId: String = "",
    val outcome: String = "",
    val result: JsonElement? = null,
    val error: ApiError? = null,
)

@Serializable
data class DirListResult(
    val path: String = "",
    val dirs: List<String> = emptyList(),
    val isGit: Boolean = false,
)

@Serializable
data class ToolVersion(
    val tool: String,
    val version: String = "",
    val digest: String = "",
    val path: String = "",
    val state: String = "",
    val managed: Boolean = false,
    val installedAt: String = "",
    val health: String = "",
    val inUseByRuns: List<String> = emptyList(),
)

@Serializable
data class ExtensionRef(
    val name: String,
    val kind: String = "",
    val harness: String = "",
    val version: String = "",
    val digest: String = "",
    val layer: String = "",
    val managed: Boolean = false,
    val source: String = "",
)

@Serializable
data class ResolvedValue(
    val key: String,
    val value: String = "",
    val layer: String = "",
    val overrode: List<String> = emptyList(),
    val restartRequired: Boolean = false,
)

@Serializable
data class EnvironmentGeneration(
    val generationId: String,
    val nodeId: String = "",
    val profile: String = "",
    val projectId: String? = null,
    val createdAt: String = "",
    val runtimeVersions: Map<String, String> = emptyMap(),
    val configDigests: Map<String, String> = emptyMap(),
    val skills: List<ExtensionRef> = emptyList(),
    val plugins: List<ExtensionRef> = emptyList(),
    val mcpServers: List<ExtensionRef> = emptyList(),
    val connectionGrants: List<String> = emptyList(),
    val values: List<ResolvedValue> = emptyList(),
    val digest: String = "",
    val previousId: String = "",
)

@Serializable
data class DriftItem(
    val path: String,
    val key: String = "",
    val kind: String = "",
    val expected: String = "",
    val actual: String = "",
    val detail: String = "",
)

@Serializable
data class PlanChange(
    val path: String,
    val action: String = "",
    val diff: String = "",
    val backup: String = "",
)

@Serializable
data class ChangePlan(
    val planId: String,
    val nodeId: String = "",
    val scope: String = "",
    val changes: List<PlanChange> = emptyList(),
    val conflicts: List<DriftItem> = emptyList(),
    val restartRequired: List<String> = emptyList(),
    val resultDigest: String = "",
)

@Serializable
data class Connection(
    val connectionId: String,
    val name: String = "",
    val service: String = "",
    val modes: List<String> = emptyList(),
    val availability: List<ConnectionAvailability> = emptyList(),
    val scope: ConnectionScope = ConnectionScope(),
    val generation: Long = 0,
)

@Serializable
data class ConnectionAvailability(
    val nodeId: String,
    val state: String = "",
    val mode: String = "",
    val verifiedAt: String = "",
    val detail: String = "",
)

@Serializable
data class ConnectionScope(
    /** "broad" | "scoped" | "none" (unknown values tolerated). */
    val access: String = "",
    val nodes: List<String> = emptyList(),
    val projects: List<String> = emptyList(),
    val sessions: List<String> = emptyList(),
)
