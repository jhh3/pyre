"""hive.query / hive.intent tests.

No real model calls, no real DB, no real streams: vendored.mem is replaced
with a canned-NDJSON recorder, the synthesis subprocess is replaced by
patching query._synthesize, and DB verbs run against a tmp SQLite built from
the vendored schema.sql + hive's additive schema. The trailer-parsing tests
use a throwaway git repo under pytest's tmp_path.
"""

from __future__ import annotations

import json
import os
import sqlite3
import subprocess
import sys
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(REPO_ROOT / "plugins" / "hive" / "lib"))

from hive import config, db, intent, query  # noqa: E402

SCHEMA_SQL = (REPO_ROOT / "plugins" / "hive" / "vendor" / "jhh3-mem" / "db"
              / "schema.sql").read_text()

CANDS = [
    {"uid": "m1-AAA", "ts": 1756200000, "kind": "decision",
     "repo_key": "acme/webapp", "title": "Chose zram over pure swapfile",
     "learned": "zram absorbs bursts before the swapfile is touched",
     "decided": "keep 32G zstd zram in front of the 32G swapfile",
     "next_steps": "", "outcome": "configured both, PSI calm since"},
    {"uid": "m1-BBB", "ts": 1756100000, "kind": "bugfix",
     "repo_key": "jhh3/hive", "title": "Fixed WAL busy timeout in db layer",
     "learned": "busy_timeout must be per-connection",
     "decided": "", "next_steps": "watch for lock contention",
     "outcome": "no more SQLITE_BUSY"},
]


class MemRecorder:
    """Stands in for vendored.mem: records calls, returns canned NDJSON.

    `rows_seq` (a list of row-lists) serves call N its Nth element (last one
    repeated) so tests can give the two retrieval searches different results.
    """

    def __init__(self, rows=None, rc=None, rows_seq=None):
        self.rows = CANDS if rows is None else rows
        self.rows_seq = rows_seq
        self.rc = rc
        self.calls: list[list[str]] = []

    def __call__(self, args, timeout=60, capture=True):
        i = len(self.calls)
        self.calls.append(list(args))
        rows = (self.rows_seq[min(i, len(self.rows_seq) - 1)]
                if self.rows_seq is not None else self.rows)
        rc = self.rc if self.rc is not None else (0 if rows else 1)
        stdout = "\n".join(json.dumps(r) for r in rows)
        return subprocess.CompletedProcess(args, rc, stdout=stdout, stderr="")


def _fake_store(tmp_path, monkeypatch, rows):
    """Fake jhh3-mem DB for ask's single-SQL hydration; returns its path."""
    dbfile = tmp_path / "mem.db"
    conn = sqlite3.connect(dbfile)
    conn.executescript(SCHEMA_SQL)
    for r in rows:
        conn.execute(
            "INSERT INTO entries (uid, source_bytes, origin, producer, repo_key,"
            " kind, ts, title, learned, decided, next_steps, outcome,"
            " files_touched, superseded) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
            (r["uid"], r.get("source_bytes", 100), "live", "test",
             r.get("repo_key", ""), r.get("kind", "session"), r.get("ts", 0),
             r.get("title", ""), r.get("learned", ""), r.get("decided", ""),
             r.get("next_steps", ""), r.get("outcome", ""),
             r.get("files_touched", ""), r.get("superseded", 0)))
    conn.commit()
    conn.close()
    monkeypatch.setattr(query.db, "path", lambda: dbfile)
    return dbfile


@pytest.fixture
def mem(tmp_path, monkeypatch):
    rec = MemRecorder()
    monkeypatch.setattr(query.vendored, "mem", rec)
    _fake_store(tmp_path, monkeypatch, CANDS)
    return rec


def _no_synth(monkeypatch):
    def boom(prompt, model, timeout=25):  # pragma: no cover
        raise AssertionError("synthesis must not be called")
    monkeypatch.setattr(query, "_synthesize", boom)


# ── ask: end-to-end citation validation ─────────────────────────────────────

def test_ask_valid_citations(mem, monkeypatch, capsys):
    monkeypatch.setattr(
        query, "_synthesize",
        lambda prompt, model, timeout=25: "We front the swapfile with zram [m:m1-AAA].")
    assert query.run("ask", ["why zram"]) == 0
    out = capsys.readouterr().out
    assert "We front the swapfile with zram" in out
    assert "Sources:" in out
    assert "[m:m1-AAA]" in out
    assert "Chose zram over pure swapfile" in out
    assert "hive show m1-AAA" in out
    # retrieval went through the vendored ranker with Q5 defaults
    assert mem.calls[0][:2] == ["search", "why zram"]
    assert "--all-repos" in mem.calls[0]


def test_ask_unknown_citation_discards_synthesis(mem, monkeypatch, capsys):
    monkeypatch.setattr(
        query, "_synthesize",
        lambda prompt, model, timeout=25: "Because of X [m:not-a-real-uid].")
    assert query.run("ask", ["why zram"]) == 0
    out = capsys.readouterr().out
    assert "synthesis discarded" in out
    assert "Because of X" not in out          # the whole answer is dropped
    assert "Chose zram over pure swapfile" in out  # raw hits shown instead


def test_ask_uncited_reply_discards_synthesis(mem, monkeypatch, capsys):
    monkeypatch.setattr(
        query, "_synthesize",
        lambda prompt, model, timeout=25: "Because reasons, trust me.")
    assert query.run("ask", ["why zram"]) == 0
    out = capsys.readouterr().out
    assert "synthesis discarded" in out
    assert "trust me" not in out


def test_ask_not_in_memory_abstention(mem, monkeypatch, capsys):
    monkeypatch.setattr(
        query, "_synthesize", lambda prompt, model, timeout=25: "NOT_IN_MEMORY")
    assert query.run("ask", ["something unrelated"]) == 1
    out = capsys.readouterr().out
    assert "not in memory" in out
    assert "m1-AAA" in out                    # top hits still listed


def test_ask_model_failure_falls_back_to_hits(mem, monkeypatch, capsys):
    monkeypatch.setattr(query, "_synthesize", lambda prompt, model, timeout=25: None)
    assert query.run("ask", ["why zram"]) == 0
    out = capsys.readouterr().out
    assert "synthesis unavailable" in out
    assert "m1-AAA" in out and "m1-BBB" in out


def test_ask_empty_retrieval(monkeypatch, capsys):
    monkeypatch.setattr(query.vendored, "mem", MemRecorder(rows=[]))
    _no_synth(monkeypatch)
    assert query.run("ask", ["nothing at all"]) == 1
    assert "not in memory (0 hits)" in capsys.readouterr().out


def test_ask_raw_skips_synthesis(mem, monkeypatch, capsys):
    _no_synth(monkeypatch)
    assert query.run("ask", ["why zram", "--raw"]) == 0
    out = capsys.readouterr().out
    assert "m1-AAA" in out and "m1-BBB" in out


def test_ask_repo_and_kind_flags_forwarded(mem, monkeypatch):
    monkeypatch.setattr(
        query, "_synthesize", lambda prompt, model, timeout=25: "NOT_IN_MEMORY")
    query.run("ask", ["q", "--repo", "jhh3/hive", "--kind", "decision",
                      "--limit", "7"])
    call = mem.calls[0]
    assert ["--repo", "jhh3/hive"] == call[call.index("--repo"):call.index("--repo") + 2]
    assert "decision" in call and "7" in call


def test_build_prompt_bounded_and_cited(mem):
    cands = [dict(CANDS[0], uid=f"m1-{i:03d}", learned="x" * 5000)
             for i in range(50)]
    prompt = query._build_prompt("the question", cands)
    assert len(prompt.encode()) <= query._PROMPT_MAX_BYTES
    assert "the question" in prompt
    assert "[m:m1-000]" in prompt             # citable tag heads each entry
    assert "NOT_IN_MEMORY" in prompt          # abstention instruction present
    assert "Not in memory:" in prompt         # partial-answer instruction present


# ── ask: partial-answer contract ────────────────────────────────────────────

def test_ask_partial_answer_is_success(mem, monkeypatch, capsys):
    reply = ("zram fronts the swapfile [m:m1-AAA].\n"
             "Not in memory: whether billing was affected")
    monkeypatch.setattr(query, "_synthesize", lambda p, m, timeout=25: reply)
    assert query.run("ask", ["zram and billing"]) == 0
    out = capsys.readouterr().out
    assert "zram fronts the swapfile" in out
    assert "Not in memory: whether billing was affected" in out
    assert "Sources:" in out
    assert "useful? pin it: hive pin m1-AAA" in out


def test_ask_abstention_requires_whole_reply(mem, monkeypatch, capsys):
    # NOT_IN_MEMORY *inside* a cited answer must not trigger the abstention path.
    reply = "search prints NOT_IN_MEMORY when the store is empty [m:m1-BBB]."
    monkeypatch.setattr(query, "_synthesize", lambda p, m, timeout=25: reply)
    assert query.run("ask", ["when is NOT_IN_MEMORY printed"]) == 0
    out = capsys.readouterr().out
    assert "Sources:" in out and "[m:m1-BBB]" in out


def test_ask_abstention_whole_reply_with_whitespace(mem, monkeypatch, capsys):
    monkeypatch.setattr(query, "_synthesize",
                        lambda p, m, timeout=25: "  NOT_IN_MEMORY\n")
    assert query.run("ask", ["something unrelated"]) == 1
    assert "not in memory" in capsys.readouterr().out


def test_ask_mixed_abstention_keeps_cited_prose(mem, monkeypatch, capsys):
    # A contract-violating "NOT_IN_MEMORY" line glued to a cited answer:
    # the bare line is dropped, the cited prose survives as a success.
    reply = "NOT_IN_MEMORY\n\nNo decision recorded, only fixes [m:m1-BBB]."
    monkeypatch.setattr(query, "_synthesize", lambda p, m, timeout=25: reply)
    assert query.run("ask", ["billing decision"]) == 0
    out = capsys.readouterr().out
    assert "NOT_IN_MEMORY" not in out
    assert "only fixes" in out and "Sources:" in out


def test_ask_partial_answer_bad_cite_still_discarded(mem, monkeypatch, capsys):
    reply = "partial claim [m:bogus-uid].\nNot in memory: the rest"
    monkeypatch.setattr(query, "_synthesize", lambda p, m, timeout=25: reply)
    assert query.run("ask", ["q about zram"]) == 0
    out = capsys.readouterr().out
    assert "synthesis discarded" in out
    assert "partial claim" not in out


# ── ask: single-SQL hydration ───────────────────────────────────────────────

def test_hydrate_single_sql_fills_fields(tmp_path, monkeypatch):
    _fake_store(tmp_path, monkeypatch, CANDS)
    out = query._hydrate([{"uid": "m1-AAA", "cont": "thin"}])
    assert out[0]["learned"].startswith("zram absorbs")
    assert out[0]["repo_key"] == "acme/webapp"
    assert out[0]["cont"] == "thin"           # search-row extras survive


def test_hydrate_picks_max_source_bytes_non_superseded(tmp_path, monkeypatch):
    rows = [dict(CANDS[0], uid="m9", learned="older", source_bytes=5),
            dict(CANDS[0], uid="m9", learned="current", source_bytes=20),
            dict(CANDS[0], uid="m9", learned="poison", source_bytes=30,
                 superseded=1)]
    _fake_store(tmp_path, monkeypatch, rows)
    out = query._hydrate([{"uid": "m9"}])
    assert out[0]["learned"] == "current"


def test_hydrate_missing_db_returns_cands_unchanged(tmp_path, monkeypatch):
    monkeypatch.setattr(query.db, "path", lambda: tmp_path / "absent.db")
    cands = [{"uid": "x", "title": "t"}]
    assert query._hydrate(cands) == cands


def test_ask_prompt_carries_hydrated_fields(mem, monkeypatch):
    # Search returns thin rows; the prompt must carry the DB's full fields.
    mem.rows = [{"uid": "m1-AAA", "title": "Chose zram over pure swapfile"}]
    prompts = []

    def grab(prompt, model, timeout=25):
        prompts.append(prompt)
        return "NOT_IN_MEMORY"

    monkeypatch.setattr(query, "_synthesize", grab)
    query.run("ask", ["why zram"])
    assert "zram absorbs bursts" in prompts[0]           # learned, from the DB
    assert "PSI calm since" in prompts[0]                # outcome, from the DB


# ── ask: retrieval breadth (second search + merge) ──────────────────────────

def test_keyword_query_strips_stopwords_keeps_identifiers():
    q = query._keyword_query(
        "what was decided about the test_executor billing blocked tests?")
    toks = q.split()
    assert "test_executor" in toks and "billing" in toks
    assert "what" not in toks and "the" not in toks and "was" not in toks
    assert len(toks) <= 8


def test_ask_merges_second_search(tmp_path, monkeypatch, capsys):
    extra = {"uid": "m1-CCC", "ts": 1756000000, "kind": "note",
             "repo_key": "jhh3/hive", "title": "Executor billing decision"}
    rec = MemRecorder(rows_seq=[CANDS, [CANDS[0], extra]])
    monkeypatch.setattr(query.vendored, "mem", rec)
    _fake_store(tmp_path, monkeypatch, CANDS + [extra])
    _no_synth(monkeypatch)
    assert query.run(
        "ask", ["what was decided about executor_billing tests", "--raw"]) == 0
    out = capsys.readouterr().out
    assert "m1-CCC" in out                    # second search contributed
    assert out.count("· m1-AAA") == 1         # merged by uid, best rank kept
    assert len(rec.calls) == 2
    q2 = rec.calls[1][1]                      # second search is keyword-stripped
    assert "what" not in q2.split() and "executor_billing" in q2


def test_ask_no_second_search_when_enough_hits(tmp_path, monkeypatch):
    many = [dict(CANDS[0], uid=f"m2-{i}") for i in range(8)]
    rec = MemRecorder(rows=many)
    monkeypatch.setattr(query.vendored, "mem", rec)
    _fake_store(tmp_path, monkeypatch, many)
    _no_synth(monkeypatch)
    assert query.run("ask", ["what was decided about billing", "--raw"]) == 0
    assert len(rec.calls) == 1


# ── ask: --explain ──────────────────────────────────────────────────────────

def test_ask_explain_line(mem, monkeypatch, capsys, tmp_path):
    monkeypatch.setattr(query, "_synthesize",
                        lambda p, m, timeout=25: "zram [m:m1-AAA].")
    monkeypatch.setenv("JHH3_MEM_HOME", str(tmp_path / "jm"))  # no vec0.so
    monkeypatch.delenv("OPENAI_API_KEY", raising=False)
    assert query.run("ask", ["why zram", "--explain"]) == 0
    err = capsys.readouterr().err
    assert ("retrieval: 2 hits (search1=2, search2=2 merged), "
            "semantic lane: inactive") in err


def test_semantic_lane_active_detection(monkeypatch, tmp_path):
    jm = tmp_path / "jm"
    (jm / "lib").mkdir(parents=True)
    (jm / "lib" / "vec0.so").write_bytes(b"")
    monkeypatch.setenv("JHH3_MEM_HOME", str(jm))
    monkeypatch.setenv("OPENAI_API_KEY", "sk-test")
    assert query._semantic_active() is True
    monkeypatch.delenv("OPENAI_API_KEY")
    assert query._semantic_active() is False


# ── tmp DB: wrong / pin ─────────────────────────────────────────────────────

@pytest.fixture
def tmp_db(tmp_path, monkeypatch):
    dbfile = tmp_path / "index.db"
    monkeypatch.delenv("HIVE_DB", raising=False)
    monkeypatch.setattr(config, "DB_PATH", dbfile)
    monkeypatch.setattr(db, "JHH3_MEM_DB", tmp_path / "absent.db")
    seed = sqlite3.connect(dbfile)
    seed.executescript(SCHEMA_SQL)
    seed.commit()
    seed.close()
    conn = db.connect(dbfile)
    db.ensure_hive_schema(conn)
    yield conn
    conn.close()


def _insert_entry(conn, uid, durability=1.0):
    conn.execute(
        "INSERT INTO entries (uid, origin, producer, repo_key, kind, ts, title,"
        " durability) VALUES (?, 'noted', 'test', 'jhh3/hive', 'note', 1756200000,"
        " 'a test entry', ?)", (uid, durability))
    conn.commit()


def test_wrong_demotes_from_injection_only(tmp_db, capsys):
    _insert_entry(tmp_db, "u-wrong")
    assert query.run("wrong", ["u-wrong"]) == 0
    row = tmp_db.execute(
        "SELECT injectable FROM entries WHERE uid='u-wrong'").fetchone()
    assert row["injectable"] == 0
    fb = tmp_db.execute(
        "SELECT kind FROM hive_feedback WHERE uid='u-wrong'").fetchone()
    assert fb["kind"] == "wrong"
    assert "demoted 1 row" in capsys.readouterr().out


def test_pin_floors_durability_and_reinjects(tmp_db, capsys):
    _insert_entry(tmp_db, "u-pin", durability=0.85)
    tmp_db.execute("UPDATE entries SET injectable=0 WHERE uid='u-pin'")
    tmp_db.commit()
    assert intent.run("pin", ["u-pin"]) == 0
    row = tmp_db.execute(
        "SELECT durability, injectable FROM entries WHERE uid='u-pin'").fetchone()
    assert row["durability"] == pytest.approx(1.5)
    assert row["injectable"] == 1
    fb = tmp_db.execute(
        "SELECT kind FROM hive_feedback WHERE uid='u-pin'").fetchone()
    assert fb["kind"] == "pin"


def test_pin_does_not_lower_high_durability(tmp_db):
    _insert_entry(tmp_db, "u-hi", durability=1.8)
    assert intent.run("pin", ["u-hi"]) == 0
    row = tmp_db.execute(
        "SELECT durability FROM entries WHERE uid='u-hi'").fetchone()
    assert row["durability"] == pytest.approx(1.8)


# ── done / park fall back to vendored ws ────────────────────────────────────

def test_done_falls_back_to_ws_update(monkeypatch, capsys):
    calls = []

    def fake_ws(args, timeout=60, capture=True):
        calls.append(list(args))
        return subprocess.CompletedProcess(args, 0, "", "")

    monkeypatch.setattr(intent.vendored, "ws", fake_ws)
    assert intent.run("done", ["my-stream"]) == 0
    assert ["update", "my-stream", "--status", "done"] in calls
    assert intent.run("park", ["my-stream"]) == 0
    assert ["update", "my-stream", "--status", "parked"] in calls


# ── trailer parsing: why / xref_scan ────────────────────────────────────────

TRAILER_URL = "https://claude.ai/code/session_ABC123"


@pytest.fixture
def git_repo(tmp_path):
    repo = tmp_path / "fixture-repo"
    repo.mkdir()
    env = dict(os.environ,
               GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@example.com",
               GIT_COMMITTER_NAME="t", GIT_COMMITTER_EMAIL="t@example.com")

    def g(*args):
        subprocess.run(["git", "-C", str(repo), *args], check=True,
                       capture_output=True, env=env)

    subprocess.run(["git", "init", "-q", str(repo)], check=True,
                   capture_output=True, env=env)
    (repo / "f.txt").write_text("hello\n")
    g("add", "f.txt")
    g("commit", "-q", "-m",
      f"add thing (#42)\n\nSome body text.\n\nClaude-Session: {TRAILER_URL}")
    (repo / "other.txt").write_text("no trailer\n")
    g("add", "other.txt")
    g("commit", "-q", "-m", "plain commit without trailer")
    return repo, g


def test_parse_trailer_session():
    assert query._parse_trailer_session(
        f"body\n\nClaude-Session: {TRAILER_URL}") == "session_ABC123"
    assert query._parse_trailer_session("no trailer here") == ""


def test_why_live_scan_fallback(git_repo, tmp_db, monkeypatch, capsys):
    repo, _ = git_repo
    monkeypatch.chdir(repo)
    assert query.run("why", ["f.txt"]) == 0
    out = capsys.readouterr().out
    assert "session_ABC123" in out
    assert "add thing" in out


def test_why_path_with_line_suffix(git_repo, tmp_db, monkeypatch, capsys):
    repo, _ = git_repo
    monkeypatch.chdir(repo)
    assert query.run("why", ["f.txt:12"]) == 0
    assert "session_ABC123" in capsys.readouterr().out


def test_why_no_trailers(git_repo, tmp_db, monkeypatch, capsys):
    repo, _ = git_repo
    monkeypatch.chdir(repo)
    assert query.run("why", ["other.txt"]) == 1
    assert "no session-linked commits" in capsys.readouterr().out


def test_xref_scan_upserts_and_is_incremental(git_repo, tmp_db):
    repo, g = git_repo
    n = query.xref_scan(tmp_db, [repo])
    assert n == 1
    row = tmp_db.execute("SELECT * FROM hive_commit_xref").fetchone()
    assert row["session_id"] == "session_ABC123"
    assert row["pr"] == "42"
    assert row["subject"].startswith("add thing")
    state = tmp_db.execute("SELECT * FROM hive_xref_state").fetchone()
    assert state["last_sha"]

    # nothing new -> no rows added
    assert query.xref_scan(tmp_db, [repo]) == 0

    # a new trailer commit is picked up from the cursor
    (repo / "g.txt").write_text("more\n")
    g("add", "g.txt")
    g("commit", "-q", "-m",
      "second thing\n\nClaude-Session: https://claude.ai/code/session_DEF456")
    assert query.xref_scan(tmp_db, [repo]) == 1
    ids = {r["session_id"] for r in
           tmp_db.execute("SELECT session_id FROM hive_commit_xref")}
    assert ids == {"session_ABC123", "session_DEF456"}


def test_session_lookup_from_xref(git_repo, tmp_db, monkeypatch, capsys):
    repo, _ = git_repo
    monkeypatch.chdir(repo)
    query.xref_scan(tmp_db, [repo])
    assert query.run("session", ["#42"]) == 0
    assert "session_ABC123" in capsys.readouterr().out


# ── resume-context ──────────────────────────────────────────────────────────

def test_resume_context_missing_stream(tmp_path, monkeypatch, capsys):
    monkeypatch.setattr(config, "STREAMS_VAULT", tmp_path / "vault")
    assert query.run("resume-context", ["nope"]) == 1
    assert "no stream" in capsys.readouterr().err


def test_resume_context_assembly(tmp_path, monkeypatch, capsys):
    vault = tmp_path / "vault"
    sdir = vault / "streams" / "my-stream"
    sdir.mkdir(parents=True)
    (sdir / "STREAM.md").write_text(
        "---\ntitle: my-stream\nstatus: active\nrepo: Acme/webapp\n"
        "branch: alice/eng-1\nnext: \"finish the thing\"\n---\nbody\n")
    (sdir / "VISION.md").write_text("The vision is simple.\n")
    (sdir / "LOG.md").write_text("## log\nolder\nnewest entry\n")
    monkeypatch.setattr(config, "STREAMS_VAULT", vault)
    rec = MemRecorder(rows=[CANDS[0]])
    monkeypatch.setattr(query.vendored, "mem", rec)

    assert query.run("resume-context", ["my-stream"]) == 0
    out = capsys.readouterr().out
    assert out.startswith("# resume-context: my-stream")
    assert "finish the thing" in out
    assert "The vision is simple." in out
    assert "newest entry" in out
    assert "[m:m1-AAA]" in out
    assert len(out.encode()) <= query._RESUME_MAX_BYTES + 1  # + newline
    # repo looked like a repo_key -> scoped, lowercased
    assert ["recent", "--json", "--limit", "5", "--repo", "acme/webapp"] \
        in rec.calls
