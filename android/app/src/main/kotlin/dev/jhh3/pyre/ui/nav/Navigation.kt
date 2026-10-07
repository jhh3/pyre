package dev.jhh3.pyre.ui.nav

import androidx.compose.runtime.mutableStateListOf
import androidx.lifecycle.ViewModelStore
import androidx.lifecycle.ViewModelStoreOwner

/** Destinations. Deep links (`pyre://<kind>/<id>`) map onto these. */
sealed interface Screen {
    val key: String

    data object Sessions : Screen { override val key = "sessions" }
    data object Projects : Screen { override val key = "projects" }
    data object Machines : Screen { override val key = "machines" }
    data object Accounts : Screen { override val key = "accounts" }
    data object Activity : Screen { override val key = "activity" }
    data object Settings : Screen { override val key = "settings" }
    data object Launch : Screen { override val key = "launch" }
    data object AllArtifacts : Screen { override val key = "artifacts" }
    data class Pairing(val prefill: String? = null) : Screen { override val key = "pair" }
    data class SessionDetail(val sessionId: String) : Screen { override val key = "session/$sessionId" }
    data class Approval(val approvalId: String) : Screen { override val key = "approval/$approvalId" }
    data class Message(val messageId: String) : Screen { override val key = "message/$messageId" }
    data class Project(val projectId: String) : Screen { override val key = "project/$projectId" }
    data class Document(val documentId: String, val revisionId: String = "") : Screen { override val key = "document/$documentId/$revisionId" }
    data class Artifact(val artifactId: String = "", val versionId: String = "") : Screen { override val key = "artifact/$artifactId/$versionId" }
    data class Terminal(val sessionId: String) : Screen { override val key = "terminal/$sessionId" }
    data class HtmlPreview(val versionId: String) : Screen { override val key = "preview/$versionId" }
    data class Node(val nodeId: String) : Screen { override val key = "node/$nodeId" }
    data class Comment(val commentId: String) : Screen { override val key = "comment/$commentId" }
    /** An old inbox link (pyre://attention/<id>): resolves to the item's session, then replaces itself. */
    data class AttentionLink(val attentionId: String) : Screen { override val key = "attention/$attentionId" }

    /** The bottom-bar destinations: Sessions · Projects · Machines · Accounts. */
    val isRoot: Boolean get() = this == Sessions || this == Projects || this == Machines || this == Accounts
}

object DeepLinks {
    /**
     * Maps `pyre://<kind>/<id>` (contracts.DeepLink) to a screen. Pairing
     * links open a confirmation screen; they never pair automatically.
     */
    fun parse(raw: String?): Screen? {
        if (raw.isNullOrBlank()) return null
        // Kinds contain underscores (artifact_version), which java.net.URI does
        // not accept as a host, so parse the fixed shape directly.
        if (Regex("^pyre://pair([/?].*)?$", RegexOption.IGNORE_CASE).matches(raw.trim())) return Screen.Pairing(raw.trim())
        // An optional query (pyre://session/<id>?run=..&seq=.., a digest's
        // transcript moment) is accepted and ignored: it opens the object.
        val m = Regex("^pyre://([a-z_]+)/([A-Za-z0-9_.:-]+)/?(\\?[A-Za-z0-9_.:=&%-]*)?$").matchEntire(raw.trim()) ?: return null
        val kind = m.groupValues[1]
        val id = m.groupValues[2]
        if (id == "." || id == "..") return null
        return when (kind) {
            "session" -> Screen.SessionDetail(id)
            "run" -> null
            "approval" -> Screen.Approval(id)
            "message" -> Screen.Message(id)
            "project" -> Screen.Project(id)
            "document" -> Screen.Document(id)
            "document_revision" -> null // needs the document id; opened from its document
            "artifact" -> Screen.Artifact(artifactId = id)
            "artifact_version" -> Screen.Artifact(versionId = id)
            "node" -> Screen.Node(id)
            "comment" -> Screen.Comment(id)
            // There is no Inbox: an attention link opens the session it is about.
            "attention" -> if (id == "inbox") Screen.Sessions else Screen.AttentionLink(id)
            "inbox" -> Screen.Sessions
            else -> null
        }
    }
}

class NavEntry(val screen: Screen) : ViewModelStoreOwner {
    override val viewModelStore = ViewModelStore()
}

/** Minimal back stack; each entry owns its ViewModels (cleared on pop). Home is Sessions. */
class Navigator(start: Screen = Screen.Sessions) {
    val stack = mutableStateListOf(NavEntry(start))
    val current: NavEntry get() = stack.last()

    fun push(s: Screen) {
        if (current.screen == s) return
        stack.add(NavEntry(s))
    }

    /** Switch bottom-level tab, clearing detail screens. */
    fun root(s: Screen) {
        while (stack.isNotEmpty()) stack.removeAt(stack.lastIndex).viewModelStore.clear()
        stack.add(NavEntry(s))
    }

    /** Replace the current screen (a resolved link). */
    fun replace(s: Screen) {
        if (stack.size <= 1) return root(s)
        stack.removeAt(stack.lastIndex).viewModelStore.clear()
        push(s)
    }

    fun pop(): Boolean {
        if (stack.size <= 1) return false
        stack.removeAt(stack.lastIndex).viewModelStore.clear()
        return true
    }

    /** Replace everything (e.g. after unpair). */
    fun reset(s: Screen) = root(s)
}
