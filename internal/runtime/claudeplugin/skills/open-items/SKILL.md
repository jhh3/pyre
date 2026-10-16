---
name: open-items
description: List every pending decision or question this session and its children have for the owner, numbered so he can answer by number. Use when the owner types /open-items or asks what is waiting on him.
---

# /open-items — what is waiting on the owner

1. **Collect.** Your own open questions and decisions; `find_asks` (open asks from this
   session and its children); the children's latest reports (`list_sessions`, then
   `read_session --tail` on any child that is idle or waiting); approvals pending
   (`list_approvals`). Drop anything already answered or decided — a decided ask is final.
2. **Print a numbered list**, most urgent first. For each item:
   - **Context** — two lines a newcomer can follow: what it is and why it matters.
   - **Recommendation** — what you would do, in one line.
   - **Default** — what happens if he does not answer (and by when, Pacific time).
   - **Links** — PR / file / ask / session IDs.
3. Tell him to answer by number, e.g. `1. yes  2. no  3. b`. When he answers, act on each
   immediately and confirm in one line per item.
4. If nothing is pending, say so in one line. No padding.

Anything that needs his decision while he is away goes through `ask_owner` (a topic key,
context, 2–4 options, a default you will take; no default for irreversible actions) —
never a plain message, and never a repeat of a topic already asked.
