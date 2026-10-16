---
name: ship
description: Get a PR or branch merged and deployed safely, once, then verify the running version. Use when the owner types /ship or asks to merge and deploy something.
argument-hint: "[PR or branch]"
---

# /ship — merge and deploy safely

Target: $ARGUMENTS (default: this session's current PR or branch). The repo's own
AGENTS.md / CLAUDE.md / release docs win wherever they differ from this procedure.

1. **Evidence first.** Before asking the owner anything, state: how it was tested (real
   run, not just unit tests), who reviewed it, and the root cause for a fix. Missing
   evidence is work to do now, not a question.
2. **Bring it current.** Rebase on or merge main, resolve conflicts, push. Wait for CI and
   the required reviews on the exact head SHA (check-runs API, not a guess); fix failures
   and review findings and repeat.
3. **Merge per the repo's rules** — merge queue when it has one, otherwise its documented
   merge method. Several PRs ready together: merge them all first, then deploy once.
4. **Deploy exactly once.** First check that no other deploy is in progress (the repo's
   deploy status / workflow runs); if one is, wait for it — never overlap deploys. Then
   deploy by the repo's documented path.
5. **Verify.** Confirm the running version matches the merged SHA (health/version
   endpoint, release tag, logs) and watch it briefly for errors.
6. **Report** in one block:
   - PR: URL
   - Pushed: branch @ SHA
   - CI: state (run IDs)
   - Merged / queued: yes, how
   - Deployed: where, and the verified running version
   - Risk: one line

Prod config changes or anything irreversible beyond the repo's normal deploy need the
owner's explicit go-ahead (`ask_owner`, no default).
