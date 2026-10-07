package dev.jhh3.pyre.model

import kotlinx.serialization.Serializable
import kotlinx.serialization.json.JsonElement

// Request bodies for `POST /v1/<op>`. PyreJson encodes with
// encodeDefaults=false and explicitNulls=false, which reproduces Go's
// `omitempty`: optional fields carry defaults and are omitted when unset.
// Fields that Go always encodes (no omitempty) have no default here, so the
// Kotlin compiler forces callers to supply them and they are always sent.
//
// `operationId` is the Mutation.operation_id: client-generated, stable across
// retries of the same logical intent (see core.OperationIds).

@Serializable
class EmptyRequest

@Serializable
data class HelloRequest(val protocol: Int, val client: String)

@Serializable
data class GetRequest(val id: String)

@Serializable
data class ListNodesRequest(
    val cursor: String = "",
    val limit: Int = 0,
    val includeRevoked: Boolean = false,
)

@Serializable
data class PairingExchange(
    val pairingId: String,
    val code: String,
    val clientName: String,
    val kind: String,
) {
    override fun toString(): String = "PairingExchange(pairingId=$pairingId, code=<redacted>, clientName=$clientName)"
}

/** clients.pair (owner-only): mints a one-time pairing ticket. Used by tests/tools. */
@Serializable
data class PairingRequest(
    val operationId: String,
    val correlationId: String = "",
    val clientName: String,
    val kind: String,
    val ttlSeconds: Int = 0,
)

@Serializable
data class PairingTicket(
    val pairingId: String,
    val code: String,
    val hubUrl: String = "",
    val hubCertSha256: String = "",
    val expiresAt: String = "",
    val uri: String = "",
) {
    override fun toString(): String = "PairingTicket(pairingId=$pairingId, code=<redacted>, uri=<redacted>)"
}

@Serializable
data class ListSessionsRequest(
    val cursor: String = "",
    val limit: Int = 0,
    val nodeId: String = "",
    val projectId: String? = null,
    val harness: String = "",
    val states: List<String> = emptyList(),
    val query: String = "",
    val includeArchived: Boolean = false,
)

@Serializable
data class AccountPolicy(
    val mode: String,
    val accountId: String = "",
    val prefer: List<String> = emptyList(),
)

@Serializable
data class NodeSelector(
    val platform: String = "",
    val labels: Map<String, String> = emptyMap(),
    val leastLoaded: Boolean = false,
)

@Serializable
data class CheckoutRequest(
    val mode: String,
    val repoId: String = "",
    val branch: String = "",
    val path: String = "",
)

@Serializable
data class StartSessionRequest(
    val operationId: String,
    val correlationId: String = "",
    val nodeId: String = "",
    val nodeSelector: NodeSelector? = null,
    val harness: String,
    val title: String = "",
    val cwd: String = "",
    val checkout: CheckoutRequest? = null,
    val projectId: String? = null,
    val envProfile: String = "",
    val accountPolicy: AccountPolicy,
    val initialPrompt: String = "",
    val args: List<String> = emptyList(),
    val tags: List<String> = emptyList(),
    val includeContext: Boolean = false,
    val parentSessionId: String = "",
)

@Serializable
data class ReadSessionRequest(
    val sessionId: String,
    val runId: String = "",
    val cursor: String = "",
    val limit: Int = 0,
    val tail: Boolean = false,
)

@Serializable
data class AttachRequest(
    val operationId: String,
    val correlationId: String = "",
    val sessionId: String,
    val clientNodeId: String = "",
    val preferred: List<String> = emptyList(),
    val readOnly: Boolean = false,
)

@Serializable
data class UpdateSessionRequest(
    val operationId: String,
    val sessionId: String,
    val title: String? = null,
    /** Replaces the whole tag list when set (read-modify-write; see domain/Pins). */
    val tags: List<String>? = null,
)

@Serializable
data class SessionCommandRequest(
    val operationId: String,
    val correlationId: String = "",
    val sessionId: String,
    val runId: String = "",
    val reason: String = "",
)

@Serializable
data class LeaseRequest(
    val operationId: String,
    val correlationId: String = "",
    val runId: String,
    val ttlSeconds: Int = 0,
    val takeover: Boolean = false,
)

@Serializable
data class ListApprovalsRequest(
    val cursor: String = "",
    val limit: Int = 0,
    val sessionId: String = "",
    val states: List<String> = emptyList(),
)

@Serializable
data class AnswerApprovalRequest(
    val operationId: String,
    val correlationId: String = "",
    val approvalId: String,
    val runId: String,
    val optionId: String = "",
    val text: String = "",
)

@Serializable
data class SendMessageRequest(
    val operationId: String,
    val correlationId: String = "",
    val messageId: String = "",
    val recipient: Address,
    val projectId: String? = null,
    val threadId: String = "",
    val inReplyTo: String = "",
    val mode: String = "",
    val body: MessageBody,
    val attachments: List<Attachment> = emptyList(),
    val ttlSeconds: Int = 0,
    val requestAck: Boolean = false,
)

@Serializable
data class ListMessagesRequest(
    val cursor: String = "",
    val limit: Int = 0,
    val box: String = "",
    val address: Address? = null,
    val threadId: String = "",
    val projectId: String? = null,
    val unacked: Boolean = false,
    val runtimeStates: List<String> = emptyList(),
    val afterCursor: Long = 0,
    val waitSeconds: Int = 0,
)

@Serializable
data class ReissueMessageRequest(
    val operationId: String,
    val correlationId: String = "",
    val messageId: String,
    val acceptDuplicateRisk: Boolean,
    val body: MessageBody? = null,
)

@Serializable
data class ResolveDeliveryRequest(
    val operationId: String,
    val correlationId: String = "",
    val messageId: String,
    val resolution: String,
    val note: String = "",
)

@Serializable
data class AckMessageRequest(
    val operationId: String,
    val correlationId: String = "",
    val messageId: String,
    val note: String = "",
)

@Serializable
data class CancelMessageRequest(
    val operationId: String,
    val correlationId: String = "",
    val messageId: String,
)

@Serializable
data class ListProjectsRequest(
    val cursor: String = "",
    val limit: Int = 0,
    val includeArchived: Boolean = false,
    val query: String = "",
)

@Serializable
data class GetDocumentRequest(
    val documentId: String = "",
    val projectId: String = "",
    val slug: String = "",
    val revisionId: String = "",
)

@Serializable
data class ListDocumentsRequest(
    val cursor: String = "",
    val limit: Int = 0,
    val projectId: String? = null,
)

@Serializable
data class ListRevisionsRequest(
    val cursor: String = "",
    val limit: Int = 0,
    val documentId: String,
)

@Serializable
data class ProposeRevisionRequest(
    val operationId: String,
    val correlationId: String = "",
    val documentId: String,
    val baseRevisionId: String,
    val content: String,
    val summary: String = "",
)

@Serializable
data class FinalizeUploadRequest(
    val operationId: String,
    val correlationId: String = "",
    val uploadId: String,
)

@Serializable
data class MintPreviewRequest(val versionId: String, val ttlSeconds: Int = 0)

@Serializable
data class ListArtifactsRequest(
    val cursor: String = "",
    val limit: Int = 0,
    val projectId: String? = null,
    val sessionId: String = "",
)

@Serializable
data class ListVersionsRequest(
    val cursor: String = "",
    val limit: Int = 0,
    val artifactId: String,
)

@Serializable
data class CreateCommentRequest(
    val operationId: String,
    val correlationId: String = "",
    val target: ObjectRef,
    val anchor: Anchor? = null,
    val body: String,
    val inReplyTo: String = "",
    val mentions: List<String> = emptyList(),
)

@Serializable
data class ListCommentsRequest(
    val cursor: String = "",
    val limit: Int = 0,
    val target: ObjectRef? = null,
    val artifactId: String = "",
    val documentId: String = "",
    val projectId: String? = null,
    val unresolved: Boolean = false,
)

@Serializable
data class ListAccountsRequest(
    val cursor: String = "",
    val limit: Int = 0,
    val provider: String = "",
)

@Serializable
data class ListAttentionRequest(
    val cursor: String = "",
    val limit: Int = 0,
    val states: List<String> = emptyList(),
    val kinds: List<String> = emptyList(),
    val projectId: String? = null,
    val sessionId: String = "",
)

@Serializable
data class UpdateAttentionRequest(
    val operationId: String,
    val correlationId: String = "",
    val attentionId: String,
    val state: String,
    val snoozedUntil: String? = null,
)

@Serializable
data class SetNotificationPrefsRequest(
    val operationId: String,
    val correlationId: String = "",
    val prefs: NotificationPrefs,
)

@Serializable
data class PushRegistration(
    val operationId: String,
    val correlationId: String = "",
    val provider: String,
    val deviceToken: String = "",
    val clientId: String = "",
) {
    override fun toString(): String = "PushRegistration(provider=$provider, deviceToken=<redacted>)"
}

@Serializable
data class ListEventsRequest(
    val afterCursor: Long,
    val limit: Int = 0,
    val kinds: List<String> = emptyList(),
    val sessionId: String = "",
    val projectId: String? = null,
    val waitSeconds: Int = 0,
)

@Serializable
data class NodeActionRequest(
    val operationId: String,
    val correlationId: String = "",
    val nodeId: String,
    val action: String,
    val params: JsonElement? = null,
    val timeoutSeconds: Int = 0,
)

@Serializable
data class DirListParams(val path: String = "")

@Serializable
data class AccountSelectParams(val provider: String, val policy: AccountPolicy)
