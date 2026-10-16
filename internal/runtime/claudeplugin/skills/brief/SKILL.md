---
name: brief
description: Executive status of this session and its child sessions against the project's goals, verdict first. Use when the owner types /brief or asks where things stand.
---

# /brief — executive status

1. **Gather.** The goals: `get_context` / `pyre context` (project brief) or the original
   request. The children: `list_sessions` (state, start time, last activity) and
   `read_session --tail 20` on each running or idle child. Your own progress.
2. **Write it short, verdict first** — the first line answers "on track or not, and why".
   Then:
   - **Done** — results with links (PR URL, files).
   - **Running** — who, what, for how long.
   - **Idle children** — finished or stuck; what you will do with each.
   - **Blocked** — on what.
   - **Needs you** — numbered, each with a recommendation and a default.
   - **Pace** — one honest line: is this fast enough to hit the goal, and if not what
     changes (more parallelism, cut scope, different approach).
   - **Next** — steps with ETAs in Pacific time.
3. **Act, don't just report.** Restart stuck children, archive finished ones, start the
   next lane, push results that are ready — then say what you did.

Plain English, no jargon a newcomer could not follow, no padding.
