---
name: ws
description: Work-stream registry for coding agents. Use when registering or updating a work stream, logging progress or decisions, managing stream tasks/subtasks, messaging another agent's stream, checking what work is in flight (status/brief), resuming a stream, or when a session-start note says this directory is (or could be) a registered stream. Triggers - "register this work stream", "update the stream", "ws status", "message the other agent", "what's in flight", "check your inbox".
---

# ws — work-stream registry

`ws` is a CLI-backed registry of work streams shared by the human and every
coding agent on this machine (any harness). It lives in a markdown vault at
`$WS_ROOT` (default `~/streams`). **`ws help` is the canonical command
reference** — run it for exact syntax. This skill covers when and how to use
it well.

## When to register

Register a stream when the work will **outlive this session or span multiple
PRs** — a feature build, a migration, an investigation with follow-ups.
**Skip registration for quick one-offs** (a single fix, a question, a lookup).
The user may also explicitly tell you to register; do so immediately.

```
ws register <slug>        # from inside the worktree; captures everything itself
```

Right after registering, write `VISION.md` in the stream directory (the
register output prints the path): 5–15 lines on **why this work exists, the
north star, and constraints/decisions** a future agent must know. Then set
the first step: `ws update --next "..."`.

## The four everyday habits

Inside a registered worktree the stream is inferred from cwd — no slug needed:

1. `ws log "landed toolbar PR #123"` — dated milestone/decision notes (multi-line: `ws log -` with stdin).
   And when you **open a PR**, track it: `ws pr add '#123'` — streams often span several PRs and the dashboard shows each one's state (open PRs on your current branch are also auto-captured at register/update time). `ws prs` lists every PR across all in-flight streams with live state/CI/title (merged and closed hidden; `--md` prints a pasteable checklist).
2. `ws update --next "wire the crop handles"` — whenever your picture of "what's next" changes; also `--status blocked|review|parked|done|active` on state changes.
3. `ws task add "text reflow"` / `ws task done t2` — stream-level milestones.
4. `ws inbox read` — when told you have mail (hooks, a wake nudge, or the 📬 banner any ws command prints); **act on messages, don't just read them**.

## What goes where

- **VISION.md** — the why and north star. Written once at registration; update only on direction changes. Never narrate progress here.
- **DOC.md** (via `ws doc`) — the living, human-readable deep-dive: what's in flight, current shape, decisions, links. On any substantial stream, scaffold it (`ws doc`) and keep it current at milestones — it renders to HTML and is linked prominently from the dashboard, so it's the first thing the user opens. Distinct from VISION (stable why) and LOG (append-only history).
- **LOG.md** (via `ws log`) — dated milestones and decisions. Not turn-by-turn narration.
- **Tasks** — stream-level milestones a human would care about on a dashboard. NOT your internal micro-checklist — keep 5-minute items out of the registry.
- **`next`** — one honest sentence a fresh agent could act on cold.

## Sub-agents and parallel work

Each task is its own file, so parallel sub-agents can update their own tasks
concurrently without conflicts. When spawning a sub-agent for a task, include
in its prompt: "You own task <id> — keep it updated via `ws task update <id>
--status ...` / `ws task done <id>` (run from this worktree)."

## Coordination

- `ws status` — table of every stream (yours and others); ✋ marks streams that need the human. `ws brief` — full dump with a "Needs you" section; use it when asked to manage/triage all work.
- `ws msg <to> "..."` — message another stream, `hq` (the human's mailbox), or a `#channel`. Typed envelopes: `--kind fyi|question|decision|review|reply`, `--sev high` for urgent. If the target's session is dead, `--revive` restarts it so the message lands on boot.
- **Asking the human**: `ws msg hq "..." --kind question --expect-reply` — it surfaces at the top of `ws brief`/`ws status` and in the HQ session. Add `--wait` to block for the answer.
- **Answering**: mail marked "reply expected" MUST be answered before you go idle (`ws msg <sender> --reply-to <id> "..."`) — the Stop hook holds you until you do.
- **Channels**: `ws chan join <name>` subscribes your stream (new traffic only); `ws msg '#name' "..."` broadcasts; posts arrive through the same hook delivery as DMs. Use channels for cross-stream coordination (e.g. `#deploys`), DMs for requests.
- `ws resume <slug>` — attaches if a session is alive, otherwise **creates** the stream's tmux session (`ws-<slug>`) in its home directory — the worktree, or `$WS_ROOT/streams/<slug>` for manual streams — running the right resume command. Manual streams therefore always have a place to "open": their vault dir, where session-start injection gives full stream context.
- `ws up` — boot recovery: restarts the session of **every** in-flight agent stream at once (skips manual/parked/done; `--dry-run` previews, `--install` prints the @reboot cron line). After a machine restart, one command brings the whole fleet back.
- Before going idle, make sure the stream record is current (`ws log` / `ws update --next`) — the Stop hook will hold you if unread mail or an owed reply is pending.

## Harness notes

On Claude Code, Codex (plugin or `ws setup`-written hooks.json), and Gemini
CLI (ws extension), hooks deliver inbox mail automatically and inject stream
context at session start. Any MCP-capable harness can also connect the `ws
mcp` stdio server (tools: ws_status/ws_brief/ws_send/ws_inbox/ws_log/
ws_update; every result carries an unread-mail banner). On a harness with
NEITHER: run `ws inbox read` at natural checkpoints — every ws command prints
a 📬 banner when you have unread mail, and that banner is your delivery
signal.
