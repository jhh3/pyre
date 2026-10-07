# hive

> Part of [Pyre](../../README.md), in `tools/hive`. Optional: Pyre works with or without it, and with a separately installed copy. Install the bundled copy with `scripts/install/install.sh --with-tools` (an existing install is detected and left alone), or build it with `make tools`. The bundled launcher is `<prefix>/tools/hive/bin/hive`; hooks, crons and vaults are still set up only by `hive install`, which you run yourself.

Unified session memory, workstreams, worktrees & lifecycle for coding agents
(Claude Code + Codex), synced across machines via private git vaults.
Successor to `jhh3-mem`, `ws`, `wt`, and (removed, not absorbed) agent-deck.
Full design: [docs/PLAN.md](docs/PLAN.md).

## Verbs

| | |
|---|---|
| `hive ask "<q>"` | cited answer over all memory (`--repo K` to hard-scope, `--raw` for hits only); citations are binary-validated — uncited answers are discarded |
| `hive grep <pat>` | verbatim search over raw transcripts; `--all-machines` for the fleet, `-i` case-insensitive |
| `hive search / show <uid> / timeline` | reranked hits (no LLM) / one full entry / faceted browse |
| `hive why <path>[:line]` · `hive session <sha\|pr>` | commit ↔ session xref via `Claude-Session:` trailers |
| `hive resume-context <slug>` | deterministic ≤4KB machine-switch brief |
| `hive brief` | cross-machine "what was I doing everywhere" |
| `hive wt open\|ls\|rm\|pick\|repos` | worktree + tmux + stream birth/death (`rm` archives before destroying) |
| `hive ws …` · `hive mem …` | vendored ws / jhh3-mem, byte-identical behavior |
| `hive done\|park\|pin` · `note\|forget\|wrong` | intent + memory hygiene |
| `hive tick / sync / raw import\|status\|prune / enrich --uid` | plumbing (normally cron-driven) |
| `hive doctor / status / install / uninstall / migrate / rebuild --to` | admin |

## Operational model — two actors

1. **Hooks** (fail-open, bounded): capture + enqueue only. No LLMs, no
   network. Hard timeouts, always exit 0, per-module breakers.
2. **`hive tick`** — one cron (`*/5`, plus `@reboot … tick --boot`) running
   sweep → reap → enrich → sync → janitor under a `systemd-run --scope`
   with `MemoryMax=2G` / `CPUQuota=50%` (configurable, `[tick]`). Losing
   every hook loses latency, never data; the sweep is also the entire Codex
   capture story (`~/.codex/sessions`).

## Storage

- `~/streams` — streams vault: `streams/`, `mem/lanes/<machine>/*.ndjson`
  (append-only, single-writer per lane — that is the whole conflict
  strategy), `mem/tombstones/`, `handoffs/`, `meta/epoch`. Redacted.
- `~/.local/share/hive/vault-raw` — raw vault:
  `raw/<machine>/<harness>/<session-id>/<seq>.jsonl.zst` + `manifest.json`.
  Unredacted (resume fidelity; private repo — decision on record).
- `~/.local/share/hive/index.db` — derived SQLite (WAL/FTS), rebuildable
  from the lanes with `hive rebuild --to <path>`.
- `~/.config/hive/hive.toml` — config; see
  [docs/hive.toml.example](docs/hive.toml.example).

## Install

    claude plugin marketplace add jhh3/hive
    claude plugin install hive@hive
    hive install                 # config stub, ~/.local/bin/hive shim, crons, vault dirs
    hive install --take-over     # + ws/jhh3-mem alias shims, legacy crons removed
    hive install --remove-agent-deck   # strip agent-deck hooks from settings.json
    hive doctor

Every mutating admin verb takes `--dry-run`; crontab and settings.json are
backed up before editing (crontab backups under `~/.local/state/hive/`).

## Second machine

1. Install the plugin + `hive install` (as above).
2. Clone the private vaults: streams vault → `~/streams`, raw vault →
   `~/.local/share/hive/vault-raw` (or point origin remotes at them).
3. `hive rebuild --to ~/.local/share/hive/index.db` — index from lanes.
4. `hive doctor` until green; `hive sync` / the tick keep it current.
5. Resume work: `hive brief`, then `wt <branch>` (worktree recreated from
   the stream record) or `hive raw import <session-id>` for a true
   `claude --resume`.

## Dev workflow

- `HIVE_DEV=1 hive …` — the `~/.local/bin/hive` shim runs this checkout
  (`plugins/hive/bin/hive`) instead of the newest installed plugin copy.
- Tests: `python -m pytest tests/` (no network, no real DB or $HOME); from the
  Pyre root, `make tools-test`. The vendored jhh3-mem shell suite
  (`plugins/hive/vendor/jhh3-mem/tests/run.sh`) assumes GNU coreutils (Linux).
- Ship: bump `version` in `plugins/hive/.claude-plugin/plugin.json`, push,
  then `claude plugin marketplace update hive && claude plugin update hive@hive`.

## Safety

- Kill switch: `HIVE_DISABLE=1` (hooks no-op); per-hook breakers trip after
  repeated failures and are reported by `hive doctor`.
- Logs: `~/.local/state/hive/hooks.log`, `~/.local/state/hive/tick.log`.
- `hive doctor` checks deps, DB, breakers, tick age, crontab, both vaults,
  vendored tools, and pending decommission steps.

## Decommission map

| Before | Now |
|---|---|
| `jhh3-mem` CLI + cron | `hive mem …` (vendored) · capture/enrich via the tick · `hive ask/search` |
| `ws` cron (sync/watchdog/up) | `hive ws …` (vendored) · tick sync + reap + `tick --boot` |
| `wt.zsh` (`_WT_REPOS`) | `hive wt …` + `[repos.*]` in hive.toml (shell shim keeps the `cd`) |
| agent-deck hooks/TUI | removed — `wt ls`, `ws serve`, `hive doctor` cover visibility |
| jhh3-mem SQLite as source of truth | NDJSON lanes canonical; pre-hive backup `memory.db.pre-hive` kept forever next to the legacy store (`~/.local/share/jhh3-mem/`) |

Codex parity: transcripts are swept like Claude Code's; `hive install`
maintains a marked block in `~/.codex/AGENTS.md` teaching Codex the query
verbs, and the wt/spawn wrapper prepends the same digest Claude Code gets
at SessionStart (`hive.codexparity`).

## License

MIT, as part of Pyre (see [LICENSE](../../LICENSE)). Standard library Python only; the vendored marked and xterm.js browser assets (MIT) are listed in Pyre's [NOTICE](../../NOTICE).
