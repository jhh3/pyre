package dev.jhh3.pyre.testing

import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.model.*
import java.io.File
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.JsonElement

/** Test-side mirror of contracts.FixtureBundle (fixtures.go). */
@Serializable
data class FixtureBundle(
    val schemaVersion: Int,
    val name: String,
    val description: String = "",
    val synthetic: Boolean,
    val requirements: List<String> = emptyList(),
    val cases: List<String> = emptyList(),
    val nodes: List<Node> = emptyList(),
    val sessions: List<SessionView> = emptyList(),
    val runs: List<Run> = emptyList(),
    val messages: List<MessageView> = emptyList(),
    val approvals: List<Approval> = emptyList(),
    val leases: List<InputLease> = emptyList(),
    val projects: List<Project> = emptyList(),
    val documents: List<Document> = emptyList(),
    val revisions: List<DocumentRevision> = emptyList(),
    val proposals: List<DocumentProposal> = emptyList(),
    val artifacts: List<Artifact> = emptyList(),
    val versions: List<ArtifactVersion> = emptyList(),
    val comments: List<ReviewComment> = emptyList(),
    val notes: List<Note> = emptyList(),
    val accounts: List<AccountView> = emptyList(),
    val observations: List<AccountObservation> = emptyList(),
    val fallbacks: List<FallbackPlan> = emptyList(),
    val attention: List<AttentionItem> = emptyList(),
    val events: List<Event> = emptyList(),
    val environments: List<EnvironmentGeneration> = emptyList(),
    val tools: List<ToolVersion> = emptyList(),
    val drift: List<DriftItem> = emptyList(),
    val plans: List<ChangePlan> = emptyList(),
    val connections: List<Connection> = emptyList(),
    val exchanges: List<FixtureExchange> = emptyList(),
    val contents: Map<String, String> = emptyMap(),
)

@Serializable
data class FixtureExchange(
    val name: String,
    val op: String,
    val principal: Principal? = null,
    val request: JsonElement,
    val response: JsonElement? = null,
    val error: ApiError? = null,
    val explanation: String = "",
)

object Fixtures {
    val dir: File by lazy {
        val p = System.getProperty("pyre.fixtures.dir") ?: "../../tests/fixtures/contracts"
        File(p).also { require(it.isDirectory) { "fixtures dir not found: $p" } }
    }

    fun files(): List<File> = dir.listFiles { f -> f.name.endsWith(".json") }!!.sortedBy { it.name }

    fun text(prefix: String): String = files().first { it.name.startsWith(prefix) }.readText()

    /** Strict: any key the Kotlin models lack fails the decode. */
    fun bundle(prefix: String): FixtureBundle = PyreJson.strict.decodeFromString(FixtureBundle.serializer(), text(prefix))
}
