package dev.jhh3.pyre.model

import kotlinx.serialization.Serializable

/** Repo index (internal/contracts/repos.go; `repos.list`, additive). */
object RepoOps {
    const val LIST_REPOS = "repos.list"
    const val ACTION_WORKTREE_CREATE = "worktree.create"
}

@Serializable
data class ListReposRequest(val query: String = "", val nodeId: String = "", val limit: Int = 0)

/** One repository on one machine (Go embeds Repo, so its fields are top level). */
@Serializable
data class RepoEntry(
    val name: String = "",
    val path: String = "",
    val defaultBranch: String = "",
    val branch: String = "",
    val remoteUrl: String = "",
    val lastActivity: String = "",
    val worktrees: Int = 0,
    val nodeId: String = "",
    val nodeName: String = "",
    val lastUsedAt: String? = null,
    val score: Int = 0,
)

@Serializable
data class RepoList(val items: List<RepoEntry> = emptyList(), val nodes: Map<String, String> = emptyMap())

@Serializable
data class WorktreeCreateParams(val repos: List<String>, val branch: String = "", val slug: String = "", val dir: String = "")

@Serializable
data class CreatedWorktree(val repo: String = "", val path: String = "", val branch: String = "")

@Serializable
data class WorktreeCreateResult(val cwd: String = "", val branch: String = "", val worktrees: List<CreatedWorktree> = emptyList())
