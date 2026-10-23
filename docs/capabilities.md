# Runtime adapter capabilities (WP07)

Implements decision I07 (runtime report §8–§9): direct native adapters, no HAPI. Code: `internal/runtime/{tmux,shell,claude,codex,agy}`. Contract: `contracts.RuntimeAdapter` (§4 of [contracts](contracts.md)).

Labels: **verified-real** = exercised against the real harness (versions below); **verified-fake** = unit/fake suites only; **unproven**.

## Tested versions and environments (2026-10-04)

| Host | OS | tmux | Claude Code | Codex | Suites |
|---|---|---|---|---|---|
| Mac (dev) | macOS 26.6 arm64 | 3.4 | 2.1.289 (default login) | 0.160.0 npm (default `~/.codex`) | unit + fake (`go test -race`, 3× repeat), `TestRealClaude*`, `TestRealCodex` |
| server | Ubuntu 26.04 x86_64 | 3.6 | 2.1.289 (default login) | not logged in, not installed on PATH | unit + fake, `TestRealClaude*` (cross-compiled test binaries) |

Real tests run only with `PYRE_REAL_HARNESS=1` and need an already-trusted scratch cwd (`PYRE_REAL_CWD`; Codex: `PYRE_REAL_CODEX_CWD`). They use tiny synthetic prompts (sonnet/low effort for Claude; `model_reasoning_effort="low"` for Codex), an isolated tmux socket, and never edit `~/.claude/settings.json`, `~/.claude.json` or `~/.codex/config.toml` (hooks come from `--settings`). Normal harness writes still happen: Claude transcripts under `~/.claude/projects/<slug>/`, Codex rollouts under `~/.codex/sessions`.

## Shared node layer: isolated tmux (`internal/runtime/tmux`)

- One Pyre tmux server per node, addressed only by `-S <socket>` (absolute, ≤100 bytes; macOS `sun_path` is 104). Config written next to the socket: `remain-on-exit on` (exit status stays observable), `exit-empty off`, history 10k.
- The server is started with a bare `start-server`: a daemonized tmux server keeps the argv of the client that started it in `ps` for its whole life, so starting it with `new-session -e TOKEN=…` would expose that value. Panes inherit the server's global environment.
- Commands run as argv (tmux execs ≥2 args directly; a single arg is wrapped as `exec '<quoted>'`).
- The run environment (`PYRE_SESSION_TOKEN`, environment-overlay and connection values, `PATH`) never goes on a command line or into the tmux session environment. The adapter writes it, together with `ResolvedLaunch.Unset` and the harness argv, to a private launch file (0600, in the 0700 socket dir). The pane runs `<pyred> __pyre_exec_env <file> --`, which reads and deletes the file, applies the environment, and execs the argv from the file in place (same PID), byte for byte. Explicit `Env` wins over `Unset`. Consequences:
  - The tmux command line stays small whatever the guide and prompt size. tmux refuses a client command over 16 KiB (MAX_IMSGSIZE): "command too long", I48. A spec with no environment and an argv under 4 KiB keeps the plain command line.
  - tmux clients, the tmux server and `ps` show only the file path.
  - A window or pane a human opens later in a run's session does not inherit the run's variables. Agent tools inherit them from the harness process: hooks, the pyre MCP server and the doorbell monitor are children of Claude.
  - The run's `PATH` (pyre's bin dir first) now takes effect. Under `-e` it did not: `pyre` was not on `PATH` in shell runs on either host. This is consistent with tmux taking a new pane's `PATH` from the client that creates it.
  - A respawned Codex TUI gets the environment again from adapter memory. After a pyred restart it gets only `CODEX_HOME`.
  - Verified on Mac (tmux 3.4) and server (tmux 3.6, systemd scope): [argv secrets evidence](evidence/security/argv-secrets-2026-10-04.md).
- Targets use exact match (`=name`, `=name:`); tmux otherwise prefix-matches.
- Text only through `load-buffer` (stdin) + `paste-buffer -p -d` (bracketed paste — tmux adds the markers only if the app enabled mode 2004, as Ink does). Keys only from a named allowlist (Enter, Escape, C-u, C-c, C-d, BSpace, arrows, Tab, End).
- `list-clients` (non-read-only) is the human-attached signal. `capture-pane` bounded to 256 KiB.
- `TerminateSession`: SIGTERM to the pane's process group, wait, SIGKILL, then `kill-session` (plain `kill-session` only SIGHUPs; a Claude in a dialog took seconds to exit on server).
- Attach descriptors: local `tmux -u -S <sock> attach-session -t =<run> [-r]`; remote `ssh -t <alias> "bash -lc '<quoted tmux attach>'"` (alias validated, never option-like). verified-real (local, via an outer isolated tmux as the pty; remote form verified in WP01 C18).

## Shell (`shell-tmux`)

| Item | Behaviour |
|---|---|
| Capabilities | `attach.native_terminal`, `attach.read_only`, `stop`, `screen.read` |
| Input acceptance | none — Deliver/Reconcile/Answer/Interrupt/ResumeSpec return `unsupported`; Pyre never types into a shell |
| State | `running` while the pane lives (reason says screen changed / unchanged since T, via cursor hash), `exited` with exit code (persisted to `exit.json` so it survives session removal) |
| Stop | `TerminateSession`; Recover → `not_found` once gone |
| Status | verified-fake (real tmux, `/bin/sh`) on Mac and server |

## Claude Code (`claude-tmux`)

Launch: `claude --settings <run>/pyre-hooks.json --session-id <uuid> | --resume <uuid> --permission-mode <explicit, default "default"> [--append-system-prompt <AgentGuide>] <Args> [InitialPrompt]` in tmux session `<run_id>`, env from `ResolvedLaunch.Env` plus `PYRE_RUN_ID`, `PYRE_SESSION_ID`, `PYRE_CLAUDE_STATE_DIR`. `--settings/--session-id/--resume/-c/-p/--bg/--fork-session` in Args are rejected. `ProfileAsConfigDir` sets `CLAUDE_CONFIG_DIR=ProfileDir` (mechanism verified by WP13; the path is used verbatim because the keychain item name hashes the exact string — pass the canonical path).

| Capability | Mac | server | Notes |
|---|---|---|---|
| `attach.native_terminal`, `attach.read_only` | verified-real | verified-real | attach argv registers a tmux client; read-only viewers do not block delivery |
| `input.queue`, `input.paste`, `input.reconcile` | verified-real | verified-real | see acceptance below |
| `input.monitor` (doorbell; pyred default) | verified-real | verified-real | mail accepted with a human attached or after a dialog closes; see Doorbell below |
| `approvals.answer` + `approvals.race_with_tui` | verified-real | verified-real | broker allow → effective; TUI-first / timeout paths verified-fake |
| `input.steer` + `input.stop_hook` (opt-in `StopHookSteer`) | verified-fake | — | turn-end steer; WP01 C7 verified the mechanism on real Claude |
| `interrupt` | verified-fake | — | Escape; state from transcript `turn_duration` |
| `stop` | verified-real | verified-real | `/exit` → SessionEnd → exit 0 |
| `resume.native` | verified-real | verified-real | same session id, history kept |
| `resume.handoff` | contract only | | another account whose profile does not share the transcript store → `handoff` |
| `account.cross_account_resume` (with `ProfileAsConfigDir`) | verified-fake | verified-fake | every Pyre Claude profile links projects/, todos/, file-history/, plans/, tasks/, session-env/, shell-snapshots/, history.jsonl to `<state>/shared/claude/`; the daemon resumes natively (same uuid) under another account only when both profiles resolve projects/ to the same directory, else handoff |
| `subagents.observe` | verified-real | verified-real | SubagentStart/Stop |
| `transcript.read`, `screen.read`, `hooks.events` | verified-real | verified-real | |
| `requires_trust_dialog` | verified-real | verified-real | untrusted cwd → `waiting_input`, "needs human: workspace trust"; `TrustWorkspace(cwd)` lets the adapter accept it (verified-fake only — accepting writes `~/.claude.json`) |
| `account.isolated_profile` (opt-in) | via WP13 | via WP13 | not exercised here with a second login |
| Not advertised | | | `input.idempotent`, `questions.answer`, `input.channel`, `agent.tools` (daemon's job) |

**How acceptance is known.** Deliver renders `[pyre-msg:<id>] <text>`. Evidence, strongest first: `UserPromptSubmit` hook whose prompt contains the marker (`claude.hook.user_prompt_submit`, ref = hook event id), Stop-hook injection claim (`claude.stop_hook.injected`), transcript `queue-operation enqueue` (`claude.transcript.queue_enqueue`), transcript user record (`claude.transcript.user_record`). A later user record for a queued message is reported in `Observation.Receipts` with `Detail: consumed_at=<RFC3339>` (contract has no receipt field for it yet).

**Gate (refuse → `queued`, nothing journaled).** Pane alive; no writable tmux client attached (input lease); hook state `waiting_input`/`running`/`waiting_on_background`; no pending/expired approval; no human-only dialog (`AskUserQuestion`, `ExitPlanMode`, elicitation); input box visible (waits ≤2 s for the TUI to render) and empty (dim placeholder recognised via SGR 2). A human's draft is never pasted over.

**Busy.** Paste while `running` uses Claude's native queue (enqueue evidence immediately, consumption later) — verified-real Mac and server.

**Idempotency / crash window.** No runtime idempotency key in the TUI. Before any byte is pasted an `attempting` record is fsynced to `<run>/deliveries.jsonl`; the paste must be recognisable in the input box before Enter — a paste into a dialog is ignored there and a bare Enter could approve it. Recognised: the marker, a collapsed `[Pasted text #N …]` (pastes over 800 characters) when the box was empty, or — because Claude shows only the last six rows of a long draft — the box showing exactly the end of the pasted text (whitespace ignored, at least 40 characters; any other text after the paste fails the match). Before pasting, a run window that a detached client left narrow is restored to the launch size (200×50) when no client is attached (`resize-window`, then the window's `window-size` is unset so the next attach still sizes it). The input lease is checked again right before Enter: a human who attached after the paste gets the draft and the result is `indeterminate` with what to do. An unrecognised draft is never submitted or cleared: `indeterminate` with "Needs a human: …", and Observe reports `needs human: Claude's input box holds Pyre's unsent draft of <id>` while a recognised leftover draft is in the box. Deliver of an id that already has evidence returns `accepted` without pasting. Reconcile: evidence → `accepted`; a draft of the message still on the input line (marker visible, or its end matching the normalised tail kept in the journal) → cleared (C-u/BSpace loop; C-u clears only one line of a multi-line draft) → `rejected` (never submitted, safe retry); otherwise `indeterminate`. Never auto-replays. Verified-real (crash before Enter with a 3-line draft, crash after Enter) on Mac and server.

**State.** Hooks (SessionStart, UserPromptSubmit, Pre/PostToolUse, PermissionRequest, Notification, Stop, StopFailure, SubagentStart/Stop, SessionEnd, PreCompact) folded incrementally, plus transcript `system/turn_duration` newer than the last state-changing hook → `waiting_input` (interrupts and TUI denials fire no Stop). Stop with non-empty `background_tasks` → `waiting_on_background`. No SessionStart yet → `starting`, or `waiting_input` "needs human: <dialog>" for a known/unknown dialog. Pane dead → `exited` + exit code; tmux unreachable → `unknown`. The adapter's `ChangeStamp` (size and mtime of `events.jsonl` and the transcript) lets pyred observe a backed-off run as soon as Claude writes something; a native-terminal interrupt fires no hook, so without it the state waited for the idle poll (`Poll.IdleMax`, 30 s). Verified-real on server: `waiting_approval` → `waiting_input` 1.9 s after a native Escape.

**Approvals.** With the broker on (default), the PermissionRequest hook writes `<run>/pending/<rid>.json` (correlated with the matching `PreToolUse.tool_use_id`) and waits up to `ApprovalWait` (120 s) for `<run>/answers/<rid>.json`; claiming is an atomic rename. `Approval.RequestID = rid`, `ApprovalID = apr_` + hash(run, rid); options `allow`/`deny` (+ text as deny message). `Answer` returns `effect=submitted`; Observe reports `effective` (PostToolUse after an allow; the turn moving on after a deny), `superseded` (TUI answered first / hook killed by a TUI "No" while a Pyre answer was pending), `native` resolution without Pyre, `expired` when the hook timed out and the TUI dialog is still up (human must answer there), `withdrawn` when the turn ended. Duplicate or stale answers → `conflict`. An expired approval is never answered by Pyre, but it stays actionable: the run stays `waiting_approval` with reason "permission prompt waiting in its terminal (… attach to answer it, or interrupt to cancel)", `pyre approvals` lists it with the attach/interrupt commands while the run still waits, and the inbox replaces the answer item with an urgent session item ("Answer in its terminal: …", group `terminal:<approval>`). When the prompt ends, the node reports the native outcome; the hub settles the expired approval (resolved/withdrawn) and the item is withdrawn.

**Ask nudge (Stop hook).** When a top-level run (no `$PYRE_PARENT_SESSION_ID`) ends a turn whose final message leaves an owner-directed question (`internal/askdetect.OwnerQuestion`: deterministic, last ~1200 characters, code blocks and quotes ignored) and the turn filed no ask (`mcp__pyre__ask_owner`, `mcp__pyre__flag_attention` or `pyre ask new` since the turn's `UserPromptSubmit`), the Stop hook blocks once with "file each open decision with ask_owner" and records `PyreStopAskNudge` (the run stays `running`). A queued Stop-hook injection goes first; never twice in a turn; skipped when the owner typed the turn's prompt within 30 minutes (no `[pyre-msg:…]` marker and not the launch prompt, `run.json` `initial_prompt`). The hub's input lease is not visible to the hook, so typing through a remote client is inferred the same way.

**Page nudge (Stop hook).** A top-level run in a project (`$PYRE_PROJECT_ID` set at launch, no parent) whose turn used a non-Pyre tool and whose own events show no page update (`mcp__pyre__update_project_page` or `pyre project page set`) and no earlier page nudge in the last 3 hours is blocked once at Stop with "check your project page (get_project_page) and update it if missing or stale" and records `PyreStopPageNudge` (run stays `running`). The hook has no hub connection, so a lead whose page another session keeps current is reminded at most every 3 hours and told to just end the turn if the page is current. At most one Stop-hook continuation per turn (`stop_hook_active`, an injection or the ask nudge go first); skipped when the owner typed the turn's prompt, as for the ask nudge.

**Pre-approved tools.** Every Claude run's `--settings` allows the Pyre MCP messaging tools `mcp__pyre__send_message`, `inbox`, `get_message`, `ack_message` and `wait_reply` (with or without the doorbell), plus the read-only `get_digest` and `search`, the self-only `rename_session` (the hub lets an agent rename only its own session, and never over a title the owner set), and the self-only `flag_attention`/`clear_attention` (an agent marks its own session "needs you" with a one-line reason; the hub refuses other sessions, and a permission prompt for it would itself need the owner), and the project page tools `update_project_page`/`get_project_page` (the hub lets only the owner and top-level sessions in the project write it). Session control (`start_session`, `stop_session`, `interrupt_session`), `answer_approval`, `reissue_message`, `resolve_delivery`, `publish`, project creation, session-project association and archiving (`create_project`, `associate_session_project`, `archive_session`), document/note writes and connection calls keep Claude's permission prompt. The agent guide's MCP variant tells Claude to reply with `mcp__pyre__send_message` (`reply_to`), because `pyre send` through Bash opens a permission dialog in the default mode.

**Interrupt / stop.** Interrupt sends Escape only when running/awaiting approval — a raw terminal action the daemon must gate with the input lease. Stop pastes `/exit` only into an empty idle prompt with no human attached (verified on screen before Enter), waits for exit, falls back to `TerminateSession`; exit status persisted before the session is removed.

**Resume / IDs.** `ResumeSpec(sameAccount)` → `native_resume` with the same Claude session id; Launch with `Resume` passes `--resume <id>` (same cwd required: transcripts are per project dir). Observe always reports the id from the latest SessionStart.

**Limit events.** StopFailure payloads, transcript API-error records and Claude error lines (`⎿ …`, `/login`, `/upgrade`) near the bottom of the screen are matched for usage limit (`limit reached|<epoch>` gives `ResetAt`), auth failure and rate limiting. Pattern list is narrow and unverified against real limit text (could not be triggered).

**Doorbell (`input.monitor`; on by default in pyred, decision I16).** Introduced by the S-D spike (DOORBELL.md); integrated with the real hub and pyred in doorbell-integration-2026-10-04.md. Verified-real on Mac and server.
- **Wiring:** pyred sets `DoorbellCommand = [<pyred>, inbox-watch]` for Claude runs. It is off with `pyred run --no-doorbell`, with `PYRE_DOORBELL=0`, or when no pyre CLI is found (the run would have no MCP inbox tool). A run whose launch environment disables plugin monitors (`DISABLE_TELEMETRY`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, a third-party provider) drops `input.monitor` from its capabilities (`RunHandle.Data["caps_unavailable"]`).
- **Mechanism:** a generated per-run plugin (`--plugin-dir=<state>/pyre-plugin`) runs `pyred inbox-watch` as a Claude Code monitor. It prints one coalesced line per batch of new mail (no bodies), which wakes the session even with a human attached. The line and the agent guide name the pre-approved MCP tools `mcp__pyre__inbox`, `mcp__pyre__send_message` (reply) and `mcp__pyre__ack_message`. The `pyre inbox` shell command needs a permission prompt and leaves no evidence.
- **Waiting (W-localwait):** the watcher waits on pyred's local `inbox.wait` (socket only), which wakes on a `deliver` command for that session, on hub link (re)connect, or after at most 60 s; it then lists the inbox once from the hub without waiting. An idle session therefore holds no hub request. Against a hub URL or an older pyred (`inbox.wait` → 404 → unsupported) it long-polls `messages.list` as before, and re-probes after a failed poll.
- **Evidence and receipts:** a PostToolUse on `mcp__pyre__inbox`/`get_message` returning the id is `claude.tool.inbox_read`. The node mints an attempt for it and reports one `accepted` receipt with `consumed` set. Re-observing the evidence after a pyred restart changes nothing. The doorbell line itself (`claude.monitor.notified`) never counts as acceptance.
- **No double delivery:** paste and doorbell exclude each other per message. The watcher skips ids that have a paste attempt. Deliver leaves mail the doorbell already announced to the agent's read for `DoorbellPasteHold` (3 min) and then falls back to paste. The engine never pastes an accepted message.
- **Timing:** notifications are queued like mail. They are delivered at turn end or after a dialog closes, never mid-turn.
- **Monitor lifecycle:** one watcher per run. Claude starts it through a shell (`zsh -c "… eval '<cmd>' … && pwd -P"`); the watcher exits when its shell or that shell's parent (Claude) changes. Claude does not restart a dead monitor: `DoorbellAlive` checks the watcher's pid file, and the gate then says the doorbell is down. The monitor is not counted as a Stop background task.

**Limitations.** Paste and screen parsing depend on the Ink TUI (calibrated on 2.1.289; Probe flags other versions). A clean TUI-side approval "Yes" leaves the broker hook waiting until timeout (harmless; effect reported as native). A `TrustWorkspace` acceptance writes Claude's own trust entry. Dev channels and `--bg` are not used.

## Codex (`codex-appserver`)

One Pyre-owned `codex app-server --listen unix://<StateRoot>/codex/<home-key>/codex.sock` per `CODEX_HOME` binding (`ProfileDir`; empty = node default home), run in tmux session `codex-as-<home-key>` so it survives daemon restarts. Every start makes the socket directory and `<StateRoot>/codex` private to the current user (`fileperm.UserOnlyDir`: 0700 on Unix; on Windows a protected DACL with one (OI)(CI) full-control ACE for the user, which is the only ACL Codex accepts on a socket directory). Refs in `<dir>/refs/<run_id>` (and transient `probe-*`) make the refcount restart-safe; the last ref stops the server (SIGTERM to the process group — npm's node wrapper and the native binary — then SIGKILL). This satisfies WP13's one-process-per-home lease (holder `codex-app-server`). JSON-RPC over the unix-socket WebSocket (`github.com/coder/websocket`), `initialize` with `experimentalApi` and noisy-delta opt-outs. Per run, tmux session `<run_id>` runs `codex resume <thread> --remote unix://<sock> -C <cwd>` as the native TUI.

| Capability | Mac | server | Notes |
|---|---|---|---|
| `input.queue`, `input.app_server`, `input.reconcile` | verified-real | verified-fake | clientId evidence; reconcile from `thread/turns/list` after a restart |
| `input.steer` | verified-fake | verified-fake | `turn/steer` with `expectedTurnId`; WP01 X5 verified on real Codex |
| `approvals.answer` | verified-real | verified-fake | command approval answered through Pyre → `effective` |
| `questions.answer` | verified-fake | | `item/tool/requestUserInput` answers |
| `interrupt` | verified-fake | | WP01 X6 verified on real Codex |
| `stop`, `resume.native` | verified-real | verified-fake | resume kept the thread id in a fresh app-server |
| `subagents.observe` | verified-real | verified-fake | `subAgentActivity` + `parentThreadId` |
| `attach.native_terminal`, `attach.read_only`, `attach.codex_remote`, `screen.read` | verified-real | verified-fake | TUI respawned on Attach if the human quit it |
| `transcript.read`, `account.isolated_profile` | verified-real / via WP13 | | |
| `wake.background_command` | | verified-real | Codex 0.163.0; evidence |
| Not advertised | | | `input.idempotent` (Codex does not dedupe clientId), `hooks.events` |

**How acceptance is known.** `turn/start {threadId, input, clientUserMessageId: <message_id>}` when the thread is idle/unloaded (unloaded threads are resumed first). Evidence: the `userMessage` item echoing `clientId == message_id` (`codex.item.user_message.client_id`, ref = item id). If the turn/start/steer response returned a turn id but the item has not arrived within `DeliverTimeout`, the result is `accepted` with `protocol_ack` (ref = turn id) and the item receipt follows in Observe.

**Busy.** Thread `active` (or an approval pending) → `queued` for `ModeQueue`; `ModeSteer` uses `turn/steer` with the active turn id (mismatch / no active turn → `rejected`, safe retry). If the thread turns busy between the status check and `turn/start` (e.g. a human typed in the TUI), Codex folds the message into the active turn; the detail says "joined the active turn". Native TUI attachment does not block structured delivery (they share the app-server queue).

**Idempotency / crash window.** An `attempting` record is fsynced before every send. Codex does not dedupe clientIds, so Deliver searches the thread for the id when an earlier attempt exists and refuses (`indeterminate`) to resend an id whose outcome is unknown. JSON-RPC error → `rejected`; lost connection/timeout after the write → `indeterminate`. Reconcile searches up to 20 pages of `thread/turns/list`. Verified-fake (send-then-die, new adapter Recover + Reconcile) and verified-real (restart + Recover + Reconcile, single app-server).

**State.** `thread/status/changed`: `idle`/`notLoaded` → `waiting_input`, `active` → `running`, `activeFlags waitingOnApproval|waitingOnUserInput` → `waiting_approval`, `systemError` → `unknown`; app-server unreachable → `unknown` (Observe reconnects to a live server but never starts one); stopped → `exited`. A turn that started (`turn/started`) and ended between two observations is reported `running` once ("turn active (ended since the last observation)"), so its end is seen; a failed turn no observation saw (an automatic capacity continue failing at once) is not. `sessions.read` of a stopped run, or one whose app-server is confirmed not running, reads the run's rollout file instead (stale, "read from the rollout file"; user/assistant messages and tool calls numbered over the rollout, so a live read's cursor does not carry over; Pyre messages are `pyre_message` with their id and background-command notices `system`, as in the live read). Any other app-server error stays an empty stale read. The rollout is looked up in the launch day's folders (±1 day), then `archived_sessions`, then (at most every 10 minutes) every day folder, and remembered in the run's state dir (`rollout-path`); a run on the node's own login finds it under `CODEX_HOME`, else `~/.codex`.

**Approvals.** Server requests `item/commandExecution|fileChange|permissions/requestApproval` and `item/tool/requestUserInput` become approvals with `RequestID = as<app-server pid>:<rpc id>:<itemId>`. Options come from `availableDecisions` (`accept`, `acceptForSession`, `decline`, `cancel`, `acceptWithExecpolicyAmendment`, …; `allow`/`deny` accepted as aliases). Answered once; `serverRequest/resolved` is the truth: after Pyre answered → `effective`, otherwise `native` (superseded). Other server requests are left to a connected TUI. After an app-server restart pending requests are re-sent with new ids (old ids → `not_found`).

**Interrupt / stop.** `turn/interrupt` (bounded; it never answers for a finished turn) then `thread/backgroundTerminals/clean` (commands outlive an interrupt). An idle thread with commands Pyre saw start and not finish is cleaned too (detail "nothing to interrupt (no active turn); background commands stopped …"): the agent may be waiting on them. MCP `interrupt_session` says so. Stop interrupts an active turn, `thread/unsubscribe`s, terminates the TUI session and drops the ref.

**Background-command wake.** Codex's unified exec yields a long command back to the model (after about 10 s) and keeps it running after the turn ends. Codex reports its exit (`item/completed` for the commandExecution item, with the turn that started it) but starts no turn. When such a command finishes after its turn ended, Pyre starts one turn (`clientUserMessageId pyre-bgwake-<n>`, transcript kind `system`): ``[pyre] background command finished: `<command>` (exit N, cwd …, ran …). Last output:`` plus the last 2 KB. Completions during another turn wait until the thread is idle (no active turn, approval, usage limit or due capacity continue) and are never steered in. Completions within 1.5 s share one notice (up to 8 listed, 6 KB of output in all). A turn/start Codex refused is retried up to 3 times. One whose outcome is unknown is dropped, never resent. No wake after Stop (which first waits for a wake already sending), for the commands running when Pyre interrupts the thread (ignored by item id when Pyre saw them start, so even a failed clean cannot wake the agent; commands started before a pyred restart are covered only by the 5 s mute after the clean), or for a session the hub reports archived or archiving when done (`Options.WakeAllowed`; allowed when the hub is unreachable). A wake is audited as `node.background_wake` (automation `background_wake`, once per run), is not a Pyre message (no receipt, no inbox item) and raises no owner attention itself. Its turn end is an ordinary turn end (a child reports it to its parent). Limits: the queue is in memory, so exits while pyred is down are missed. Subagent threads are not woken. A later turn that polled the command itself still gets the notice. Verified live with 0.163.0 (evidence).

**Resume / IDs.** Thread ids are stable; `ResumeSpec(sameAccount)` → `native_resume`; Launch with `Resume` calls `thread/resume`. Cross-home resume needs the rollout in the target home's `sessions/` (WP13 shares the store; unproven here) → `handoff`.

**Accounts (WP13).** `ReadRateLimits(ctx, codexHome)` returns the raw `account/rateLimits/read` result through the home's single app-server (starting it under a transient ref when idle and stopping it afterwards); `ReadAccount` returns type/plan only (no email, `refreshToken:false`). Neither requests a token refresh.

**Limitations.** Linux real turns unproven (server not logged in). Experimental methods used: `thread/backgroundTerminals/clean`. Threads report `source` as an app-server client and appear in the user's `codex` history. Using the default home runs a second app-server next to the user's own shared daemon (as in WP01); WP13 profiles avoid that.

## Antigravity CLI (`agy-tmux`)

Google Antigravity's CLI, verified-real with agy 1.4.2 on server (Ubuntu 26.04, tmux 3.6, the machine's own signed-in login) on 2026-10-18 (`TestRealAgy`, `PYRE_REAL_AGY=1`; evidence in the PR), plus fake suites. A node offers agy only where the `agy` binary is found (`ProbeResult.Absent` otherwise, so machines without it do not list it; Windows always reports it absent until validated, and macOS is offered but untested, which the probe reason says) and signed in (`~/.gemini/antigravity-cli/antigravity-oauth-token` exists; its content is never read by the adapter).

Launch: `agy --add-dir <state>/agy/<session_id> [--conversation <id>] [--dangerously-skip-permissions] [--model M] [--effort E] <Args> [--prompt-interactive <InitialPrompt>]` in tmux session `<run_id>`, env from `ResolvedLaunch.Env` plus `PYRE_RUN_ID`, `PYRE_SESSION_ID`. agy has no per-run settings, MCP or system-prompt flags, but it loads `.agents/` customizations from every workspace folder, so the adapter writes a per-session folder (rewritten at each launch; one folder per session keeps a resumed conversation's workspace stable):

- `.agents/hooks.json`: one named hook `pyre` running `pyred hook agy <Event> <state-dir>` for SessionStart, PreInvocation, PostToolUse, PostInvocation and Stop. The handler appends conversation id, transcript path, model, tool name and stop reason (never tool arguments) to `<state>/agy-events.jsonl` and answers `{}`. PreToolUse is not registered: agy treats a PreToolUse answer without a decision as a denial.
- `.agents/mcp_config.json`: the `pyre` MCP server (`pyre mcp`). It is a child of agy and inherits the session's environment (`PYRE_SESSION_TOKEN` etc.), so tools act as the session (verified-real with a synthetic stdio server that reported the session id).
- `.agents/rules/pyre.md`: an always-on rule with the agent guide. agy sorts workspace folders by path and runs commands in the first by default, so the rule names the real working directory and tells the agent to stay out of the configuration folder (verified-real: commands ran in the cwd).

Nothing is written to the owner's `~/.gemini/config` or to the project. agy itself records accepted workspace trust in `~/.gemini/antigravity-cli/settings.json` (`trustedWorkspaces`), as when a person answers.

| Capability | server | Notes |
|---|---|---|
| `attach.native_terminal`, `attach.read_only` | unproven | the shared tmux attach (as Claude); not exercised against agy |
| `input.queue`, `input.paste`, `input.reconcile` | verified-real | see acceptance below |
| `interrupt` | verified-real | Escape; no Stop hook fires, the footer turns back to `? for shortcuts` |
| `stop` | verified-real | `/exit` at an idle empty prompt, else `TerminateSession` |
| `resume.native` | verified-real | `--conversation <id>` (history and memory kept); the conversation must exist in this node's agy home, else `handoff` |
| `resume.handoff` | contract only | |
| `transcript.read`, `screen.read`, `hooks.events` | verified-real | |
| `agent.tools` | verified-real | pyre MCP server registered (when the pyre CLI is installed) |
| `requires_trust_dialog` | verified-real | accepted in `--trust-dir` directories, and anywhere for a Claude bypass run |
| `permissions.bypass` | verified-real | `--dangerously-skip-permissions` |
| Not advertised | | `input.steer` (agy's alt+m "send immediately" is not driven), `input.idempotent`, `approvals.answer` / `questions.answer` (permission prompts are answered in the terminal; the run reports `waiting_approval` "needs human"), `subagents.observe`, `account.isolated_profile`, `account.cross_account_resume` |

**Accounts.** agy has no config-dir switch: only a different `HOME` isolates it (verified: `HOME=<scratch> agy models` says "Please sign in"), and a different `HOME` would also change git, ssh and every tool the agent runs, while the OAuth token file is rewritten on refresh (a link to it would not stay shared). Pyre therefore runs agy on the node's own login (`credential_kind=ambient`) and does not advertise `account.isolated_profile`. Account selection never balances agy (`contracts.ManagedLogins`): every launch binds the own login under any `--default-account`; an explicit account must be that login (a google account whose rota_ref is the sign-in's email, read from the token file's id_token claims). The usage view shows one "your own login" card under google per machine with agy; placement does no account dry runs for agy, and the rebalancer never moves ambient runs.

**How acceptance is known.** Bracketed paste of `[pyre-msg:<id>] <text>` into the idle, empty prompt (marker or the text's end must show in the input box before Enter), then the marker in a USER_INPUT step of `brain/<conversation>/.system_generated/logs/transcript_full.jsonl` (`agy.transcript.user_input`, ref `<conversation>#<step>`), which agy appends as soon as the prompt is submitted. An `attempting` record is fsynced first; reconcile clears an unsent draft (rejected) or reports indeterminate; nothing is replayed.

**Busy.** A running turn, a dialog, a draft or an attached human → `queued` (the engine retries at the next idle). agy's own input queue (typing while busy) is not used.

**State.** Hooks (PreInvocation…Stop) plus the screen footer: `esc to cancel` under the input box → `running`; `? for shortcuts` → `waiting_input` (also after an interrupt, which fires no hook, once the last hook is 3 s old; with a draft in the box the footer shows only the model, and no `esc to cancel` for 3 s also ends the turn, since a running turn keeps `esc to cancel` over a draft); a trust or permission dialog (no input box, `↑/↓ Navigate`) → `waiting_approval` "needs human"; pane dead → `exited`. The native id (conversation) arrives with the first hook, i.e. with the first prompt.

**Models.** `--model` takes the ids `agy models` lists (cached for an hour; the default is settings.json `model`); `--effort` low…max.

**Limitations.** Screen parsing is calibrated on agy 1.4.2 (Probe flags other versions). Usage and limits are not observed (no LimitEvent). The configuration manager has no agy overlay (environment profiles are claude/codex only); connection grants apply as for shells. Mac and other machines are untested (agy is installed on server only).

## Model and effort (all agent harnesses)

`pyre start --model M --effort E` (MCP `start_session` `model`/`effort`, the TUI form's Model and Effort rows) travel to the node as `--pyre-model=` / `--pyre-effort=` start args, which the node removes, records on the run (`AdapterData` `pyre.model` / `pyre.effort`, shown in session details) and hands to the adapter as `ResolvedLaunch.Model/Effort`. A resume keeps the run's model unless a new one is given. Nothing chosen launches exactly as before. Mapping: Claude `--model <alias|name> --effort <level>`; Codex the thread's `model` and `model_reasoning_effort` (overriding `-m` and a managed effort overlay); agy `--model --effort`. Nodes list choices in `HarnessInfo.Models/Efforts` (`pyre models`, MCP `list_models`): Claude a static alias list (opus, sonnet, haiku, fable), Codex `codex debug models --bundled` (the catalog shipped with the binary: no network, no refresh of the owner's login), agy `agy models` (refreshed in the background so the probe stays fast). The CLI refuses `--model/--effort` for a node whose pyred lists no choices for the harness (an older pyred would pass the args to the harness, which exits on the unknown option), placement skips such nodes when a model is asked for, an effort the harness does not offer there is refused (CLI and node), and the Codex-to-Claude fallback drops the Codex model (and an effort Claude lacks), saying so in the prompt.

## Daemon wiring (summary)

```go
tmuxSock := filepath.Join(pyreState, "tmux", "pyre.sock") // <=100 bytes, dir 0700
claudeA, _ := claude.New(claude.Options{NodeID, TmuxSocket: tmuxSock, SSHAlias,
    HookCommand: []string{pyredPath, "hook", "claude"}, ProfileAsConfigDir: true,
    TrustWorkspace: ownerTrusts /* optional */, StopHookSteer: false})
codexA, _ := codex.New(codex.Options{NodeID, TmuxSocket: tmuxSock, SSHAlias, StateRoot: filepath.Join(pyreState, "rt")})
agyA, _ := agy.New(agy.Options{NodeID, TmuxSocket: tmuxSock, SSHAlias, WorkspaceRoot: filepath.Join(pyreState, "agy"),
    HookCommand: []string{pyredPath, "hook", "agy"}, MCPCommand: []string{pyreCLI, "mcp"}, TrustWorkspace: ownerTrusts})
shellA, _ := shell.New(shell.Options{NodeID, TmuxSocket: tmuxSock, SSHAlias})
// `pyred hook claude <Event> <state-dir>`:
os.Exit(claude.HookMain(os.Args[3:], os.Stdin, os.Stdout))
// WP13: accounts.Config{CodexReader: codexA}
```

Each `ResolvedLaunch.StateDir` is a per-run private dir (created 0700). Persist the returned `RunHandle`; call `Recover` after a restart (never `Launch` again).
