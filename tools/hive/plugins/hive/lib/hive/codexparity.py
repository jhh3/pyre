"""Codex injection parity (PLAN §5 M6): memory flowing INTO Codex sessions.

Codex transcripts are already captured by the tick sweep; this module covers
the other direction. Two pieces:

- ensure_agents_md(): maintain a marked block in ~/.codex/AGENTS.md teaching
  Codex to use hive from the shell. Idempotent, and content outside the
  markers is never touched. Called from `hive install`.
- digest(cwd): repo-scoped context block for prepending to a codex session's
  first prompt (the wt/spawn wrapper's hook point) — the vendored SessionStart
  injector reused verbatim, so Codex sees exactly what Claude Code would.
"""

from __future__ import annotations

import subprocess
from pathlib import Path

from . import util, vendored

# Module-level so tests monkeypatch it; only `hive install` writes the real one.
AGENTS_MD = Path.home() / ".codex" / "AGENTS.md"

BEGIN = "<!-- hive:begin -->"
END = "<!-- hive:end -->"

BLOCK = f"""{BEGIN}
## Recalling past work with hive

`hive` is a local CLI over the unified memory of past agent sessions
(every repo, every machine). Run it from the shell:

- `hive ask "<question>"` — cited answer over past work. Use when: "have we
  hit this before", "why is it built this way", unfamiliar code, or you are
  about to re-derive something that smells previously solved.
- `hive grep <pattern>` — verbatim recall from raw transcripts (exact error
  strings, flags, SQL).
- `hive search <terms>` — reranked hits, no LLM.
- `hive show <uid>` — drill into any cited [m:<uid>] before acting on it.
- `hive brief` — cross-machine "what was I doing everywhere".

An empty result is not empty memory — most history is search-only, so try
`hive ask` before concluding there is nothing.
{END}"""


def _spliced(text: str) -> str | None:
    """`text` with the hive block ensured; None if the markers are malformed
    (only one present, or END before BEGIN) — then we refuse to touch it."""
    b, e = text.find(BEGIN), text.find(END)
    if b == -1 and e == -1:
        if text and not text.endswith("\n"):
            text += "\n"
        sep = "\n" if text.strip() else ""
        return text + sep + BLOCK + "\n"
    if b == -1 or e == -1 or e < b:
        return None
    return text[:b] + BLOCK + text[e + len(END):]


def ensure_agents_md(dry_run: bool = False) -> bool:
    """Ensure the marked hive block in AGENTS_MD. True iff changed (or would
    change under dry_run). Never touches content outside the markers."""
    try:
        old = AGENTS_MD.read_text()
        existed = True
    except FileNotFoundError:
        old, existed = "", False
    new = _spliced(old)
    if new is None:
        util.eprint(f"hive: malformed hive markers in {AGENTS_MD}; not touching it")
        return False
    if existed and new == old:
        if dry_run:
            print(f"hive: {AGENTS_MD} block already current")
        return False
    action = ("replace hive block in" if BEGIN in old
              else "append hive block to" if existed else "create")
    if dry_run:
        print(f"hive: [dry-run] would {action} {AGENTS_MD}")
        return True
    AGENTS_MD.parent.mkdir(parents=True, exist_ok=True)
    AGENTS_MD.write_text(new)
    print(f"hive: {action} {AGENTS_MD} — done")
    return True


def digest(cwd: str) -> str:
    """Repo-scoped context block for a codex session's first prompt.

    The vendored SessionStart injector, verbatim — Codex parity means the
    same bytes Claude Code would get. Empty string on any failure or when
    there is nothing to inject; never raises (a launch must not block)."""
    try:
        res = vendored.mem(["explain-inject", "--cwd", cwd])
    except (OSError, subprocess.TimeoutExpired):
        return ""
    if res.returncode != 0:
        return ""
    return res.stdout or ""
