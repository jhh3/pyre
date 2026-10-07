"""hive — unified session memory, workstreams, worktrees & lifecycle.

Modules:
  core   — config, db, identity ledger, job queue, lanes/sync, tick, harness
  mem    — capture, enrichment, index, injection (from jhh3-mem)
  ws     — streams, mail, watchdog, serve (from ws)
  wt     — worktree + tmux + session birth/death (from wt.zsh)
  query  — ranking, search/show/timeline, ask, grep
"""

__version__ = "0.1.0"
