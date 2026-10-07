"""hive query surface — ask, grep, why, session, resume-context, wrong.

`ask` (PLAN §6, decision Q5) is the flagship: retrieval through the vendored
jhh3-mem ranker (which already does current-repo-boosted-global scoring —
other repos at 0.45; a keyword-stripped second search widens thin results),
one read-only SQL query hydrating every candidate, then a haiku-class
synthesis whose citations are BINARY-validated: any cite outside the
candidate set, or no cite at all, discards the whole synthesis and falls
back to the raw hit list. Partial answers are allowed (a trailing
`Not in memory: ...` line) — only a whole-reply NOT_IN_MEMORY abstains.
An LLM answer is never trusted further than its citations.

Everything else here is deterministic: `grep` is verbatim search over local
transcripts, `why`/`session` read the commit-xref (Claude-Session trailers),
`resume-context` is pure file assembly (no LLM), `wrong` demotes an entry
from injection without deleting it.

Simple verbs (search/show/note/forget/explain-inject/timeline) pass straight
through to the vendored CLI so behavior stays byte-identical until cutover.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import sqlite3
import subprocess
import sys
import time
from pathlib import Path

from . import config, db, util, vendored

_PASSTHROUGH = {"search", "show", "note", "forget", "explain-inject"}

_CITE_RE = re.compile(r"\[m:([^\]\s]+)\]")
_TRAILER_RE = re.compile(r"^Claude-Session:\s*(\S+)", re.I | re.M)
_PR_RE = re.compile(r"#(\d+)")

_ABSTAIN = "NOT_IN_MEMORY"
_PROMPT_MAX_BYTES = 12000
_PROMPT_MAX_CANDS = 20     # full candidates offered to synthesis
_SEARCH_LIMIT = 40         # vendored ranker keeps rank order; breadth is cheap
_SECOND_SEARCH_MIN = 8     # fewer hits than this -> keyword-stripped retry
_GREP_FILE_BOUND = 4000
_GREP_MAX_AGE_DAYS = 90
_GREP_SHOW_FILES = 20
_XREF_SCAN_CAP = 500       # commits per repo per janitor run
_RESUME_MAX_BYTES = 4096


class QueryError(Exception):
    """User-facing failure; run() prints it and returns 1."""


def run(verb: str, rest: list[str]) -> int:
    try:
        if verb in _PASSTHROUGH:
            return vendored.mem([verb, *rest], timeout=600, capture=False).returncode
        if verb == "timeline":
            return vendored.mem(["recent", *rest], timeout=600, capture=False).returncode
        if verb == "ask":
            return _ask(rest)
        if verb == "grep":
            return _grep(rest)
        if verb == "why":
            return _why(rest)
        if verb == "session":
            return _session(rest)
        if verb == "resume-context":
            return _resume_context(rest)
        if verb == "wrong":
            return _wrong(rest)
        util.eprint(f"hive: unknown query verb '{verb}'")
        return 2
    except QueryError as e:
        util.eprint(f"hive {verb}: {e}")
        return 1
    except SystemExit as e:          # argparse error paths
        return int(e.code or 0)
    except subprocess.TimeoutExpired:
        util.eprint(f"hive {verb}: subcommand timed out")
        return 1


# ── shared rendering ─────────────────────────────────────────────────────────

def _trim_bytes(s: str, n: int) -> str:
    """UTF-8-boundary-safe byte truncation."""
    b = s.encode("utf-8")
    if len(b) <= n:
        return s
    return b[: max(0, n - 1)].decode("utf-8", "ignore") + "…"


def _date(ts) -> str:
    try:
        return time.strftime("%Y-%m-%d", time.localtime(int(ts)))
    except (ValueError, TypeError, OverflowError):
        return "????-??-??"


def _flat(s: str) -> str:
    return " ".join(str(s).split())


def _hit_line(c: dict) -> str:
    title = _trim_bytes(_flat(c.get("title", "(untitled)")), 120)
    return (f"  {_date(c.get('ts', 0))} · {c.get('kind', '?')} · "
            f"{c.get('repo_key', '?')} · {title} · {c.get('uid', '?')}")


def _print_hits(cands: list[dict], limit: int | None = None) -> None:
    for c in cands[:limit] if limit else cands:
        print(_hit_line(c))
    if cands:
        print(f"drill down: hive show {cands[0].get('uid', '<uid>')}")


def _parse_ndjson(text: str) -> list[dict]:
    out = []
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        row = util.read_json_line(line)
        if isinstance(row, dict) and row.get("uid"):
            out.append(row)
    return out


# ── ask ──────────────────────────────────────────────────────────────────────

def _ask(rest: list[str]) -> int:
    ap = argparse.ArgumentParser(prog="hive ask", description="cited answer over memory")
    ap.add_argument("question")
    ap.add_argument("--repo", help="hard-scope retrieval to one repo_key")
    ap.add_argument("--all", action="store_true",
                    help="all repos (the default: current-repo-boosted-global)")
    ap.add_argument("--kind")
    ap.add_argument("--limit", type=int, default=_SEARCH_LIMIT)
    ap.add_argument("--raw", action="store_true", help="show the hit list, skip synthesis")
    ap.add_argument("--model")
    ap.add_argument("--explain", action="store_true",
                    help="print retrieval diagnostics on stderr")
    args = ap.parse_args(rest)

    cands, n1, n2 = _retrieve(args)
    try:
        return _answer(args, cands)
    finally:
        if args.explain:
            lane = "active" if _semantic_active() else "inactive"
            util.eprint(f"retrieval: {len(cands)} hits "
                        f"(search1={n1}, search2={n2} merged), "
                        f"semantic lane: {lane}")


def _retrieve(args) -> tuple[list[dict], int, int]:
    """Vendored search, widened by a keyword-stripped second pass when thin.

    Merge is by uid keeping best rank: search-1 order is preserved, search-2
    contributes only uids search-1 missed, in its own rank order. No LLM.
    """
    cands = _search(args.question, args)
    extra: list[dict] = []
    if len(cands) < _SECOND_SEARCH_MIN:
        kw = _keyword_query(args.question)
        if kw and kw != args.question:
            extra = _search(kw, args)
    known = {c["uid"] for c in cands}
    merged = cands + [c for c in extra if c["uid"] not in known]
    return merged, len(cands), len(extra)


def _search(terms: str, args) -> list[dict]:
    # The vendored ranker already implements decision Q5 — --all-repos keeps
    # other repos at a 0.45 multiplier below the current one.
    mem_args = ["search", terms, "--json", "--limit", str(args.limit),
                "--all-repos"]
    if args.repo:
        mem_args += ["--repo", args.repo]     # explicit --repo beats --all-repos
    if args.kind:
        mem_args += ["--kind", args.kind]
    try:
        res = vendored.mem(mem_args, timeout=60)
    except (OSError, subprocess.TimeoutExpired) as e:
        raise QueryError(f"retrieval failed: {e}")
    return _parse_ndjson(res.stdout or "")


_STOPWORDS = frozenset(
    "a about after again all also am an and any are as at be because been "
    "before being but by can could did do does doing down for from had has "
    "have having how i if in into is it its just me more most my no nor not "
    "now of off on once only or other our out over own same should so some "
    "such than that the their them then there these they this those through "
    "to too under until up very was we were what when where which while who "
    "whom why will with would you your".split())


def _keyword_query(question: str) -> str:
    """Stopword/punctuation-stripped variant: identifiers + rare words, <=8 terms."""
    terms: list[str] = []
    seen: set[str] = set()
    for tok in re.findall(r"[A-Za-z0-9_][\w./-]*", question):
        low = tok.lower()
        if low in seen:
            continue
        identifier = bool(re.search(r"[\d_./-]", tok))
        if not identifier and (low in _STOPWORDS or len(low) < 3):
            continue
        seen.add(low)
        terms.append(tok)
        if len(terms) >= 8:
            break
    return " ".join(terms)


def _semantic_active() -> bool:
    """The vendored semantic lane's gate: an OpenAI key AND the vec0 extension."""
    if not os.environ.get("OPENAI_API_KEY"):
        return False
    home = Path(os.environ.get("JHH3_MEM_HOME")
                or Path.home() / ".local/share/jhh3-mem")
    return (home / "lib" / "vec0.so").is_file()


def _answer(args, cands: list[dict]) -> int:
    if not cands:
        print("not in memory (0 hits) — try broader terms with `hive search <terms>`")
        return 1

    if args.raw:
        _print_hits(cands)
        return 0

    model = args.model or os.environ.get("HIVE_ASK_MODEL") or "haiku"
    prompt = _build_prompt(args.question, _hydrate(cands)[:_PROMPT_MAX_CANDS])
    reply = _synthesize(prompt, model)

    if reply is None:
        print("synthesis unavailable (model call failed or timed out) — showing raw hits")
        _print_hits(cands)
        return 0

    # Abstention only on a whole-reply NOT_IN_MEMORY: a partial answer ending
    # in `Not in memory: ...` (or mentioning the token) is NOT abstention.
    # A contract-violating mixed reply ("NOT_IN_MEMORY" + prose) keeps the
    # prose: bare NOT_IN_MEMORY lines are dropped, the rest is validated.
    reply = "\n".join(ln for ln in reply.strip().splitlines()
                      if ln.strip() != _ABSTAIN).strip()
    if not reply:
        print("not in memory")
        _print_hits(cands, limit=5)
        return 1

    # Binary citation validation: at least one cite, every cite known.
    cited = _CITE_RE.findall(reply)
    known = {c["uid"] for c in cands}
    if not cited or any(u not in known for u in cited):
        print("synthesis discarded: uncited/unknown citations — showing raw hits")
        _print_hits(cands)
        return 0

    by_uid = {c["uid"]: c for c in cands}
    print(reply)
    print()
    print("Sources:")
    seen: list[str] = []
    for u in cited:
        if u not in seen:
            seen.append(u)
    for u in seen:
        c = by_uid[u]
        title = _trim_bytes(_flat(c.get("title", "(untitled)")), 120)
        print(f"  [m:{u}] {_date(c.get('ts', 0))} · {c.get('repo_key', '?')} · {title}")
    print(f"drill down: hive show {seen[0]}")
    print(f"useful? pin it: hive pin {seen[0]}")
    return 0


_HYDRATE_COLS = ("uid", "ts", "repo_key", "kind", "title", "learned",
                 "decided", "next_steps", "outcome", "files_touched")


def _hydrate(cands: list[dict]) -> list[dict]:
    """Fill the distilled fields for every candidate in ONE read-only query.

    The search NDJSON carries only title/cont; the prompt wants the full
    fields. All candidate uids are fetched at once — per uid, the current
    (non-superseded, max-source_bytes) row — through a mode=ro URI so
    hydration can never write to or create the live store (db.connect would).
    Any failure leaves the candidates as-is; rank order is preserved.
    """
    uids = [str(c["uid"]) for c in cands if c.get("uid")]
    if not uids:
        return cands
    rows: dict[str, dict] = {}
    try:
        conn = sqlite3.connect(f"file:{db.path()}?mode=ro", uri=True, timeout=5.0)
        try:
            conn.row_factory = sqlite3.Row
            ph = ",".join("?" * len(uids))
            # SQLite bare-column semantics: with max(source_bytes) the other
            # selected columns come from that same max row.
            sql = (f"SELECT {', '.join(_HYDRATE_COLS)}, max(source_bytes) "
                   f"FROM entries WHERE superseded=0 AND uid IN ({ph}) "
                   "GROUP BY uid")
            for r in conn.execute(sql, uids):
                rows[r["uid"]] = {k: r[k] for k in _HYDRATE_COLS}
        finally:
            conn.close()
    except sqlite3.Error:
        return cands
    out = []
    for c in cands:
        full = rows.get(str(c.get("uid")))
        if full:
            merged = dict(full)
            merged.update({k: v for k, v in c.items() if v not in ("", None)})
            c = merged
        out.append(c)
    return out


_PROMPT_HEADER = """\
You are answering a question from a personal engineering memory store.
Rules:
- Answer ONLY from the numbered memory entries below. No outside knowledge.
- Cite every claim inline by copying the exact [m:<uid>] tag from the head of
  the supporting entry — the full uid, never the entry number.
- If the entries only PARTLY answer the question, give the partial answer
  (with citations) and end with one final line: Not in memory: <what is missing>.
- Only if the entries answer NOTHING at all, reply with exactly NOT_IN_MEMORY
  and no other text. Never combine NOT_IN_MEMORY with an answer.
- Be concise: a few sentences, no preamble.

Memory entries:
"""


def _build_prompt(question: str, cands: list[dict]) -> str:
    tail = f"\nQuestion: {question}\n"
    budget = _PROMPT_MAX_BYTES - len(_PROMPT_HEADER.encode()) - len(tail.encode())
    blocks = []
    for i, c in enumerate(cands, 1):
        lines = [f"[{i}] [m:{c.get('uid')}]  date={_date(c.get('ts', 0))}  "
                 f"repo={c.get('repo_key', '?')}  kind={c.get('kind', '?')}"]
        for label, field, cap in (("title", "title", 200), ("learned", "learned", 700),
                                  ("decided", "decided", 700), ("next", "next_steps", 400),
                                  ("outcome", "outcome", 700), ("files", "files_touched", 300),
                                  ("note", "cont", 300)):
            val = _flat(c.get(field) or "")
            if val and not (field == "cont" and any(
                    c.get(f) for f in ("learned", "decided", "next_steps", "outcome"))):
                lines.append(f"{label}: {_trim_bytes(val, cap)}")
        block = "\n".join(lines) + "\n\n"
        cost = len(block.encode())
        if cost > budget:
            break
        blocks.append(block)
        budget -= cost
    return _PROMPT_HEADER + "".join(blocks) + tail


def _synthesize(prompt: str, model: str, timeout: int = 25) -> str | None:
    """One bounded `claude -p` call; None on any failure (never raises).

    Both self-capture guards are set so the synthesis call can't write a
    memory about itself (the jhh3-mem paid-treadmill lesson).
    """
    env = dict(os.environ, JHH3_MEM_NO_CAPTURE="1")
    env[config.NO_CAPTURE_ENV] = "1"
    try:
        res = subprocess.run(
            ["claude", "-p", "--model", model],
            input=prompt, capture_output=True, text=True,
            timeout=timeout, env=env,
        )
    except (OSError, subprocess.TimeoutExpired):
        return None
    if res.returncode != 0 or not (res.stdout or "").strip():
        return None
    return res.stdout.strip()


# ── grep ─────────────────────────────────────────────────────────────────────

def _grep(rest: list[str]) -> int:
    pattern, all_machines, icase = None, False, False
    for a in rest:
        if a == "--all-machines":
            all_machines = True
        elif a == "-i":
            icase = True
        elif pattern is None:
            pattern = a
        else:
            raise QueryError(f"unexpected argument: {a}")
    if not pattern:
        raise QueryError("usage: hive grep <pattern> [--all-machines] [-i]")

    flags = re.IGNORECASE if icase else 0
    fixed = False
    try:
        rx = re.compile(pattern, flags)
    except re.error:
        fixed = True
        rx = re.compile(re.escape(pattern), flags)

    files = _transcript_files()
    if all_machines:
        try:
            from . import raw
            files += [str(p) for p in raw.iter_transcripts(all_machines=True)]
        except (ImportError, AttributeError):
            util.eprint("hive grep: fleet-wide transcripts unavailable "
                        "(raw module not built yet) — searching local only")
    if not files:
        print("no transcripts to search")
        return 1

    matched = _matching_files(pattern, files, icase=icase, fixed=fixed)
    if not matched:
        print(f"no matches ({len(files)} transcripts searched)")
        return 1

    for f in matched[:_GREP_SHOW_FILES]:
        p = Path(f)
        print(f"{p.stem}  ({p.parent.name})")
        for line in _matching_lines(p, rx):
            print(f"    {line}")
    extra = len(matched) - _GREP_SHOW_FILES
    if extra > 0:
        print(f"… {extra} more matching transcript(s)")
    return 0


def _transcript_files(bound: int = _GREP_FILE_BOUND) -> list[str]:
    """Local *.jsonl transcripts, newest first, mtime < 90d, count-bounded."""
    cutoff = time.time() - _GREP_MAX_AGE_DAYS * 86400
    out: list[tuple[float, str]] = []
    for root in (config.CLAUDE_PROJECTS, config.CODEX_SESSIONS):
        if not root.is_dir():
            continue
        for dirpath, _dirs, names in os.walk(root):
            for name in names:
                if not name.endswith(".jsonl"):
                    continue
                p = os.path.join(dirpath, name)
                try:
                    mt = os.stat(p).st_mtime
                except OSError:
                    continue
                if mt >= cutoff:
                    out.append((mt, p))
    out.sort(reverse=True)
    return [p for _, p in out[:bound]]


def _matching_files(pattern: str, files: list[str], icase: bool,
                    fixed: bool) -> list[str]:
    use_rg = shutil.which("rg") is not None
    matched: list[str] = []
    for i in range(0, len(files), 400):
        chunk = files[i:i + 400]
        if use_rg:
            cmd = ["rg", "-l", "--no-messages"]
            cmd += ["-i"] if icase else []
            cmd += ["-F"] if fixed else []
            cmd += ["-e", pattern, "--"] + chunk
        else:
            cmd = ["grep", "-l", "-s"]
            cmd += ["-i"] if icase else []
            cmd += ["-F"] if fixed else ["-E"]
            cmd += ["-e", pattern, "--"] + chunk
        try:
            res = util.run(cmd, timeout=30)
        except (OSError, subprocess.TimeoutExpired):
            continue
        matched += [ln for ln in (res.stdout or "").splitlines() if ln]
        if len(matched) >= _GREP_SHOW_FILES + 1:
            break
    return matched


def _matching_lines(path: Path, rx: re.Pattern, cap: int = 3) -> list[str]:
    out: list[str] = []
    try:
        with open(path, errors="ignore") as f:
            for line in f:
                if rx.search(line):
                    out.append(line.strip()[:200])
                    if len(out) >= cap:
                        break
    except OSError:
        pass
    return out


# ── why / session (commit xref) ──────────────────────────────────────────────

def _repo_key(cwd: str) -> str:
    from . import ledger
    return ledger._repo_info(cwd)[0]


def _parse_trailer_session(body: str) -> str:
    """Claude-Session trailer URL -> session id (tail of the URL)."""
    m = _TRAILER_RE.search(body or "")
    if not m:
        return ""
    return m.group(1).rstrip("/").rsplit("/", 1)[-1]


def _git_log_records(repo: str, extra: list[str],
                     cap: int = 50) -> list[tuple[str, str, str, int]] | None:
    """[(sha, subject, body, committed_at)] via NUL-separated git log.

    None means git itself failed (bad revision, not a repo) — distinct from
    an empty-but-successful log, which xref_scan must treat as "nothing new".
    """
    r = util.run(["git", "-C", repo, "log", "-z", f"--max-count={cap}",
                  "--format=%H%x09%ct%x09%s%x09%b"] + extra, timeout=30)
    if r.returncode != 0:
        return None
    out = []
    for rec in (r.stdout or "").split("\x00"):
        if not rec.strip():
            continue
        parts = rec.split("\t", 3)
        if len(parts) < 3:
            continue
        sha, ct, subject = parts[0].strip(), parts[1], parts[2]
        body = parts[3] if len(parts) > 3 else ""
        try:
            ts = int(ct)
        except ValueError:
            ts = 0
        out.append((sha, subject, body, ts))
    return out


def _print_xref_row(conn, sha: str, subject: str, session_id: str,
                    ts: int = 0, slug: str = "") -> None:
    line = f"{sha[:10]}  {_flat(subject)[:100]}"
    if ts:
        line += f"  ({_date(ts)})"
    print(line)
    detail = f"    session: {session_id or '(no Claude-Session trailer)'}"
    if not slug and session_id:
        row = conn.execute(
            "SELECT stream_slug, title FROM hive_sessions WHERE session_id=?",
            (session_id,)).fetchone()
        if row:
            slug = row["stream_slug"] or ""
            if row["title"]:
                detail += f"  title: {row['title']}"
    if slug:
        detail += f"  stream: {slug}"
    print(detail)


def _why(rest: list[str]) -> int:
    if not rest:
        raise QueryError("usage: hive why <path>[:line]")
    target = rest[0].rsplit(":", 1)[0] if re.search(r":\d+$", rest[0]) else rest[0]
    cwd = os.getcwd()
    conn = db.connect()
    db.ensure_hive_schema(conn)
    repo_key = _repo_key(cwd)

    printed = 0
    have_xref = conn.execute(
        "SELECT 1 FROM hive_commit_xref WHERE repo_key=? LIMIT 1",
        (repo_key,)).fetchone() if repo_key else None
    if have_xref:
        shas = [s for s, _, _, _ in _git_log_records(cwd, ["--", target]) or []]
        for sha in shas:
            row = conn.execute(
                "SELECT * FROM hive_commit_xref WHERE repo_key=? AND sha=?",
                (repo_key, sha)).fetchone()
            if row:
                _print_xref_row(conn, row["sha"], row["subject"],
                                row["session_id"], row["committed_at"],
                                row["stream_slug"])
                printed += 1

    if not printed:
        # Live scan fallback: parse trailers straight out of git log.
        for sha, subject, body, ts in _git_log_records(cwd, ["--", target]) or []:
            sid = _parse_trailer_session(body)
            if sid:
                _print_xref_row(conn, sha, subject, sid, ts)
                printed += 1
    conn.close()
    if not printed:
        print(f"no session-linked commits found for {target}")
        return 1
    return 0


def _session(rest: list[str]) -> int:
    if not rest:
        raise QueryError("usage: hive session <sha-or-pr>")
    key = rest[0]
    conn = db.connect()
    db.ensure_hive_schema(conn)
    pr = key.lstrip("#")
    rows = conn.execute(
        "SELECT * FROM hive_commit_xref WHERE sha LIKE ? OR (pr != '' AND pr = ?) "
        "ORDER BY committed_at DESC LIMIT 20",
        (key + "%", pr)).fetchall()
    printed = 0
    for row in rows:
        _print_xref_row(conn, row["sha"], row["subject"], row["session_id"],
                        row["committed_at"], row["stream_slug"])
        printed += 1

    if not printed:
        cwd = os.getcwd()
        if re.fullmatch(r"[0-9a-f]{7,40}", key.lower()):
            recs = _git_log_records(cwd, [key])
        else:
            recs = _git_log_records(cwd, ["--all", f"--grep={key}"], cap=20)
        for sha, subject, body, ts in recs or []:
            sid = _parse_trailer_session(body)
            _print_xref_row(conn, sha, subject, sid, ts)
            printed += 1
    conn.close()
    if not printed:
        print(f"nothing found for {key}")
        return 1
    return 0


def xref_scan(conn, repo_paths: list[Path]) -> int:
    """Janitor phase: incremental Claude-Session trailer sweep per repo.

    Bounded at 500 commits per repo per run; state in hive_xref_state keyed
    by repo path so each machine advances its own cursor.
    """
    db.ensure_hive_schema(conn)
    total = 0
    for rp in repo_paths:
        rp = Path(rp)
        if not rp.is_dir():
            continue
        repo_key = _repo_key(str(rp))
        if not repo_key:
            continue
        row = conn.execute(
            "SELECT last_sha FROM hive_xref_state WHERE repo_path=?",
            (str(rp),)).fetchone()
        last_sha = row["last_sha"] if row else ""
        extra = [f"{last_sha}..HEAD"] if last_sha else ["HEAD"]
        recs = _git_log_records(str(rp), extra, cap=_XREF_SCAN_CAP)
        if recs is None and last_sha:
            # last_sha may be gone (rebase/gc): fall back to a bounded rescan.
            recs = _git_log_records(str(rp), ["HEAD"], cap=_XREF_SCAN_CAP)
        for sha, subject, body, ts in recs or []:
            sid = _parse_trailer_session(body)
            if not sid:
                continue
            m = _PR_RE.search(subject)
            slug_row = conn.execute(
                "SELECT stream_slug FROM hive_sessions WHERE session_id=?",
                (sid,)).fetchone()
            conn.execute(
                "INSERT OR REPLACE INTO hive_commit_xref "
                "(sha, repo_key, pr, session_id, stream_slug, subject, committed_at) "
                "VALUES (?,?,?,?,?,?,?)",
                (sha, repo_key, m.group(1) if m else "", sid,
                 slug_row["stream_slug"] if slug_row else "",
                 _trim_bytes(subject, 200), ts))
            total += 1
        head = util.run(["git", "-C", str(rp), "rev-parse", "HEAD"], timeout=10)
        if head.returncode == 0 and head.stdout.strip():
            conn.execute(
                "INSERT OR REPLACE INTO hive_xref_state "
                "(repo_path, last_sha, updated_at) VALUES (?,?,?)",
                (str(rp), head.stdout.strip(), db.now()))
        conn.commit()
    return total


# ── resume-context ───────────────────────────────────────────────────────────

def _frontmatter(text: str) -> dict[str, str]:
    lines = text.splitlines()
    if not lines or lines[0].strip() != "---":
        return {}
    fm: dict[str, str] = {}
    for line in lines[1:]:
        if line.strip() == "---":
            break
        key, sep, val = line.partition(":")
        if sep:
            fm[key.strip()] = val.strip().strip('"')
    return fm


def _resume_context(rest: list[str]) -> int:
    if not rest:
        raise QueryError("usage: hive resume-context <slug>")
    slug = rest[0]
    sdir = config.STREAMS_VAULT / "streams" / slug
    md = sdir / "STREAM.md"
    try:
        fm = _frontmatter(md.read_text())
    except OSError:
        util.eprint(f"hive resume-context: no stream '{slug}' ({md})")
        return 1

    parts = [f"# resume-context: {slug}", ""]
    for key in ("title", "status", "repo", "branch", "worktree", "next", "updated"):
        if fm.get(key):
            parts.append(f"- {key}: {fm[key]}")

    try:
        vision = (sdir / "VISION.md").read_bytes()[:400].decode("utf-8", "ignore")
        if vision.strip():
            parts += ["", "## vision", vision.strip()]
    except OSError:
        pass
    try:
        raw_log = (sdir / "LOG.md").read_bytes()
        tail = raw_log[-1200:].decode("utf-8", "ignore")
        if tail.strip():
            parts += ["", "## log (tail)", tail.strip()]
    except OSError:
        pass

    # Latest distilled entries for the stream's repo (deterministic, no LLM).
    repo = fm.get("repo", "").strip()
    mem_args = ["recent", "--json", "--limit", "5"]
    if "/" in repo:                      # looks like a repo_key (owner/repo)
        mem_args += ["--repo", repo.lower()]
    try:
        res = vendored.mem(mem_args, timeout=30)
        entries = _parse_ndjson(res.stdout or "")
    except (OSError, subprocess.TimeoutExpired):
        entries = []
    if entries:
        parts += ["", "## recent memory"]
        for c in entries:
            title = _trim_bytes(_flat(c.get("title", "(untitled)")), 100)
            parts.append(f"- {_date(c.get('ts', 0))} · {c.get('kind', '?')} · "
                         f"{title} [m:{c.get('uid')}]")
        parts.append(f"drill down: hive show {entries[0].get('uid')}")

    print(_trim_bytes("\n".join(parts), _RESUME_MAX_BYTES))
    return 0


# ── wrong ────────────────────────────────────────────────────────────────────

def _wrong(rest: list[str]) -> int:
    if not rest:
        raise QueryError("usage: hive wrong <uid>")
    uid = rest[0]
    conn = db.connect()
    db.ensure_hive_schema(conn)
    conn.execute("INSERT INTO hive_feedback (uid, kind, ts) VALUES (?, 'wrong', ?)",
                 (uid, db.now()))
    n = 0
    has_entries = conn.execute(
        "SELECT 1 FROM sqlite_master WHERE type='table' AND name='entries'").fetchone()
    if has_entries:
        cur = conn.execute("UPDATE entries SET injectable=0 WHERE uid=?", (uid,))
        n = cur.rowcount
    conn.commit()
    conn.close()
    print(f"marked wrong: {uid} — demoted {n} row(s) from injection "
          "(search still finds it)")
    return 0
