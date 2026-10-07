# Runtime adapter capabilities (WP07)

Implements decision I07 (runtime report §8–§9): direct native adapters, no HAPI. Code: `internal/runtime/{tmux,shell,claude,codex}`. Contract: `contracts.RuntimeAdapter` (§4 of [contracts](contracts.md)).

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
- The run environment (`PYRE_SESSION_TOKEN`, environment-overlay and connection values, `PATH`) never goes on a command line or into the tmux session environment. The adapter writes it, together with `ResolvedLaunch.Unset`, to a private file (0600, in the 0700 socket dir). The pane runs `<pyred> __pyre_exec_env <file> -- argv`, which reads and deletes the file, applies it, and execs argv in place (same PID). Explicit `Env` wins over `Unset`. Consequences:
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
| `resume.handoff` | contract only | | cross-account → `handoff` |
| `subagents.observe` | verified-real | verified-real | SubagentStart/Stop |
| `transcript.read`, `screen.read`, `hooks.events` | verified-real | verified-real | |
| `requires_trust_dialog` | verified-real | verified-real | untrusted cwd → `waiting_input`, "needs human: workspace trust"; `TrustWorkspace(cwd)` lets the adapter accept it (verified-fake only — accepting writes `~/.claude.json`) |
| `account.isolated_profile` (opt-in) | via WP13 | via WP13 | not exercised here with a second login |
| Not advertised | | | `input.idempotent`, `questions.answer`, `input.channel`, `account.cross_account_resume`, `agent.tools` (daemon's job) |

**How acceptance is known.** Deliver renders `[pyre-msg:<id>] <text>`. Evidence, strongest first: `UserPromptSubmit` hook whose prompt contains the marker (`claude.hook.user_prompt_submit`, ref = hook event id), Stop-hook injection claim (`claude.stop_hook.injected`), transcript `queue-operation enqueue` (`claude.transcript.queue_enqueue`), transcript user record (`claude.transcript.user_record`). A later user record for a queued message is reported in `Observation.Receipts` with `Detail: consumed_at=<RFC3339>` (contract has no receipt field for it yet).

**Gate (refuse → `queued`, nothing journaled).** Pane alive; no writable tmux client attached (input lease); hook state `waiting_input`/`running`/`waiting_on_background`; no pending/expired approval; no human-only dialog (`AskUserQuestion`, `ExitPlanMode`, elicitation); input box visible (waits ≤2 s for the TUI to render) and empty (dim placeholder recognised via SGR 2). A human's draft is never pasted over.

**Busy.** Paste while `running` uses Claude's native queue (enqueue evidence immediately, consumption later) — verified-real Mac and server.

**Idempotency / crash window.** No runtime idempotency key in the TUI. Before any byte is pasted an `attempting` record is fsynced to `<run>/deliveries.jsonl`; the paste must be recognisable in the input box before Enter — a paste into a dialog is ignored there and a bare Enter could approve it. Recognised: the marker, a collapsed `[Pasted text #N …]` (pastes over 800 characters) when the box was empty, or — because Claude shows only the last six rows of a long draft — the box showing exactly the end of the pasted text (whitespace ignored, at least 40 characters; any other text after the paste fails the match). Before pasting, a run window that a detached client left narrow is restored to the launch size (200×50) when no client is attached (`resize-window`, then the window's `window-size` is unset so the next attach still sizes it). The input lease is checked again right before Enter: a human who attached after the paste gets the draft and the result is `indeterminate` with what to do. An unrecognised draft is never submitted or cleared: `indeterminate` with "Needs a human: …", and Observe reports `needs human: Claude's input box holds Pyre's unsent draft of <id>` while a recognised leftover draft is in the box. Deliver of an id that already has evidence returns `accepted` without pasting. Reconcile: evidence → `accepted`; a draft of the message still on the input line (marker visible, or its end matching the normalised tail kept in the journal) → cleared (C-u/BSpace loop; C-u clears only one line of a multi-line draft) → `rejected` (never submitted, safe retry); otherwise `indeterminate`. Never auto-replays. Verified-real (crash before Enter with a 3-line draft, crash after Enter) on Mac and server.

**State.** Hooks (SessionStart, UserPromptSubmit, Pre/PostToolUse, PermissionRequest, Notification, Stop, StopFailure, SubagentStart/Stop, SessionEnd, PreCompact) folded incrementally, plus transcript `system/turn_duration` newer than the last state-changing hook → `waiting_input` (interrupts and TUI denials fire no Stop). Stop with non-empty `background_tasks` → `waiting_on_background`. No SessionStart yet → `starting`, or `waiting_input` "needs human: <dialog>" for a known/unknown dialog. Pane dead → `exited` + exit code; tmux unreachable → `unknown`. The adapter's `ChangeStamp` (size and mtime of `events.jsonl` and the transcript) lets pyred observe a backed-off run as soon as Claude writes something; a native-terminal interrupt fires no hook, so without it the state waited for the idle poll (`Poll.IdleMax`, 30 s). Verified-real on server: `waiting_approval` → `waiting_input` 1.9 s after a native Escape.

**Approvals.** With the broker on (default), the PermissionRequest hook writes `<run>/pending/<rid>.json` (correlated with the matching `PreToolUse.tool_use_id`) and waits up to `ApprovalWait` (120 s) for `<run>/answers/<rid>.json`; claiming is an atomic rename. `Approval.RequestID = rid`, `ApprovalID = apr_` + hash(run, rid); options `allow`/`deny` (+ text as deny message). `Answer` returns `effect=submitted`; Observe reports `effective` (PostToolUse after an allow; the turn moving on after a deny), `superseded` (TUI answered first / hook killed by a TUI "No" while a Pyre answer was pending), `native` resolution without Pyre, `expired` when the hook timed out and the TUI dialog is still up (human must answer there), `withdrawn` when the turn ended. Duplicate or stale answers → `conflict`. An expired approval is never answered by Pyre, but it stays actionable: the run stays `waiting_approval` with reason "permission prompt waiting in its terminal (… attach to answer it, or interrupt to cancel)", `pyre approvals` lists it with the attach/interrupt commands while the run still waits, and the inbox replaces the answer item with an urgent session item ("Answer in its terminal: …", group `terminal:<approval>`). When the prompt ends, the node reports the native outcome; the hub settles the expired approval (resolved/withdrawn) and the item is withdrawn.

**Pre-approved tools.** Every Claude run's `--settings` allows the Pyre MCP messaging tools `mcp__pyre__send_message`, `inbox`, `get_message`, `ack_message` and `wait_reply` (with or without the doorbell), plus the read-only `get_digest` and `search`, the self-only `rename_session` (the hub lets an agent rename only its own session, and never over a title the owner set), and the self-only `flag_attention`/`clear_attention` (an agent marks its own session "needs you" with a one-line reason; the hub refuses other sessions, and a permission prompt for it would itself need the owner). Session control (`start_session`, `stop_session`, `interrupt_session`), `answer_approval`, `reissue_message`, `resolve_delivery`, `publish`, project creation, session-project association and archiving (`create_project`, `associate_session_project`, `archive_session`), document/note writes and connection calls keep Claude's permission prompt. The agent guide's MCP variant tells Claude to reply with `mcp__pyre__send_message` (`reply_to`), because `pyre send` through Bash opens a permission dialog in the default mode.

**Interrupt / stop.** Interrupt sends Escape only when running/awaiting approval — a raw terminal action the daemon must gate with the input lease. Stop pastes `/exit` only into an empty idle prompt with no human attached (verified on screen before Enter), waits for exit, falls back to `TerminateSession`; exit status persisted before the session is removed.

**Resume / IDs.** `ResumeSpec(sameAccount)` → `native_resume` with the same Claude session id; Launch with `Resume` passes `--resume <id>` (same cwd required: transcripts are per project dir). Observe always reports the id from the latest SessionStart.

**Limit events.** StopFailure payloads, transcript API-error records and Claude error lines (`⎿ …`, `/login`, `/upgrade`) near the bottom of the screen are matched for usage limit (`limit reached|<epoch>` gives `ResetAt`), auth failure and rate limiting. Pattern list is narrow and unverified against real limit text (could not be triggered).

**Doorbell (`input.monitor`; on by default in pyred, decision I16).** Introduced by the S-D spike (DOORBELL.md); integrated with the real hub and pyred in doorbell-integration-2026-10-04.md. Verified-real on Mac and server.
- **Wiring:** pyred sets `DoorbellCommand = [<pyred>, inbox-watch]` for Claude runs. It is off with `pyred run --no-doorbell`, with `PYRE_DOORBELL=0`, or when no pyre CLI is found (the run would have no MCP inbox tool). A run whose launch environment disables plugin monitors (`DISABLE_TELEMETRY`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, a third-party provider) drops `input.monitor` from its capabilities (`RunHandle.Data["caps_unavailable"]`).
- **Mechanism:** a generated per-run plugin (`--plugin-dir=<state>/pyre-plugin`) runs `pyred inbox-watch` as a Claude Code monitor. It prints one coalesced line per batch of new mail (no bodies), which wakes the session even with a human attached. The line and the agent guide name the pre-approved MCP tools `mcp__pyre__inbox`, `mcp__pyre__send_message` (reply) and `mcp__pyre__ack_message`. The `pyre inbox` shell command needs a permission prompt and leaves no evidence.
- **Evidence and receipts:** a PostToolUse on `mcp__pyre__inbox`/`get_message` returning the id is `claude.tool.inbox_read`. The node mints an attempt for it and reports one `accepted` receipt with `consumed` set. Re-observing the evidence after a pyred restart changes nothing. The doorbell line itself (`claude.monitor.notified`) never counts as acceptance.
- **No double delivery:** paste and doorbell exclude each other per message. The watcher skips ids that have a paste attempt. Deliver leaves mail the doorbell already announced to the agent's read for `DoorbellPasteHold` (3 min) and then falls back to paste. The engine never pastes an accepted message.
- **Timing:** notifications are queued like mail. They are delivered at turn end or after a dialog closes, never mid-turn.
- **Monitor lifecycle:** one watcher per run. Claude starts it through a shell (`zsh -c "… eval '<cmd>' … && pwd -P"`); the watcher exits when its shell or that shell's parent (Claude) changes. Claude does not restart a dead monitor: `DoorbellAlive` checks the watcher's pid file, and the gate then says the doorbell is down. The monitor is not counted as a Stop background task.

**Limitations.** Paste and screen parsing depend on the Ink TUI (calibrated on 2.1.289; Probe flags other versions). A clean TUI-side approval "Yes" leaves the broker hook waiting until timeout (harmless; effect reported as native). A `TrustWorkspace` acceptance writes Claude's own trust entry. Dev channels and `--bg` are not used.

## Codex (`codex-appserver`)

One Pyre-owned `codex app-server --listen unix://<StateRoot>/codex/<home-key>/codex.sock` per `CODEX_HOME` binding (`ProfileDir`; empty = node default home), run in tmux session `codex-as-<home-key>` so it survives daemon restarts. Refs in `<dir>/refs/<run_id>` (and transient `probe-*`) make the refcount restart-safe; the last ref stops the server (SIGTERM to the process group — npm's node wrapper and the native binary — then SIGKILL). This satisfies WP13's one-process-per-home lease (holder `codex-app-server`). JSON-RPC over the unix-socket WebSocket (`github.com/coder/websocket`), `initialize` with `experimentalApi` and noisy-delta opt-outs. Per run, tmux session `<run_id>` runs `codex resume <thread> --remote unix://<sock> -C <cwd>` as the native TUI.

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
| Not advertised | | | `input.idempotent` (Codex does not dedupe clientId), `hooks.events` |

**How acceptance is known.** `turn/start {threadId, input, clientUserMessageId: <message_id>}` when the thread is idle/unloaded (unloaded threads are resumed first). Evidence: the `userMessage` item echoing `clientId == message_id` (`codex.item.user_message.client_id`, ref = item id). If the turn/start/steer response returned a turn id but the item has not arrived within `DeliverTimeout`, the result is `accepted` with `protocol_ack` (ref = turn id) and the item receipt follows in Observe.

**Busy.** Thread `active` (or an approval pending) → `queued` for `ModeQueue`; `ModeSteer` uses `turn/steer` with the active turn id (mismatch / no active turn → `rejected`, safe retry). If the thread turns busy between the status check and `turn/start` (e.g. a human typed in the TUI), Codex folds the message into the active turn; the detail says "joined the active turn". Native TUI attachment does not block structured delivery (they share the app-server queue).

**Idempotency / crash window.** An `attempting` record is fsynced before every send. Codex does not dedupe clientIds, so Deliver searches the thread for the id when an earlier attempt exists and refuses (`indeterminate`) to resend an id whose outcome is unknown. JSON-RPC error → `rejected`; lost connection/timeout after the write → `indeterminate`. Reconcile searches up to 20 pages of `thread/turns/list`. Verified-fake (send-then-die, new adapter Recover + Reconcile) and verified-real (restart + Recover + Reconcile, single app-server).

**State.** `thread/status/changed`: `idle`/`notLoaded` → `waiting_input`, `active` → `running`, `activeFlags waitingOnApproval|waitingOnUserInput` → `waiting_approval`, `systemError` → `unknown`; app-server unreachable → `unknown` (Observe reconnects to a live server but never starts one); stopped → `exited`.

**Approvals.** Server requests `item/commandExecution|fileChange|permissions/requestApproval` and `item/tool/requestUserInput` become approvals with `RequestID = as<app-server pid>:<rpc id>:<itemId>`. Options come from `availableDecisions` (`accept`, `acceptForSession`, `decline`, `cancel`, `acceptWithExecpolicyAmendment`, …; `allow`/`deny` accepted as aliases). Answered once; `serverRequest/resolved` is the truth: after Pyre answered → `effective`, otherwise `native` (superseded). Other server requests are left to a connected TUI. After an app-server restart pending requests are re-sent with new ids (old ids → `not_found`).

**Interrupt / stop.** `turn/interrupt` (bounded; it never answers for a finished turn) then `thread/backgroundTerminals/clean` (commands outlive an interrupt). Stop interrupts an active turn, `thread/unsubscribe`s, terminates the TUI session and drops the ref.

**Resume / IDs.** Thread ids are stable; `ResumeSpec(sameAccount)` → `native_resume`; Launch with `Resume` calls `thread/resume`. Cross-home resume needs the rollout in the target home's `sessions/` (WP13 shares the store; unproven here) → `handoff`.

**Accounts (WP13).** `ReadRateLimits(ctx, codexHome)` returns the raw `account/rateLimits/read` result through the home's single app-server (starting it under a transient ref when idle and stopping it afterwards); `ReadAccount` returns type/plan only (no email, `refreshToken:false`). Neither requests a token refresh.

**Limitations.** Linux real turns unproven (server not logged in). Experimental methods used: `thread/backgroundTerminals/clean`. Threads report `source` as an app-server client and appear in the user's `codex` history. Using the default home runs a second app-server next to the user's own shared daemon (as in WP01); WP13 profiles avoid that.

## Daemon wiring (summary)

```go
tmuxSock := filepath.Join(pyreState, "tmux", "pyre.sock") // <=100 bytes, dir 0700
claudeA, _ := claude.New(claude.Options{NodeID, TmuxSocket: tmuxSock, SSHAlias,
    HookCommand: []string{pyredPath, "hook", "claude"}, ProfileAsConfigDir: true,
    TrustWorkspace: ownerTrusts /* optional */, StopHookSteer: false})
codexA, _ := codex.New(codex.Options{NodeID, TmuxSocket: tmuxSock, SSHAlias, StateRoot: filepath.Join(pyreState, "rt")})
shellA, _ := shell.New(shell.Options{NodeID, TmuxSocket: tmuxSock, SSHAlias})
// `pyred hook claude <Event> <state-dir>`:
os.Exit(claude.HookMain(os.Args[3:], os.Stdin, os.Stdout))
// WP13: accounts.Config{CodexReader: codexA}
```

Each `ResolvedLaunch.StateDir` is a per-run private dir (created 0700). Persist the returned `RunHandle`; call `Recover` after a restart (never `Launch` again).
