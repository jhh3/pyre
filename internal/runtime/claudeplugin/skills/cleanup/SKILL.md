---
name: cleanup
description: Remove finished worktrees, archive idle finished children, prune caches this work created, and report disk, swap, memory and orphaned processes. Use when the owner types /cleanup or asks to tidy up after the work.
---

# /cleanup — tidy up after this work

Only clean up what this session and its children created. When in doubt, list it
instead of deleting it.

1. **Worktrees.** For each worktree this work created (`git worktree list`): remove it
   only when it has no uncommitted changes (`git status --porcelain`) and no unpushed
   commits (`git log @{u}..` empty, or the branch is merged). List the others with why
   they were kept. `git worktree prune` afterwards.
2. **Children.** Archive finished children (`archive_session`), stop idle ones whose work
   is reported. Never touch a session the owner started or is talking to, and never
   another active session.
3. **Outputs and caches.** Delete stale run outputs, temp dirs and build caches this work
   created (test artifacts, scratch builds). Leave shared caches and anything delivered
   to the owner alone.
4. **Processes.** Look for orphaned processes this work started (dev servers, watchers,
   test runners, browsers) and stop them. Never kill this session's own tmux, pyred, the
   hub, or another session's processes.
5. **Report in one short block:** removed (with space freed), kept (and why), then disk
   (`df -h` for the relevant mounts), memory and swap (`free -h`, or `vm_stat` on macOS),
   and any orphans you found and what you did.
