package dev.jhh3.pyre.ui

import android.os.Build
import androidx.activity.BackEventCompat
import androidx.activity.compose.BackHandler
import androidx.activity.compose.PredictiveBackHandler
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.consumeWindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.material3.Badge
import androidx.compose.material3.BadgedBox
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.ExtendedFloatingActionButton
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.NavigationBar
import androidx.compose.material3.NavigationBarItem
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.TopAppBarDefaults
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.mutableFloatStateOf
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.LifecycleEventEffect
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import androidx.lifecycle.viewmodel.compose.viewModel
import dev.jhh3.pyre.AppGraph
import dev.jhh3.pyre.HubSession
import dev.jhh3.pyre.PlatformActions
import dev.jhh3.pyre.R
import dev.jhh3.pyre.data.HubHealth
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.data.valueOrNull
import dev.jhh3.pyre.domain.Tone
import dev.jhh3.pyre.files.HubArtifactContent
import dev.jhh3.pyre.domain.Flags
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.ui.common.LoadingBox
import androidx.compose.runtime.LaunchedEffect
import dev.jhh3.pyre.notify.Notifications
import dev.jhh3.pyre.notify.PushSupport
import dev.jhh3.pyre.terminal.TerminalSocket
import dev.jhh3.pyre.ui.approvals.ApprovalScreen
import dev.jhh3.pyre.ui.approvals.ApprovalViewModel
import dev.jhh3.pyre.ui.comments.CommentLinkScreen
import dev.jhh3.pyre.ui.comments.CommentLinkViewModel
import dev.jhh3.pyre.ui.common.Banner
import dev.jhh3.pyre.ui.common.LocalRetrySignal
import dev.jhh3.pyre.ui.common.LocalScreenChrome
import dev.jhh3.pyre.ui.common.PyreIcon
import dev.jhh3.pyre.ui.common.RefreshWhileResumed
import dev.jhh3.pyre.ui.common.ScreenChrome
import dev.jhh3.pyre.ui.common.Supporting
import dev.jhh3.pyre.ui.launch.LaunchScreen
import dev.jhh3.pyre.ui.launch.LaunchViewModel
import dev.jhh3.pyre.ui.manage.AccountsScreen
import dev.jhh3.pyre.ui.manage.AccountsViewModel
import dev.jhh3.pyre.ui.manage.ActivityScreen
import dev.jhh3.pyre.ui.manage.ActivityViewModel
import dev.jhh3.pyre.ui.manage.MachinesScreen
import dev.jhh3.pyre.ui.manage.MachinesViewModel
import dev.jhh3.pyre.ui.nav.DeepLinks
import dev.jhh3.pyre.ui.nav.NavEntry
import dev.jhh3.pyre.ui.nav.Navigator
import dev.jhh3.pyre.ui.nav.Screen
import dev.jhh3.pyre.ui.pairing.PairingScreen
import dev.jhh3.pyre.ui.pairing.PairingViewModel
import dev.jhh3.pyre.ui.projects.AllArtifactsScreen
import dev.jhh3.pyre.ui.projects.AllArtifactsViewModel
import dev.jhh3.pyre.ui.projects.ArtifactScreen
import dev.jhh3.pyre.ui.projects.ArtifactViewModel
import dev.jhh3.pyre.ui.projects.DocumentScreen
import dev.jhh3.pyre.ui.projects.DocumentViewModel
import dev.jhh3.pyre.ui.projects.ProjectScreen
import dev.jhh3.pyre.ui.projects.ProjectViewModel
import dev.jhh3.pyre.ui.projects.ProjectsScreen
import dev.jhh3.pyre.ui.projects.ProjectsViewModel
import dev.jhh3.pyre.ui.session.MessageScreen
import dev.jhh3.pyre.ui.session.MessageViewModel
import dev.jhh3.pyre.ui.session.SessionDetailScreen
import dev.jhh3.pyre.ui.session.SessionDetailViewModel
import dev.jhh3.pyre.digest.PrefsLastSeenStore
import dev.jhh3.pyre.ui.sessions.SessionsScreen
import dev.jhh3.pyre.ui.sessions.SessionsViewModel
import dev.jhh3.pyre.ui.settings.DeviceControls
import dev.jhh3.pyre.ui.settings.SettingsScreen
import dev.jhh3.pyre.ui.settings.SettingsViewModel
import dev.jhh3.pyre.ui.settings.pushLabel
import dev.jhh3.pyre.ui.terminal.TerminalScreen
import dev.jhh3.pyre.ui.terminal.TerminalViewModel
import dev.jhh3.pyre.ui.viewer.HtmlPreviewScreen
import dev.jhh3.pyre.ui.viewer.HtmlPreviewViewModel
import kotlin.coroutines.cancellation.CancellationException
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

/** The bottom bar: Sessions · Projects · Machines · Accounts (as the TUI's tabs). Settings opens from the top bar. */
private data class Destination(val screen: Screen, val label: String, val icon: Int)

private val Destinations = listOf(
    Destination(Screen.Sessions, "Sessions", R.drawable.ic_terminal),
    Destination(Screen.Projects, "Projects", R.drawable.ic_folder),
    Destination(Screen.Machines, "Machines", R.drawable.ic_dns),
    Destination(Screen.Accounts, "Accounts", R.drawable.ic_person),
)

/** Sessions that need you (blocking flags), for the Sessions badge. Polls only while the app is in front. */
class NeedsYouCountViewModel(private val repo: Repository) : ViewModel() {
    private val _count = MutableStateFlow(0)
    val count: StateFlow<Int> = _count.asStateFlow()

    init { refresh() }

    fun refresh() {
        viewModelScope.launch {
            repo.sessions().valueOrNull()?.let { list -> _count.value = list.filterNot { it.session.archived }.count(Flags::needsYou) }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun PyreRoot(graph: AppGraph, navigator: Navigator, permissionTick: Int, actions: PlatformActions) {
    val session by graph.session.collectAsStateWithLifecycle()
    val health by graph.health.state.collectAsStateWithLifecycle()
    val s = session
    val entry = navigator.current
    val pairingScreen = entry.screen as? Screen.Pairing
    if (s == null || pairingScreen != null) {
        Surface(Modifier.fillMaxSize(), color = MaterialTheme.colorScheme.background) {
            val vm = viewModel(entry, key = "pair") {
                PairingViewModel(graph.pairing, pairingScreen?.prefill, graph.settings.prefs.value.devCleartextAllowed)
            }
            BackHandler(enabled = s != null) { navigator.pop() }
            PairingScreen(vm, onCancel = if (s != null) ({ navigator.pop() }) else null) { creds ->
                graph.settings.update { it.copy(devCleartextAllowed = creds.endpoint.devCleartext) }
                graph.onPaired(creds)
                navigator.reset(Screen.Sessions)
            }
        }
        return
    }

    var retrySignal by remember { mutableIntStateOf(0) }
    val chrome = remember(entry) { ScreenChrome() }
    val badgeVm = viewModel(key = "needs-you-count-" + s.credentials.clientId) { NeedsYouCountViewModel(s.repo) }
    val needsYou by badgeVm.count.collectAsStateWithLifecycle()
    RefreshWhileResumed(15_000) { badgeVm.refresh() }

    // Predictive back: the screen shrinks and follows the finger, then pops.
    var backProgress by remember { mutableFloatStateOf(0f) }
    PredictiveBackHandler(enabled = navigator.stack.size > 1) { progress: Flow<BackEventCompat> ->
        try {
            progress.collect { backProgress = it.progress }
            navigator.pop()
        } catch (e: CancellationException) {
            // gesture cancelled: stay
        } finally {
            backProgress = 0f
        }
    }
    val slide = with(LocalDensity.current) { 32.dp.toPx() }

    val screen = entry.screen
    val fullScreen = screen is Screen.Terminal
    CompositionLocalProvider(LocalScreenChrome provides chrome, LocalRetrySignal provides retrySignal) {
        Scaffold(
            topBar = {
                if (!fullScreen) TopBar(screen, chrome.title, navigator)
            },
            bottomBar = {
                if (screen.isRoot) BottomBar(screen, needsYou, navigator)
            },
            floatingActionButton = {
                if (screen == Screen.Sessions) {
                    ExtendedFloatingActionButton(
                        onClick = { navigator.push(Screen.Launch) },
                        icon = { PyreIcon(R.drawable.ic_add, null) },
                        text = { Text("New session") },
                        // ExtendedFloatingActionButton hides its animated label from accessibility; name it explicitly.
                        modifier = Modifier.testTag("new-session").semantics { contentDescription = "New session" },
                    )
                }
            },
            contentWindowInsets = if (fullScreen) WindowInsets(0, 0, 0, 0) else androidx.compose.material3.ScaffoldDefaults.contentWindowInsets,
            containerColor = MaterialTheme.colorScheme.surface,
        ) { inner ->
            Column(
                Modifier.fillMaxSize().padding(inner).consumeWindowInsets(inner)
                    .graphicsLayer {
                        val p = backProgress
                        scaleX = 1f - 0.06f * p
                        scaleY = 1f - 0.06f * p
                        translationX = slide * p
                    }
                    .let { if (backProgress > 0f) it.clip(RoundedCornerShape(24.dp)) else it },
            ) {
                if (!fullScreen) HubStatus(health, onRetry = {
                    retrySignal++
                    graph.flushOutbox()
                }, onPairAgain = { navigator.push(Screen.Pairing()) })
                Box(Modifier.weight(1f)) {
                    key(entry) { ScreenHost(entry, s, graph, navigator, permissionTick, actions) }
                }
            }
        }
    }
}

/**
 * Connection state as a calm inline line with a retry, not a stuck banner.
 * Only a refused protocol or a revoked device (which need action) get a
 * tinted notice.
 */
@Composable
private fun HubStatus(health: HubHealth, onRetry: () -> Unit, onPairAgain: () -> Unit) {
    when {
        health.protocolMismatch != null -> Banner(
            "This app and your hub don't speak the same version (${health.protocolMismatch}). Update the app or the hub.",
            Tone.ERROR,
            Modifier.testTag("hub-status"),
        )
        health.unauthorized -> Banner("This phone is no longer paired with your hub. Saved data stays readable.", Tone.ERROR, Modifier.testTag("hub-status")) {
            TextButton(onClick = onPairAgain) { Text("Pair again") }
        }
        !health.reachable -> Row(
            Modifier.fillMaxWidth().padding(start = 16.dp, end = 8.dp).testTag("hub-status")
                .semantics(mergeDescendants = true) {},
            verticalAlignment = Alignment.CenterVertically,
        ) {
            PyreIcon(R.drawable.ic_cloud_off, null, Modifier.size(18.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
            Spacer(Modifier.width(10.dp))
            Supporting("Can't reach your hub. Showing saved data.", Modifier.weight(1f))
            TextButton(onClick = onRetry) { Text("Retry") }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun TopBar(screen: Screen, dynamicTitle: String?, navigator: Navigator) {
    TopAppBar(
        title = { Text(dynamicTitle ?: title(screen), maxLines = 1, overflow = TextOverflow.Ellipsis) },
        navigationIcon = {
            if (!screen.isRoot) {
                IconButton(onClick = { navigator.pop() }, modifier = Modifier.testTag("back")) {
                    PyreIcon(R.drawable.ic_arrow_back, "Back")
                }
            }
        },
        actions = {
            when (screen) {
                Screen.Projects -> IconButton(onClick = { navigator.push(Screen.AllArtifacts) }) { PyreIcon(R.drawable.ic_image, "All artifacts") }
                Screen.Accounts -> IconButton(onClick = { navigator.push(Screen.Activity) }, modifier = Modifier.testTag("open-activity")) { PyreIcon(R.drawable.ic_history, "Activity") }
                else -> Unit
            }
            if (screen.isRoot) {
                IconButton(onClick = { navigator.push(Screen.Settings) }, modifier = Modifier.testTag("open-settings")) {
                    PyreIcon(R.drawable.ic_settings, "Settings")
                }
            }
        },
        colors = TopAppBarDefaults.topAppBarColors(containerColor = MaterialTheme.colorScheme.surface),
    )
}

private fun title(s: Screen) = when (s) {
    Screen.Sessions -> "Sessions"
    Screen.Projects -> "Projects"
    Screen.Machines, is Screen.Node -> "Machines"
    Screen.Accounts -> "Accounts"
    Screen.Activity -> "Activity"
    Screen.Settings -> "Settings"
    Screen.Launch -> "New session"
    Screen.AllArtifacts -> "Artifacts"
    is Screen.Pairing -> "Pair"
    is Screen.SessionDetail -> "Session"
    is Screen.Approval -> "Request"
    is Screen.Message -> "Message"
    is Screen.Project -> "Project"
    is Screen.Document -> "Document"
    is Screen.Artifact -> "Artifact"
    is Screen.Terminal -> "Terminal"
    is Screen.HtmlPreview -> "Preview"
    is Screen.Comment -> "Comment"
    is Screen.AttentionLink -> "Opening…"
}

@Composable
private fun BottomBar(current: Screen, needsYou: Int, navigator: Navigator) {
    NavigationBar {
        Destinations.forEach { d ->
            NavigationBarItem(
                selected = current == d.screen,
                onClick = { navigator.root(d.screen) },
                icon = {
                    if (d.screen == Screen.Sessions && needsYou > 0) {
                        BadgedBox(badge = { Badge { Text(if (needsYou > 99) "99+" else "$needsYou") } }) { PyreIcon(d.icon, null) }
                    } else {
                        PyreIcon(d.icon, null)
                    }
                },
                label = { Text(d.label, maxLines = 1) },
                modifier = Modifier.testTag("nav-" + d.screen.key).semantics {
                    if (d.screen == Screen.Sessions && needsYou > 0) contentDescription = "Sessions, $needsYou need you"
                },
            )
        }
    }
}

@Composable
private fun ScreenHost(entry: NavEntry, s: HubSession, graph: AppGraph, nav: Navigator, permissionTick: Int, actions: PlatformActions) {
    val repo = s.repo
    val ctx = LocalContext.current
    val openLink: (String) -> Unit = { link -> DeepLinks.parse(link)?.let(nav::push) }
    when (val screen = entry.screen) {
        Screen.Sessions -> SessionsScreen(
            viewModel(entry) { SessionsViewModel(repo, settings = graph.settings, digestStore = PrefsLastSeenStore(graph.settings, s.credentials.clientId)) },
            onOpenMachine = { nav.push(Screen.Node(it)) },
            onLaunch = { nav.push(Screen.Launch) },
            onOpenLink = openLink,
        ) { nav.push(Screen.SessionDetail(it)) }
        Screen.Projects -> ProjectsScreen(viewModel(entry) { ProjectsViewModel(repo) }, { nav.push(Screen.Project(it)) }, { nav.push(Screen.AllArtifacts) })
        Screen.Machines -> MachinesScreen(viewModel(entry) { MachinesViewModel(repo) })
        is Screen.Node -> MachinesScreen(viewModel(entry) { MachinesViewModel(repo) }, highlight = screen.nodeId)
        Screen.Accounts -> AccountsScreen(viewModel(entry) { AccountsViewModel(repo) })
        Screen.Activity -> ActivityScreen(viewModel(entry) { ActivityViewModel(repo) })
        Screen.Launch -> LaunchScreen(viewModel(entry) { LaunchViewModel(repo) }) { id ->
            nav.pop()
            nav.push(Screen.SessionDetail(id))
        }
        Screen.AllArtifacts -> AllArtifactsScreen(viewModel(entry) { AllArtifactsViewModel(repo) }) { nav.push(Screen.Artifact(artifactId = it)) }
        is Screen.SessionDetail -> SessionDetailScreen(
            viewModel(entry) { SessionDetailViewModel(repo, screen.sessionId) },
            onOpenApproval = { nav.push(Screen.Approval(it)) },
            onOpenTerminal = { nav.push(Screen.Terminal(it)) },
            onOpenLink = openLink,
        )
        is Screen.Approval -> ApprovalScreen(viewModel(entry) { ApprovalViewModel(repo, screen.approvalId, s.answerer) }) { nav.push(Screen.SessionDetail(it)) }
        is Screen.Message -> MessageScreen(viewModel(entry) { MessageViewModel(repo, screen.messageId) }, onOpenLink = openLink) { nav.push(Screen.SessionDetail(it)) }
        is Screen.Project -> ProjectScreen(viewModel(entry) { ProjectViewModel(repo, screen.projectId) }, { nav.push(Screen.Document(it)) }, { nav.push(Screen.Artifact(artifactId = it)) })
        is Screen.Document -> DocumentScreen(viewModel(entry) { DocumentViewModel(repo, screen.documentId, screen.revisionId) })
        is Screen.Artifact -> ArtifactScreen(
            viewModel(entry) { ArtifactViewModel(repo, HubArtifactContent(s.client), ctx.cacheDir, screen.artifactId, screen.versionId) },
        ) { nav.push(Screen.HtmlPreview(it)) }
        is Screen.HtmlPreview -> HtmlPreviewScreen(viewModel(entry) { HtmlPreviewViewModel(repo, s.credentials.endpoint, screen.versionId) }, s.credentials.endpoint)
        is Screen.Terminal -> TerminalScreen(
            viewModel(entry) { TerminalViewModel(repo, screen.sessionId, connector = { d, l -> TerminalSocket(s.client, d, l) }) },
            onBack = { nav.pop() },
        )
        Screen.Settings -> {
            val prefs by graph.settings.prefs.collectAsStateWithLifecycle()
            var resumeTick by remember { mutableIntStateOf(0) }
            LifecycleEventEffect(Lifecycle.Event.ON_RESUME) { resumeTick++ }
            @Suppress("UNUSED_EXPRESSION") permissionTick
            @Suppress("UNUSED_EXPRESSION") resumeTick
            SettingsScreen(
                viewModel(entry) { SettingsViewModel(repo) },
                s.credentials,
                DeviceControls(
                    pushStatus = pushLabel(PushSupport.provider() != null, PushSupport.provider()?.unavailableReason(ctx), prefs.pushStatus),
                    notificationsAllowed = Notifications.canPost(ctx),
                    streamEnabled = prefs.hubStreamEnabled,
                    batteryExempt = actions.batteryExempt(),
                    themeMode = prefs.themeMode,
                    dynamicColor = prefs.dynamicColor,
                    dynamicColorSupported = Build.VERSION.SDK_INT >= 31,
                    onRequestNotifications = actions::requestNotificationPermission,
                    onOpenNotificationSettings = actions::openNotificationSettings,
                    onToggleStream = actions::setStream,
                    onRequestBatteryExemption = actions::requestBatteryExemption,
                    onThemeMode = { m -> graph.settings.update { it.copy(themeMode = m) } },
                    onDynamicColor = { v -> graph.settings.update { it.copy(dynamicColor = v) } },
                    onUnpair = {
                        actions.setStream(false)
                        graph.unpair()
                        nav.reset(Screen.Sessions)
                    },
                ),
            )
        }
        is Screen.Comment -> CommentLinkScreen(viewModel(entry) { CommentLinkViewModel(repo, screen.commentId) }) { v ->
            nav.pop()
            nav.push(Screen.Artifact(versionId = v))
        }
        is Screen.AttentionLink -> AttentionLinkHost(screen.attentionId, repo, nav)
        is Screen.Pairing -> Unit // handled by PyreRoot
    }
}

/**
 * Resolves an old inbox link (pyre://attention/<id>) to the session the item
 * is about (else the item's own link, else Sessions) and replaces itself.
 */
@Composable
private fun AttentionLinkHost(attentionId: String, repo: Repository, nav: Navigator) {
    LaunchedEffect(attentionId) {
        val item = try {
            repo.call { getAttention(attentionId) }
        } catch (e: PyreApiException) {
            null
        }
        val target = when {
            item == null -> null
            item.sessionId.isNotEmpty() -> Screen.SessionDetail(item.sessionId)
            else -> DeepLinks.parse(item.deepLink)?.takeIf { it !is Screen.AttentionLink && it != Screen.Sessions }
        }
        if (target == null) nav.root(Screen.Sessions) else nav.replace(target)
    }
    LoadingBox()
}
