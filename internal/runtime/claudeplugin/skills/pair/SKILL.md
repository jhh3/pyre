---
name: pair
description: Ask one Claude and one Codex session the same question in parallel, compare their answers, and give one recommendation with the disagreements listed. Use when the owner types /pair or wants a second opinion from both harnesses.
argument-hint: "[question]"
---

# /pair — Claude and Codex, one recommendation

Question: $ARGUMENTS (if empty, use the decision currently under discussion).

1. **One identical brief.** The question, the relevant context (paths, links, constraints),
   and: "Judge / brainstorm only — do not write or change code. Answer verdict first,
   then reasons, risks and what would change your mind."
2. **Start both at once** with `start_session` (or `pyre start`): one `harness: claude`,
   one `harness: codex`, same node, same working directory, same project as this session,
   same prompt.
3. **Wait for both.** Their reports arrive automatically; use `wait_reply` or the reports —
   do not poll. If one fails or times out, say so and continue with the other.
4. **Compare.** Where they agree, that is the answer. Where they differ, check the facts
   yourself (read the code, run the probe) and break the tie with evidence.
5. **Answer the owner, verdict first:** one recommendation, then the disagreements as a
   short list (Claude said / Codex said / your call and why). Archive both sessions when
   done.
