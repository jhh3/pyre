# WP01 runtime spike scripts

Reproducible probes behind the runtime spike that led to decision I07 (the real-host report is not
kept in this repository).
They exist to produce evidence, not to be product code. Every script uses an isolated tmux
socket (`tmux -S <scratch>/tmux.sock` or `-L pyre-spike-*`), a scratch working directory, and
per-session flags (`--settings`, `--mcp-config`, `-c`). None of them edits `~/.claude/settings.json`,
`~/.codex/config.toml`, credentials, or the default tmux server. Real model turns use the existing
login with tiny synthetic prompts; Claude sessions write their usual transcripts under
`~/.claude/projects/<encoded scratch path>/` and Codex threads write rollouts under `~/.codex/sessions`.

Prerequisites: `claude` 2.1.289 at `~/.local/bin/claude`, `codex` 0.160.0 on `PATH`, tmux ≥ 3.4,
python3. The first launch in a new scratch directory shows Claude's workspace-trust dialog, which
`ctl.py accept-dialogs` handles.

## Claude (`claude/`)

| File | Purpose |
|---|---|
| `ctl.py` | Stateless "node controller": `ensure` (launch or reattach), `state` (hook-derived run state), `deliver ID TEXT --mode paste\|stop-inject\|channel`, `reconcile ID`, `pending`/`answer` (approval broker), `accept-dialogs`, `clients`, `capture`, `stop`. Delivery journal is written (fsync) before dispatch. |
| `hook.py` | Hook recorder, configured only through `claude --settings <scratch>/settings.json`. Appends events to `events.jsonl`; implements the one-shot Stop-hook injection and the PermissionRequest approval broker. |
| `channel_server.py` | Stdlib MCP server that declares `experimental["claude/channel"]` and pushes `notifications/claude/channel` for files dropped into `channel-inbox/`. |
| `mac_input.sh [dir]` | Input acceptance suite: idle paste, multi-line paste, busy paste (native queue), Stop-hook injection, crash after/before Enter + reconcile, subagent, approval dialog, approval broker, exit + `--resume`. About 14 tiny turns. Works on Linux too. |
| `mac_channel.sh [dir]` | Channel suite: consent dialog, idle/busy/while-dialog delivery, model trust behaviour. |
| `mac_longpaste.sh` | A 60-line paste collapses into `[Pasted text #1]` in the UI; checks how it arrives in the hook and transcript. |
| `mac_persist.sh` | SIGKILL the controller, show the harness survives and a fresh controller reattaches; local native attach from a second terminal; paste delivery queues while a human is attached. |
| `claude_probes2.sh` | `--input-format stream-json` with a client-chosen `uuid`, and Claude's own `--bg` background sessions (it stops and removes only the session it creates). |
| `last_reply.py DIR [N]` | Print the last assistant replies of a spike run, for evidence. |

## Codex (`codex/`, `codex-schema/`)

Start an app-server that Pyre owns: `bash codex/t.sh new-session -d -s as "codex app-server --listen unix://$TMPDIR/pyre-wp01/codex/as.sock"`.
On Linux the socket's parent directory must be mode 0700.

| File | Purpose |
|---|---|
| `asclient.py` | Stdlib JSON-RPC client over stdio (newline JSON), the unix socket (HTTP upgrade to WebSocket, one JSON-RPC message per text frame), or `codex app-server proxy --sock`. Includes redaction helpers. |
| `experiments.py` | `basic`, `busy`, `interrupt`, `approval`, `crash`, `reconcile`, `resume`, `subagent`, `watch`, `say`. Raw logs go to scratch `logs/`; they may contain account data. |
| `exp_extra.py` | `dedupe` (clientUserMessageId), `longturn-*` (a turn survives controller exit), orphaned/reattached approvals, shared approvals, queue, status. |
| `summarize_logs.py` | Reduces raw logs to redacted protocol traces (the original traces are not kept in this repository). |
| `codex-schema/methods-0.160.0.json` | Method index taken from `codex app-server generate-json-schema --out DIR` (the full output is 314 files, 4.3 MB, so it is regenerated rather than committed; `generate-ts --out DIR` gives 3.0 MB). |

Native TUI on the same app-server: `codex resume <thread-id> --remote unix://<sock> -C <cwd>`.

## HAPI (`hapi/run-hapi.sh`)

`setup | start | probe | spawn | send <sid> [localId] | stop | verify`. It rsyncs the pinned
checkout (86c88df) to scratch, runs `bun install`, and starts the hub (127.0.0.1:37006, relay and push
off, `HAPI_HOME` in scratch) and the runner. `verify` checks the checksums of global Claude/Codex config.

## Evidence collection

`collect_evidence.sh` copies the hook events, delivery journals, channel log and console logs
(synthetic content only, redacted) into `docs/evidence/runtime/logs/`. It never copies transcripts.
