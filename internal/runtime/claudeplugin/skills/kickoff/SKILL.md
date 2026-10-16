---
name: kickoff
description: Start a piece of work as a Pyre project, run Claude and Codex children in parallel, and finish with a merge-ready PR and an HTML summary. Use when the owner types /kickoff or asks to kick off, spin up or run a build end to end.
argument-hint: "[what to build]"
---

# /kickoff — project, parallel workers, merge-ready PR

The work is: $ARGUMENTS (if empty, use what the owner just described; ask only if there is nothing to go on).

1. **Plan in one short message, then go.** Verdict-first, plain English: goal, the
   parallel lanes (who does what, Claude or Codex), what "done" looks like. Do not wait
   for approval unless the work is irreversible or the goal is genuinely ambiguous.
2. **Project.** `create_project` (MCP) / `pyre project new <name>` with a short kebab
   name and the repo; then `associate_session_project` with session `self`. Put the goal
   and acceptance criteria in the project brief so children inherit them.
3. **Workers in parallel.** Split into file-disjoint lanes. Start them with
   `start_session` (project set, `fresh`/`worktree` for an isolated branch, harness
   `claude` or `codex` — use Codex for independent review or a second opinion). Cap at
   about 4 concurrent children (fewer on a busy machine: check memory/swap first). Every
   child joins this project. Each brief is self-contained: goal, files owned, acceptance
   criteria, tests to run, "commit, don't merge". Use in-session subagents for quick
   reads and searches instead of new sessions.
4. **Integrate as results arrive.** Children report back automatically; do not poll.
   Review each diff yourself, run the focused tests, fix or send back. Keep going until
   the whole thing works — no half-done lanes handed to the owner.
5. **Deliver a merge-ready PR (never merge unless told).** Follow the repo's PR
   conventions (AGENTS.md / CLAUDE.md, its PR skill if any). When there is UI, record
   videos/screenshots of the real thing and attach them. Docs updates go in the same PR
   or a linked draft PR. Report the PR as: URL, pushed (branch + SHA), CI state, one-line
   risk note.
6. **Summary.** Write a self-contained HTML summary (what shipped, how it was tested,
   open risks, links) to `~/Downloads/<project>.html` and open it (`xdg-open` on Linux,
   `open` on macOS). Times in Pacific.
7. **Close out.** Archive/stop finished children, remove their worktrees (never ones
   with uncommitted or unpushed work — list those instead), and end with the PR line and
   the summary path.

Decisions only the owner can make go through `ask_owner` with a default; keep working on
the default path meanwhile.
