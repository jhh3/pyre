# Work-streams vault

This directory is the data vault for the `ws` work-stream registry
(created by `ws init`; the tool itself is the `ws` plugin / CLI).

- `streams/<slug>/` — one directory per work stream:
  - `STREAM.md` — current state (frontmatter: status, worktree, branch, pr, session, pane, next, ...)
  - `VISION.md` — why the stream exists; the north star. Written at registration.
  - `LOG.md` — dated milestones and decisions (`ws log`)
  - `tasks/tN-*.md` — one file per stream-level task
- `inbox/<slug>/` — unread messages for a stream (`ws msg`); `read/` after delivery
- `todos.md` — loose personal to-dos (`ws todo`)
- `index.html` — generated dashboard (auto-refreshes; regenerated on every ws mutation)
- `CLAUDE.md` / `AGENTS.md` — the HQ manager-agent persona: start any agent in
  this directory and it becomes your work-stream manager

Useful commands: `ws status`, `ws brief`, `ws resume <slug>`, `ws help`.

This vault is a git repo; `ws sync` commits and pushes it (best-effort).
It is also a valid Obsidian vault — point Obsidian here and build a Base over
`streams/*/STREAM.md` frontmatter for table/board views.
