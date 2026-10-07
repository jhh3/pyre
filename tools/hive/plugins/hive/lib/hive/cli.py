"""hive CLI — one front door for memory, workstreams, worktrees, sync, query.

Dispatch is a flat registry so subcommand modules stay import-light: heavy
modules load only when their verb runs (hooks and shell startup must stay
fast). `hive ws …` and `hive mem …` delegate to the vendored implementations
(exact behavior preserved; staged convergence per docs/PLAN.md §2).
"""

from __future__ import annotations

import os
import sys
from pathlib import Path

PLUGIN_ROOT = Path(__file__).resolve().parent.parent.parent


def _vendored_env(tool: str) -> dict:
    env = dict(os.environ)
    env["CLAUDE_PLUGIN_ROOT"] = str(PLUGIN_ROOT / "vendor" / tool)
    return env


def _delegate(tool: str, argv: list[str]) -> int:
    """Exec a vendored tool (ws | jhh3-mem) with its plugin root remapped."""
    binpath = PLUGIN_ROOT / "vendor" / tool / "bin" / tool
    if not binpath.exists():
        print(f"hive: vendored tool missing: {binpath}", file=sys.stderr)
        return 1
    os.environ.update(_vendored_env(tool))
    os.execv(str(binpath), [str(binpath)] + argv)


def main(argv: list[str] | None = None) -> int:
    argv = list(sys.argv[1:] if argv is None else argv)
    if not argv or argv[0] in ("-h", "--help", "help"):
        print(USAGE)
        return 0

    verb, rest = argv[0], argv[1:]

    # Vendored delegations (alias shims `ws` / `jhh3-mem` land here too).
    if verb == "ws":
        return _delegate("ws", rest)
    if verb == "mem":
        return _delegate("jhh3-mem", rest)

    # Lazy imports keep startup <50ms for the common verbs.
    if verb in ("search", "show", "timeline", "ask", "grep", "why", "session",
                "resume-context", "wrong", "note", "forget", "explain-inject"):
        from . import query
        return query.run(verb, rest)
    if verb == "wt":
        from . import wt
        return wt.run(rest)
    if verb == "brief":
        from . import brief
        return brief.run(rest)
    if verb == "tick":
        from . import tick
        return tick.run(rest)
    if verb == "sync":
        from . import sync
        return sync.run(rest)
    if verb in ("raw",):
        from . import raw
        return raw.run(rest)
    if verb in ("enrich",):
        from . import enrich
        return enrich.run(rest)
    if verb in ("doctor", "install", "uninstall", "migrate", "rebuild", "status"):
        from . import admin
        return admin.run(verb, rest)
    if verb in ("done", "park", "pin"):
        from . import intent
        return intent.run(verb, rest)

    print(f"hive: unknown command '{verb}' (try `hive help`)", file=sys.stderr)
    return 2


USAGE = """\
hive — unified session memory, workstreams, worktrees & lifecycle

query
  ask "<q>" [--repo R|--all] [--stream S] [--kind K] [--raw]
                       cited answer over all memories (validated citations)
  search <terms>       reranked hits (no LLM)   [--explain]
  grep <pattern>       verbatim search over raw transcripts [--all-machines]
  show <uid>           full entry + session + drill-downs
  timeline             faceted browse [--stream|--repo|--file|--since|--machine]
  why <path>[:line]    which session wrote this code (commit xref)
  session <sha|pr>     commits/PR -> session
  resume-context <slug> machine-switch payload (cited, <=4KB)
  brief                cross-machine "what was I doing everywhere"

work
  wt [...]             worktree + tmux + session birth/death (see `hive wt -h`)
  ws [...]             full work-stream registry (vendored ws)
  mem [...]            episodic memory CLI (vendored jhh3-mem)
  done|park|pin ...    intent verbs

plumbing
  tick                 one maintenance pass (sweep->reap->enrich->sync->janitor)
  sync                 push/pull vaults now
  raw ...              raw transcript segments (import/fetch/prune)
  enrich --uid U       enrich one entry now
  migrate ...          jhh3-mem -> lanes migration (dry-run first)
  rebuild              rebuild index.db from vaults
  doctor               health: hooks, breakers, lanes lag, last tick
  install|uninstall    wire/unwire crons, shims, hooks on this machine
"""


if __name__ == "__main__":
    sys.exit(main())
